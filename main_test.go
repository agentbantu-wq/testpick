package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture creates a temp repo from path->content and returns its root.
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func discoverFixture(t *testing.T, files map[string]string) *repo {
	t.Helper()
	root := writeFixture(t, files)
	r, err := discover(root, nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return r
}

func selectedPaths(p *plan) []string {
	var out []string
	for _, s := range p.Selected {
		out = append(out, s.Test.Path)
	}
	return out
}

func hasPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

var goFixture = map[string]string{
	"pkg/auth/auth.go":      "package auth\n\nfunc Login() bool { return true }\n",
	"pkg/auth/auth_test.go": "package auth\n\nimport \"testing\"\n\nfunc TestLogin(t *testing.T) {}\n",
	"pkg/auth/util.go":      "package auth\n\nfunc helper() {}\n",
	"pkg/store/store.go":    "package store\n\nimport \"example.com/demo/pkg/db\"\n\nfunc Get() string { return db.Query() }\n",
	"pkg/db/db.go":          "package db\n\nfunc Query() string { return \"y\" }\n",
	"pkg/store/store_test.go": "package store\n\nimport (\n\t\"testing\"\n\t\"example.com/demo/pkg/db\"\n)\n\nfunc TestGet(t *testing.T) { _ = db.Query() }\n",
	"cmd/api/main.go": "package main\n\nimport \"example.com/demo/pkg/store\"\n\nfunc main() { _ = store.Get() }\n",
	"cmd/api/main_test.go": "package main\n\nimport (\n\t\"testing\"\n\t\"example.com/demo/pkg/store\"\n)\n\nfunc TestMain(t *testing.T) { _ = store.Get() }\n",
	"pkg/orphan/orphan.go": "package orphan\n\nfunc Lonely() {}\n",
}

func TestNamingGo(t *testing.T) {
	r := discoverFixture(t, goFixture)
	p := r.selectTests([]string{"pkg/auth/auth.go"}, nil, 2)
	if !hasPath(selectedPaths(p), "pkg/auth/auth_test.go") {
		t.Fatalf("expected pkg/auth/auth_test.go selected, got %v", selectedPaths(p))
	}
	if len(p.Unmapped) != 0 {
		t.Fatalf("expected no unmapped files, got %v", p.Unmapped)
	}
}

func TestSamePackageGo(t *testing.T) {
	r := discoverFixture(t, goFixture)
	// util.go has no util_test.go, but auth_test.go is in the same package.
	p := r.selectTests([]string{"pkg/auth/util.go"}, nil, 2)
	if !hasPath(selectedPaths(p), "pkg/auth/auth_test.go") {
		t.Fatalf("expected same-package auth_test.go selected, got %v", selectedPaths(p))
	}
}

func TestImportGraphGoDirect(t *testing.T) {
	r := discoverFixture(t, goFixture)
	// store.go is imported by cmd/api/main_test.go.
	p := r.selectTests([]string{"pkg/store/store.go"}, nil, 2)
	if !hasPath(selectedPaths(p), "cmd/api/main_test.go") {
		t.Fatalf("expected cmd/api/main_test.go selected via import, got %v", selectedPaths(p))
	}
	if !hasPath(selectedPaths(p), "pkg/store/store_test.go") {
		t.Fatalf("expected pkg/store/store_test.go selected (same package), got %v", selectedPaths(p))
	}
}

func TestImportGraphGoTransitive(t *testing.T) {
	r := discoverFixture(t, goFixture)
	// db.go -> imported by store_test.go (depth 1) and by store.go, which is
	// imported by main_test.go (depth 2).
	p := r.selectTests([]string{"pkg/db/db.go"}, nil, 2)
	got := selectedPaths(p)
	if !hasPath(got, "pkg/store/store_test.go") {
		t.Fatalf("expected store_test.go at depth 1, got %v", got)
	}
	if !hasPath(got, "cmd/api/main_test.go") {
		t.Fatalf("expected main_test.go at depth 2, got %v", got)
	}
	for _, s := range p.Selected {
		if s.Test.Path == "cmd/api/main_test.go" && s.Depth != 2 {
			t.Fatalf("expected main_test.go depth 2, got %d", s.Depth)
		}
	}
	// Depth 1 must not reach main_test.go.
	p1 := r.selectTests([]string{"pkg/db/db.go"}, nil, 1)
	if hasPath(selectedPaths(p1), "cmd/api/main_test.go") {
		t.Fatalf("depth 1 must not select main_test.go, got %v", selectedPaths(p1))
	}
}

func TestChangedTestFile(t *testing.T) {
	r := discoverFixture(t, goFixture)
	p := r.selectTests([]string{"pkg/auth/auth_test.go"}, nil, 2)
	if !hasPath(selectedPaths(p), "pkg/auth/auth_test.go") {
		t.Fatalf("changed test file should select itself, got %v", selectedPaths(p))
	}
}

func TestUnmapped(t *testing.T) {
	r := discoverFixture(t, goFixture)
	p := r.selectTests([]string{"pkg/orphan/orphan.go"}, nil, 2)
	if len(p.Selected) != 0 {
		t.Fatalf("expected no tests selected, got %v", selectedPaths(p))
	}
	if !hasPath(p.Unmapped, "pkg/orphan/orphan.go") {
		t.Fatalf("expected orphan.go unmapped, got %v", p.Unmapped)
	}
}

func TestInfraFileForcesFullSuite(t *testing.T) {
	r := discoverFixture(t, goFixture)
	p := r.selectTests([]string{"go.mod"}, nil, 2)
	if !p.FullSuite {
		t.Fatal("expected full suite recommendation for go.mod change")
	}
	if p.FullSuiteReason == "" {
		t.Fatal("expected a full-suite reason")
	}
	// Infra files are not "unmapped code".
	if hasPath(p.Unmapped, "go.mod") {
		t.Fatalf("go.mod must not be unmapped, got %v", p.Unmapped)
	}
}

func TestOtherChangedFiles(t *testing.T) {
	r := discoverFixture(t, goFixture)
	p := r.selectTests([]string{"README.md"}, nil, 2)
	if !hasPath(p.OtherChanged, "README.md") {
		t.Fatalf("expected README.md in other-changed, got %v", p.OtherChanged)
	}
	if len(p.Selected) != 0 {
		t.Fatalf("expected no tests selected, got %v", selectedPaths(p))
	}
}

func TestSuggestRuns(t *testing.T) {
	cases := []struct {
		lines int
		isNew bool
		want  int
	}{
		{10, true, 3},
		{49, true, 3},
		{50, true, 5},
		{200, true, 5},
		{201, true, 10},
		{500, true, 10},
		{500, false, 1},
		{10, false, 1},
	}
	for _, c := range cases {
		if got := suggestRuns(c.lines, c.isNew); got != c.want {
			t.Errorf("suggestRuns(%d, %v) = %d, want %d", c.lines, c.isNew, got, c.want)
		}
	}
}

func TestNewTestGetsRerunsInPlan(t *testing.T) {
	files := map[string]string{
		"pkg/new/new_test.go": "package new\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\nfunc TestC(t *testing.T) {}\n",
		"pkg/new/new.go": "package new\n\nfunc A() {}\n",
	}
	r := discoverFixture(t, files)
	p := r.selectTests([]string{"pkg/new/new_test.go"}, []string{"pkg/new/new_test.go"}, 2)
	if len(p.Selected) != 1 {
		t.Fatalf("expected 1 selected test, got %v", selectedPaths(p))
	}
	if p.Selected[0].Runs <= 1 {
		t.Fatalf("new test should suggest >1 runs, got %d", p.Selected[0].Runs)
	}
}

var pyFixture = map[string]string{
	"app/models.py":          "class User:\n    pass\n",
	"app/util.py":            "def slug(s):\n    return s\n",
	"tests/test_models.py":   "from app.models import User\n\ndef test_user():\n    assert User()\n",
	"tests/test_api.py":      "from app.util import slug\n\ndef test_slug():\n    assert slug('a') == 'a'\n",
	"tests/test_helpers.py":  "def test_nothing():\n    assert True\n",
	"app/unused.py":          "def nope():\n    pass\n",
}

func TestPythonNamingAndImports(t *testing.T) {
	r := discoverFixture(t, pyFixture)
	p := r.selectTests([]string{"app/models.py"}, nil, 2)
	if !hasPath(selectedPaths(p), "tests/test_models.py") {
		t.Fatalf("expected test_models.py via naming/import, got %v", selectedPaths(p))
	}
	p2 := r.selectTests([]string{"app/util.py"}, nil, 2)
	if !hasPath(selectedPaths(p2), "tests/test_api.py") {
		t.Fatalf("expected test_api.py via import, got %v", selectedPaths(p2))
	}
	if hasPath(selectedPaths(p2), "tests/test_helpers.py") {
		t.Fatalf("unrelated test_helpers.py must not be selected, got %v", selectedPaths(p2))
	}
	p3 := r.selectTests([]string{"app/unused.py"}, nil, 2)
	if len(p3.Selected) != 0 || !hasPath(p3.Unmapped, "app/unused.py") {
		t.Fatalf("unused.py should be unmapped with no selection, got selected=%v unmapped=%v", selectedPaths(p3), p3.Unmapped)
	}
}

var jsFixture = map[string]string{
	"src/lib/util.js":              "export function fmt(s) { return s; }\n",
	"src/lib/other.js":             "export function other() {}\n",
	"src/components/App.test.js":   "import { fmt } from '../lib/util.js';\n\ntest('x', () => {});\n",
	"src/lib/util.test.js":         "import { fmt } from './util.js';\n\ntest('fmt', () => {});\n",
	"src/lib/other.test.js":        "import { other } from './other.js';\n\ntest('other', () => {});\n",
}

func TestJSNamingAndImports(t *testing.T) {
	r := discoverFixture(t, jsFixture)
	p := r.selectTests([]string{"src/lib/util.js"}, nil, 2)
	got := selectedPaths(p)
	if !hasPath(got, "src/lib/util.test.js") {
		t.Fatalf("expected util.test.js via naming, got %v", got)
	}
	if !hasPath(got, "src/components/App.test.js") {
		t.Fatalf("expected App.test.js via import, got %v", got)
	}
	if hasPath(got, "src/lib/other.test.js") {
		t.Fatalf("other.test.js must not be selected, got %v", got)
	}
}

func TestCommands(t *testing.T) {
	r := discoverFixture(t, goFixture)
	p := r.selectTests([]string{"pkg/auth/auth.go", "pkg/store/store.go"}, nil, 2)
	var goCmd string
	for _, c := range p.Commands {
		if c.Language == langGo {
			goCmd = c.Command
		}
	}
	if !strings.Contains(goCmd, "go test") {
		t.Fatalf("expected a go test command, got %q", goCmd)
	}
	if !strings.Contains(goCmd, "./pkg/auth") || !strings.Contains(goCmd, "./pkg/store") {
		t.Fatalf("command should cover both packages, got %q", goCmd)
	}
	if !strings.Contains(goCmd, "./cmd/api") {
		t.Fatalf("command should cover cmd/api (main_test.go selected), got %q", goCmd)
	}
}

func TestFullSuiteCommand(t *testing.T) {
	r := discoverFixture(t, goFixture)
	p := r.selectTests([]string{"pkg/auth/auth.go", "go.mod"}, nil, 2)
	for _, c := range p.Commands {
		if c.Language == langGo && c.Command != "go test ./..." {
			t.Fatalf("full-suite go command should be 'go test ./...', got %q", c.Command)
		}
	}
}

func TestIgnoreConfig(t *testing.T) {
	files := map[string]string{
		".testpick.json":            `{"ignore": ["generated/**"]}`,
		"generated/gen.go":         "package gen\n\nfunc G() {}\n",
		"generated/gen_test.go":    "package gen\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) {}\n",
		"pkg/real.go":              "package real\n\nfunc R() {}\n",
		"pkg/real_test.go":         "package real\n\nimport \"testing\"\n\nfunc TestR(t *testing.T) {}\n",
	}
	r := discoverFixture(t, files)
	if _, ok := r.Files["generated/gen_test.go"]; ok {
		t.Fatal("ignored test file should not be indexed")
	}
	p := r.selectTests([]string{"pkg/real.go"}, nil, 2)
	if !hasPath(selectedPaths(p), "pkg/real_test.go") {
		t.Fatalf("expected pkg/real_test.go selected, got %v", selectedPaths(p))
	}
}

func TestPlanJSONRoundTrip(t *testing.T) {
	r := discoverFixture(t, goFixture)
	p := r.selectTests([]string{"pkg/auth/auth.go"}, nil, 2)
	j := p.toJSON()
	if len(j.Selected) != 1 || j.Selected[0].Path != "pkg/auth/auth_test.go" {
		t.Fatalf("unexpected JSON plan: %+v", j.Selected)
	}
	if j.Selected[0].Runs != 1 {
		t.Fatalf("existing test should run once, got %d", j.Selected[0].Runs)
	}
	if len(j.Commands) != 1 || j.Commands[0].Command != "go test ./pkg/auth" {
		t.Fatalf("unexpected JSON commands: %+v", j.Commands)
	}
}
