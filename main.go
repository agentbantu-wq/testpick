// Command testpick proposes exactly which test suites a change should run in CI.
//
// Given the files a change touches, testpick finds the test files that can
// actually be affected — by naming convention, by same-package tests, and by
// walking the reverse import graph — and prints the test commands to run,
// a GitHub Actions matrix, or a JSON plan. New test files get a suggested
// rerun count so flaky tests get shaken out before they block a merge.
//
// It is deliberately deterministic and local-only: no LLM, no network, no
// test execution. It answers "which tests?" so CI can stay fast without
// guessing by file path alone.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type language string

const (
	langGo     language = "go"
	langPython language = "python"
	langJS     language = "js"
)

// sourceFile is one code file in the repo.
type sourceFile struct {
	Path     string // repo-relative, slash-separated
	Lang     language
	IsTest   bool
	Package  string   // Go package name (go only)
	Imports  []string // raw import strings found in the file
	Lines    int
}

// repo is the discovered, indexed state of a working tree.
type repo struct {
	Root       string
	Files      map[string]*sourceFile // path -> file
	TestFiles  []*sourceFile
	DirPackage map[string]string // dir -> Go package name
	// reverse import graph: target file -> files that import it (directly)
	Reverse map[string][]*sourceFile
	Ignore  []string
	Config  *config
}

type config struct {
	Ignore   []string          `json:"ignore"`
	Commands map[string]string `json:"commands"` // lang -> template with {files} / {packages}
}

// selection is one test file chosen for the plan, with its reasons.
type selection struct {
	Test    *sourceFile
	Reasons []string `json:"reasons"`
	Depth   int      `json:"depth"` // import-graph distance from a changed file (0 = direct)
	Runs    int      `json:"suggested_runs"`
}

// plan is the full answer for a change set.
type plan struct {
	Changed            []string    `json:"changed_files"`
	Added              []string    `json:"added_files"`
	Selected           []selection `json:"selected_tests"`
	Unmapped           []string    `json:"unmapped_files"`
	OtherChanged       []string    `json:"other_changed_files"`
	FullSuite          bool        `json:"full_suite_recommended"`
	FullSuiteReason    string      `json:"full_suite_reason,omitempty"`
	Commands           []langCmd   `json:"commands"`
}

type langCmd struct {
	Language language `json:"language"`
	Command  string   `json:"command"`
}

// planJSON is the wire shape for --json (selection.Test inlined as a path).
type planJSON struct {
	Changed         []string      `json:"changed_files"`
	Added           []string      `json:"added_files"`
	Selected        []selJSON     `json:"selected_tests"`
	Unmapped        []string      `json:"unmapped_files"`
	OtherChanged    []string      `json:"other_changed_files"`
	FullSuite       bool          `json:"full_suite_recommended"`
	FullSuiteReason string        `json:"full_suite_reason,omitempty"`
	Commands        []langCmd     `json:"commands"`
}

type selJSON struct {
	Path    string   `json:"path"`
	Language language `json:"language"`
	Reasons []string `json:"reasons"`
	Depth   int      `json:"depth"`
	Runs    int      `json:"suggested_runs"`
}

func (p *plan) toJSON() planJSON {
	out := planJSON{
		Changed: p.Changed, Added: p.Added, Unmapped: p.Unmapped,
		OtherChanged: p.OtherChanged, FullSuite: p.FullSuite,
		FullSuiteReason: p.FullSuiteReason, Commands: p.Commands,
	}
	for _, s := range p.Selected {
		out.Selected = append(out.Selected, selJSON{
			Path: s.Test.Path, Language: s.Test.Lang,
			Reasons: s.Reasons, Depth: s.Depth, Runs: s.Runs,
		})
	}
	if out.Changed == nil {
		out.Changed = []string{}
	}
	if out.Added == nil {
		out.Added = []string{}
	}
	if out.Selected == nil {
		out.Selected = []selJSON{}
	}
	if out.Unmapped == nil {
		out.Unmapped = []string{}
	}
	if out.OtherChanged == nil {
		out.OtherChanged = []string{}
	}
	if out.Commands == nil {
		out.Commands = []langCmd{}
	}
	return out
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

var skipDirs = map[string]bool{
	".git": true, "vendor": true, "node_modules": true,
	"__pycache__": true, ".venv": true, "venv": true,
	".terraform": true, "target": true, "dist": true, "build": true,
}

var goExt = map[string]bool{".go": true}
var pyExt = map[string]bool{".py": true}
var jsExt = map[string]bool{".js": true, ".jsx": true, ".ts": true, ".tsx": true}

// infraFiles are changed files that can affect every test: they force a
// full-suite recommendation instead of a selective plan.
var infraFiles = map[string]bool{
	"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true,
	"yarn.lock": true, "pnpm-lock.yaml": true, "requirements.txt": true,
	"pyproject.toml": true, "Pipfile": true, "Pipfile.lock": true,
	"Dockerfile": true, "docker-compose.yml": true, "docker-compose.yaml": true,
	".testpick.json": true,
}

func isInfraFile(path string) bool {
	base := filepath.Base(path)
	if infraFiles[base] {
		return true
	}
	if strings.HasPrefix(path, ".github/") || strings.HasPrefix(path, ".circleci/") ||
		strings.HasPrefix(path, ".gitlab-ci") || base == "Makefile" ||
		strings.HasSuffix(path, ".github/workflows") {
		return true
	}
	return false
}

// classifyTest reports whether a path is a test file and its language.
func classifyTest(path string) (bool, language) {
	ext := strings.ToLower(filepath.Ext(path))
	base := filepath.Base(path)
	switch {
	case goExt[ext]:
		return strings.HasSuffix(base, "_test.go"), langGo
	case pyExt[ext]:
		if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") {
			return true, langPython
		}
		dir := filepath.Dir(path)
		if filepath.Base(dir) == "tests" || filepath.Base(dir) == "test" {
			return true, langPython
		}
		return false, langPython
	case jsExt[ext]:
		if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
			return true, langJS
		}
		if filepath.Base(filepath.Dir(path)) == "__tests__" {
			return true, langJS
		}
		return false, langJS
	}
	return false, ""
}

func langOf(path string) (language, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case goExt[ext]:
		return langGo, true
	case pyExt[ext]:
		return langPython, true
	case jsExt[ext]:
		return langJS, true
	}
	return "", false
}

// discover walks root and indexes every code file.
func discover(root string, ignore []string) (*repo, error) {
	r := &repo{
		Root:       root,
		Files:      map[string]*sourceFile{},
		DirPackage: map[string]string{},
		Reverse:    map[string][]*sourceFile{},
		Ignore:     ignore,
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return nil, err
	}
	r.Config = cfg
	r.Ignore = append(append([]string{}, ignore...), cfg.Ignore...)

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := langOf(rel); !ok {
			return nil
		}
		if matchIgnore(rel, r.Ignore) {
			return nil
		}
		sf, err := parseFile(root, rel)
		if err != nil {
			// A file we cannot parse still participates by name; imports
			// just won't be known.
			sf = &sourceFile{Path: rel}
			if l, ok := langOf(rel); ok {
				sf.Lang = l
			}
			sf.IsTest, _ = classifyTest(rel)
		}
		r.Files[rel] = sf
		if sf.IsTest {
			r.TestFiles = append(r.TestFiles, sf)
		}
		if sf.Lang == langGo && sf.Package != "" {
			if _, ok := r.DirPackage[filepath.Dir(rel)]; !ok {
				r.DirPackage[filepath.Dir(rel)] = sf.Package
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.buildReverse()
	return r, nil
}

// parseFile reads one file and extracts its language, test status, package
// name (Go) and imports.
func parseFile(root, rel string) (*sourceFile, error) {
	lang, ok := langOf(rel)
	if !ok {
		return nil, fmt.Errorf("not a code file: %s", rel)
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	sf := &sourceFile{Path: rel, Lang: lang, Lines: strings.Count(string(data), "\n") + 1}
	sf.IsTest, _ = classifyTest(rel)
	text := string(data)
	switch lang {
	case langGo:
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, full, data, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		sf.Package = f.Name.Name
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err == nil {
				sf.Imports = append(sf.Imports, p)
			}
		}
	case langPython:
		sf.Imports = parsePythonImports(text)
	case langJS:
		sf.Imports = parseJSImports(text)
	}
	return sf, nil
}

var (
	pyFromImport = regexp.MustCompile(`(?m)^\s*from\s+([.\w]+)\s+import\b`)
	pyImport     = regexp.MustCompile(`(?m)^\s*import\s+([\w., ]+)`)
	jsFromImport = regexp.MustCompile(`from\s+['"]([^'"]+)['"]`)
	jsRequire    = regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\)`)
)

func parsePythonImports(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range pyFromImport.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	for _, m := range pyImport.FindAllStringSubmatch(text, -1) {
		for _, part := range strings.Split(m[1], ",") {
			part = strings.TrimSpace(part)
			if fields := strings.Fields(part); len(fields) > 0 {
				part = fields[0]
			}
			part = strings.TrimSuffix(part, ".*")
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	return out
}

func parseJSImports(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{jsFromImport, jsRequire} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

// buildReverse resolves every code file's imports to repo files and records
// the reverse edges (target -> files that import it). Test files and
// non-test files alike are indexed, so selection can walk transitively:
// a changed file -> the modules that import it -> the tests that import
// those modules.
func (r *repo) buildReverse() {
	for _, f := range r.Files {
		for _, imp := range f.Imports {
			for _, target := range r.resolveImport(f, imp) {
				r.Reverse[target] = append(r.Reverse[target], f)
			}
		}
	}
}

// resolveImport maps one import string from importing file `from` to the
// repo-relative paths it refers to (usually one).
func (r *repo) resolveImport(from *sourceFile, imp string) []string {
	switch from.Lang {
	case langGo:
		return r.resolveGoImport(imp)
	case langPython:
		return r.resolvePythonImport(from, imp)
	case langJS:
		return r.resolveJSImport(from, imp)
	}
	return nil
}

func (r *repo) resolveGoImport(imp string) []string {
	// Prefer a repo-relative dir that is a suffix of the import path:
	// "github.com/org/repo/pkg/auth" -> "pkg/auth".
	parts := strings.Split(imp, "/")
	var dir string
	for i := 0; i < len(parts); i++ {
		cand := strings.Join(parts[i:], "/")
		if _, ok := r.DirPackage[cand]; ok {
			dir = cand
			break
		}
	}
	if dir == "" {
		// Fall back to matching the last path element against dir base names.
		last := parts[len(parts)-1]
		var matches []string
		for d := range r.DirPackage {
			if filepath.Base(d) == last {
				matches = append(matches, d)
			}
		}
		if len(matches) == 1 {
			dir = matches[0]
		}
	}
	if dir == "" {
		return nil
	}
	// A Go import names a package (a directory), not a file. Attribute
	// the edge to every non-test source file in that package: changing
	// any of them can affect importers of the package.
	var out []string
	for path, f := range r.Files {
		if f.Lang == langGo && !f.IsTest && filepath.Dir(path) == dir {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func (r *repo) resolvePythonImport(from *sourceFile, imp string) []string {
	// Relative imports: leading dots walk up from the importing file's dir.
	dots := 0
	for dots < len(imp) && imp[dots] == '.' {
		dots++
	}
	rest := imp[dots:]
	baseDir := filepath.Dir(from.Path)
	for i := 1; i < dots; i++ {
		baseDir = filepath.Dir(baseDir)
	}
	modPath := strings.ReplaceAll(rest, ".", "/")
	var cands []string
	if modPath == "" {
		cands = []string{baseDir + "/__init__.py"}
	} else if dots > 0 {
		cands = []string{filepath.Join(baseDir, modPath+".py"), filepath.Join(baseDir, modPath, "__init__.py")}
	} else {
		cands = []string{modPath + ".py", modPath + "/__init__.py"}
	}
	var out []string
	for _, c := range cands {
		c = filepath.ToSlash(c)
		if _, ok := r.Files[c]; ok {
			out = append(out, c)
		}
	}
	return out
}

var jsExts = []string{".js", ".jsx", ".ts", ".tsx"}

func (r *repo) resolveJSImport(from *sourceFile, imp string) []string {
	if !strings.HasPrefix(imp, ".") {
		return nil // bare package imports are outside the repo
	}
	base := filepath.Join(filepath.Dir(from.Path), filepath.FromSlash(imp))
	base = filepath.ToSlash(base)
	var out []string
	for _, cand := range append([]string{base}, func() []string {
		var withExt []string
		for _, e := range jsExts {
			withExt = append(withExt, base+e, base+"/index"+e)
		}
		return withExt
	}()...) {
		if f, ok := r.Files[cand]; ok && !f.IsTest {
			out = append(out, cand)
		}
	}
	return out
}

// namingCounterparts returns test files that cover src by file-name
// convention (foo.go -> foo_test.go, foo.py -> test_foo.py, ...).
func (r *repo) namingCounterparts(src string) []*sourceFile {
	lang, ok := langOf(src)
	if !ok {
		return nil
	}
	dir := filepath.Dir(src)
	stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	var cands []string
	switch lang {
	case langGo:
		cands = []string{filepath.Join(dir, stem+"_test.go")}
	case langPython:
		cands = []string{
			filepath.Join(dir, "test_"+stem+".py"),
			filepath.Join(dir, stem+"_test.py"),
			filepath.Join(dir, "tests", "test_"+stem+".py"),
			filepath.Join("tests", "test_"+stem+".py"),
		}
	case langJS:
		ext := filepath.Ext(src)
		cands = []string{
			filepath.Join(dir, stem+".test"+ext),
			filepath.Join(dir, stem+".spec"+ext),
			filepath.Join(dir, "__tests__", stem+".test"+ext),
		}
	}
	var out []*sourceFile
	for _, c := range cands {
		c = filepath.ToSlash(c)
		if f, ok := r.Files[c]; ok && f.IsTest {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

// suggestRuns estimates how many times a new test file should be run to
// shake out flakiness. It is a heuristic, not a measurement: small new
// suites get a few runs, large ones get more. Existing tests run once.
func suggestRuns(lines int, isNew bool) int {
	if !isNew {
		return 1
	}
	switch {
	case lines < 50:
		return 3
	case lines <= 200:
		return 5
	default:
		return 10
	}
}

// selectTests builds the plan for a change set.
func (r *repo) selectTests(changed, added []string, maxDepth int) *plan {
	addedSet := map[string]bool{}
	for _, a := range added {
		addedSet[a] = true
	}
	p := &plan{Changed: changed, Added: added}
	chosen := map[string]*selection{}
	add := func(tf *sourceFile, reason string, depth int) {
		s, ok := chosen[tf.Path]
		if !ok {
			s = &selection{Test: tf, Depth: depth, Runs: suggestRuns(tf.Lines, addedSet[tf.Path])}
			chosen[tf.Path] = s
		}
		for _, r := range s.Reasons {
			if r == reason {
				return
			}
		}
		s.Reasons = append(s.Reasons, reason)
		if depth < s.Depth {
			s.Depth = depth
		}
	}

	// BFS over the reverse import graph, seeded by changed code files.
	// Each queue item remembers which changed file seeded its chain, so a
	// test selected transitively still marks that changed file covered.
	type queueItem struct {
		file   string
		depth  int
		origin string
	}
	var queue []queueItem
	seen := map[string]bool{}
	covered := map[string]bool{}
	for _, c := range changed {
		f, ok := r.Files[c]
		if !ok {
			continue
		}
		if f.IsTest {
			add(f, "changed test file", 0)
			continue
		}
		// Naming convention + same-package tests are depth 1.
		for _, tf := range r.namingCounterparts(c) {
			add(tf, "naming: "+c, 1)
			covered[c] = true
		}
		if f.Lang == langGo {
			dir := filepath.Dir(c)
			for _, tf := range r.TestFiles {
				if tf.Lang == langGo && filepath.Dir(tf.Path) == dir {
					add(tf, "same package: "+dir, 1)
					covered[c] = true
				}
			}
		}
		if !seen[c] {
			seen[c] = true
			queue = append(queue, queueItem{c, 1, c})
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if it.depth > maxDepth {
			continue
		}
		for _, importer := range r.Reverse[it.file] {
			if importer.IsTest {
				add(importer, fmt.Sprintf("imports %s (depth %d)", it.file, it.depth), it.depth)
				covered[it.origin] = true
			}
			// Walk one more level: modules that import the changed file
			// may themselves be imported by other test files.
			if it.depth < maxDepth && !seen[importer.Path] {
				seen[importer.Path] = true
				queue = append(queue, queueItem{importer.Path, it.depth + 1, it.origin})
			}
		}
	}

	for _, c := range changed {
		if isInfraFile(c) {
			p.FullSuite = true
			p.FullSuiteReason = "infra file changed: " + c
			continue
		}
		f, ok := r.Files[c]
		if !ok {
			p.OtherChanged = append(p.OtherChanged, c)
			continue
		}
		if f.IsTest {
			continue
		}
		// Code file with no selected test: unmapped.
		if !covered[c] {
			p.Unmapped = append(p.Unmapped, c)
		}
	}

	for _, s := range chosen {
		sort.Strings(s.Reasons)
		p.Selected = append(p.Selected, *s)
	}
	sort.Slice(p.Selected, func(i, j int) bool { return p.Selected[i].Test.Path < p.Selected[j].Test.Path })
	sort.Strings(p.Unmapped)
	sort.Strings(p.OtherChanged)
	p.Commands = buildCommands(r, p)
	return p
}

// buildCommands renders one runnable command per language that has selected
// tests (or the full-suite command when infra changed).
func buildCommands(r *repo, p *plan) []langCmd {
	byLang := map[language][]string{}
	for _, s := range p.Selected {
		byLang[s.Test.Lang] = append(byLang[s.Test.Lang], s.Test.Path)
	}
	var out []langCmd
	langs := []language{langGo, langPython, langJS}
	for _, l := range langs {
		files, ok := byLang[l]
		if !ok {
			continue
		}
		out = append(out, langCmd{Language: l, Command: renderCommand(r, l, files, p.FullSuite)})
	}
	return out
}

func renderCommand(r *repo, l language, files []string, full bool) string {
	if r.Config != nil {
		if tpl, ok := r.Config.Commands[string(l)]; ok && tpl != "" {
			return fillTemplate(tpl, files, l)
		}
	}
	switch l {
	case langGo:
		if full {
			return "go test ./..."
		}
		dirs := map[string]bool{}
		for _, f := range files {
			dirs["./"+filepath.Dir(f)] = true
		}
		var ds []string
		for d := range dirs {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		return "go test " + strings.Join(ds, " ")
	case langPython:
		if full {
			return "pytest"
		}
		return "pytest " + strings.Join(quoteAll(files), " ")
	case langJS:
		if full {
			return "npm test"
		}
		return "npx jest " + strings.Join(quoteAll(files), " ")
	}
	return ""
}

// fillTemplate substitutes {files} (space-separated paths) and {packages}
// (space-separated Go package dirs) in a configured command template.
func fillTemplate(tpl string, files []string, l language) string {
	out := strings.ReplaceAll(tpl, "{files}", strings.Join(quoteAll(files), " "))
	if strings.Contains(out, "{packages}") {
		dirs := map[string]bool{}
		for _, f := range files {
			dirs["./"+filepath.Dir(f)] = true
		}
		var ds []string
		for d := range dirs {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		out = strings.ReplaceAll(out, "{packages}", strings.Join(ds, " "))
	}
	return out
}

func quoteAll(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		if strings.ContainsAny(f, " \t\"'") {
			out[i] = strconv.Quote(f)
		} else {
			out[i] = f
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Config + ignore matching
// ---------------------------------------------------------------------------

func loadConfig(root string) (*config, error) {
	cfg := &config{Commands: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(root, ".testpick.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("invalid .testpick.json: %w", err)
	}
	if cfg.Commands == nil {
		cfg.Commands = map[string]string{}
	}
	return cfg, nil
}

// matchIgnore supports exact paths, "dir/**" prefixes, and path.Match
// globs against the full path and the basename.
func matchIgnore(path string, patterns []string) bool {
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if strings.HasSuffix(pat, "/**") {
			if path == strings.TrimSuffix(pat, "/**") || strings.HasPrefix(path, strings.TrimSuffix(pat, "**")) {
				return true
			}
		}
		if ok, _ := filepath.Match(pat, path); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, filepath.Base(path)); ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Change collection
// ---------------------------------------------------------------------------

func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repo (use --files or --root): %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitDiffNames(args ...string) ([]string, error) {
	cmd := exec.Command("git", append([]string{"diff", "--name-only"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff failed: %w", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// collectChanged returns (changed, added) for the requested input mode.
func collectChanged(root, base, filesFlag, addedFlag string, fromStdin bool) ([]string, []string, error) {
	if filesFlag != "" {
		var changed []string
		for _, f := range strings.Split(filesFlag, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				changed = append(changed, f)
			}
		}
		var added []string
		for _, f := range strings.Split(addedFlag, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				added = append(added, f)
			}
		}
		return changed, added, nil
	}
	if fromStdin {
		var changed []string
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				changed = append(changed, line)
			}
		}
		return changed, nil, sc.Err()
	}
	// git mode
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		// Not a repo root itself; try git rev-parse from root.
		if gr, err2 := gitRoot(); err2 == nil {
			root = gr
		} else {
			return nil, nil, fmt.Errorf("no --files given and %s is not a git repo", root)
		}
	}
	_ = root
	var rangeSpec string
	if base != "" {
		rangeSpec = base + "...HEAD"
	} else {
		rangeSpec = "HEAD"
	}
	changed, err := gitDiffNames(rangeSpec)
	if err != nil {
		return nil, nil, err
	}
	added, err := gitDiffNames("--diff-filter=A", rangeSpec)
	if err != nil {
		return nil, nil, err
	}
	// Untracked files are changes too.
	out, err := exec.Command("git", "ls-files", "--others", "--exclude-standard").Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			changed = append(changed, line)
			added = append(added, line)
		}
	}
	// Dedupe, keep order.
	seen := map[string]bool{}
	var dedup []string
	for _, c := range changed {
		if !seen[c] {
			seen[c] = true
			dedup = append(dedup, c)
		}
	}
	seenA := map[string]bool{}
	var dedupA []string
	for _, a := range added {
		if !seenA[a] {
			seenA[a] = true
			dedupA = append(dedupA, a)
		}
	}
	return dedup, dedupA, nil
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

func printHuman(p *plan) {
	fmt.Printf("testpick: %d changed file(s), %d test file(s) selected\n\n", len(p.Changed), len(p.Selected))
	if len(p.Selected) > 0 {
		fmt.Println("Selected tests (block on these in CI):")
		for _, s := range p.Selected {
			runs := ""
			if s.Runs > 1 {
				runs = fmt.Sprintf("  [new test: run %dx to shake out flakes]", s.Runs)
			}
			fmt.Printf("  %-40s %-7s %s%s\n", s.Test.Path, s.Test.Lang, strings.Join(s.Reasons, "; "), runs)
		}
		fmt.Println()
	}
	if len(p.Commands) > 0 {
		fmt.Println("Commands:")
		for _, c := range p.Commands {
			fmt.Printf("  [%s] %s\n", c.Language, c.Command)
		}
		fmt.Println()
	}
	if len(p.Unmapped) > 0 {
		fmt.Println("Changed code with no matching test (consider adding one, or run the full suite):")
		for _, u := range p.Unmapped {
			fmt.Printf("  %s\n", u)
		}
		fmt.Println()
	}
	if len(p.OtherChanged) > 0 {
		fmt.Println("Other changed files (not code, no test mapping):")
		for _, o := range p.OtherChanged {
			fmt.Printf("  %s\n", o)
		}
		fmt.Println()
	}
	if p.FullSuite {
		fmt.Printf("Full suite recommended: %s\n", p.FullSuiteReason)
	}
	if len(p.Selected) == 0 && !p.FullSuite {
		fmt.Println("No tests selected. If the change is real, the affected code may be untested.")
	}
}

func printMatrix(p *plan) {
	type entry struct {
		Language string `json:"language"`
		Command  string `json:"command"`
	}
	m := struct {
		Include []entry `json:"include"`
	}{}
	for _, c := range p.Commands {
		m.Include = append(m.Include, entry{Language: string(c.Language), Command: c.Command})
	}
	if len(m.Include) == 0 {
		m.Include = []entry{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m)
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	fs := flag.NewFlagSet("testpick", flag.ContinueOnError)
	root := fs.String("root", ".", "repo root to scan (default: current directory)")
	base := fs.String("base", "", "git base ref: compare <base>...HEAD (default: working tree vs HEAD)")
	filesFlag := fs.String("files", "", "comma-separated changed files (repo-relative); skips git")
	addedFlag := fs.String("added", "", "comma-separated newly added files (with --files)")
	fromStdin := fs.Bool("stdin", false, "read changed files (one per line) from stdin")
	depth := fs.Int("depth", 2, "max import-graph depth to follow (1 = direct importers only)")
	jsonOut := fs.Bool("json", false, "print the full plan as JSON")
	matrixOut := fs.Bool("matrix", false, "print a GitHub Actions matrix JSON {\"include\": [...]}")
	quiet := fs.Bool("quiet", false, "print only selected test paths, one per line")
	ignoreFlag := fs.String("ignore", "", "comma-separated ignore patterns (in addition to .testpick.json)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *matrixOut && *jsonOut {
		fmt.Fprintln(os.Stderr, "testpick: --json and --matrix are mutually exclusive")
		os.Exit(2)
	}

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testpick:", err)
		os.Exit(1)
	}
	var ignore []string
	if *ignoreFlag != "" {
		ignore = strings.Split(*ignoreFlag, ",")
	}
	changed, added, err := collectChanged(absRoot, *base, *filesFlag, *addedFlag, *fromStdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testpick:", err)
		os.Exit(1)
	}
	if len(changed) == 0 {
		fmt.Println("testpick: no changed files — nothing to select.")
		return
	}
	r, err := discover(absRoot, ignore)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testpick:", err)
		os.Exit(1)
	}
	p := r.selectTests(changed, added, *depth)
	switch {
	case *jsonOut:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(p.toJSON())
	case *matrixOut:
		printMatrix(p)
	case *quiet:
		for _, s := range p.Selected {
			fmt.Println(s.Test.Path)
		}
	default:
		printHuman(p)
	}
}
