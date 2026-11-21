# testpick

Run only the tests your change can actually affect — and know which ones to block on.

`testpick` reads the files a change touches and proposes exactly which test
files CI should run: by file-name convention, by same-package tests, and by
walking the reverse import graph (so a change to a deep dependency selects
the tests of everything that depends on it, transitively). New test files
get a suggested rerun count, so flaky tests get shaken out before they
block a merge. It is deterministic and local-only: no LLM, no network, no
test execution. It answers "which tests?" so CI stays fast without guessing
by file path alone.

## Why this exists

The ask is specific and recent. In "Ask HN: What developer tool do you wish
existed in 2026?" (Dec 2025):

> "An LLM tool that can sit on a CI pipeline to propose what tests should be
> blocking. Instead of brute-force method of selecting the appropriate test
> suites by path or similar, have LLM analyze changes and propose the set of
> test suites that is relevant to the change. If there are new complex tests
> added to the change, estimates how many times to run them to ensure they
> are not flaky to begin with (hundreds? or thousands?)."
> — kubanczyk, <https://news.ycombinator.com/item?id=46354793>

The same thread carries the neighbouring complaint — "a sane CI system...
prototype the script/job on my own machine before committing" (wojciii,
<https://news.ycombinator.com/item?id=46400062>) — and the pain is live
right now: "Traditional CI/CD systems are 'dumb' — they run predefined
commands without understanding what changed or why" (Agentic CI, Show HN,
Jan 2026, <https://news.ycombinator.com/item?id=46794500>). Still Unbuilt's
"CI runners for every platform" (#94) tracks the same CI-cost cluster: 7
asks, 4 since 2024
(<https://stillunbuilt.shiptiffin.app/i/ci-runners-for-every-platform/>).

The LLM is the garnish, not the missing piece. The missing piece is the
deterministic core — a test-impact analysis that works for repos without
Bazel, in any language, with no service to run. That's testpick. (It also
deliberately does *not* try to be the LLM layer: point an LLM at
testpick's JSON plan if you want the prose version.)

## Quickstart

```bash
go build -o testpick .
# From a git repo: compare the working tree against HEAD
./testpick
# Compare a branch against its base
./testpick --base main
# Or hand it the changed files explicitly (works outside git)
./testpick --files pkg/db/db.go,pkg/auth/auth.go
# New test files get a rerun suggestion: pass which files are new
./testpick --files pkg/new/new_test.go --added pkg/new/new_test.go
```

## Demo

Against the fixture repo in `examples/demo-repo` — where `cmd/api` imports
`pkg/store`, and `pkg/store` imports `pkg/db`:

```bash
$ ./testpick --root examples/demo-repo --files pkg/db/db.go
testpick: 1 changed file(s), 2 test file(s) selected

Selected tests (block on these in CI):
  cmd/api/main_test.go                     go      imports pkg/store/store.go (depth 2)
  pkg/store/store_test.go                  go      imports pkg/db/db.go (depth 1)

Commands:
  [go] go test ./cmd/api ./pkg/store
```

Changing `pkg/auth/auth.go` in the same run selects only `auth_test.go` —
the store tests stay out. Changing `go.mod` recommends the full suite.

## How selection works

For each changed file, testpick selects test files by, in order:

1. **Changed test file** — a changed test selects itself.
2. **Naming convention** — `foo.go` → `foo_test.go`; `foo.py` →
   `test_foo.py` / `foo_test.py`; `foo.js` → `foo.test.js` / `foo.spec.js`.
3. **Same package** (Go) — any test in the same directory as a changed
   `.go` file.
4. **Reverse import graph** — tests that import the changed file, or
   import a module that imports it, up to `--depth` (default 2). Go
   imports resolve by import-path suffix against repo directories;
   Python by module path (including relative imports); JS/TS by relative
   `import` / `require`.

Changed code with no matching test is reported as **unmapped** — a prompt
to add a test, or to run the full suite. Changed infra files (`go.mod`,
`package.json`, `Dockerfile`, `.github/*`, lockfiles, ...) recommend the
**full suite** instead of a selective plan.

**Rerun estimate.** A new test file is suggested 3 runs if under 50 lines,
5 runs up to 200 lines, 10 runs above that. It is a heuristic, labelled as
one in the output — a shake-out, not a measurement. Existing tests run once.

## Output modes

```bash
./testpick --json      # full plan: selections, reasons, depths, reruns, commands
./testpick --matrix    # {"include": [{"language": "go", "command": "go test ..."}]}
./testpick --quiet     # selected test paths only, one per line (for piping)
```

`--matrix` drops straight into a GitHub Actions job matrix:

```yaml
jobs:
  pick:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.pick.outputs.matrix }}
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - id: pick
        run: |
          go run github.com/agentbantu-wq/testpick@latest \
            --base origin/main --matrix > matrix.json
          echo "matrix=$(cat matrix.json)" >> "$GITHUB_OUTPUT"
  test:
    needs: pick
    strategy:
      matrix: ${{ fromJson(needs.pick.outputs.matrix) }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: ${{ matrix.command }}
```

## Configuration

Optional `.testpick.json` in the repo root:

```json
{
  "ignore": ["generated/**", "fixtures/**"],
  "commands": {
    "go": "go test {packages}",
    "python": "pytest {files} -q",
    "js": "npx vitest run {files}"
  }
}
```

`{files}` is the selected test paths; `{packages}` is the deduplicated Go
package dirs. `--ignore a,b` adds patterns on the command line.

## Flags

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--root` | `.` | Repo root to scan |
| `--base` | — | Git base ref; compare `<base>...HEAD` |
| `--files` | — | Comma-separated changed files (skips git) |
| `--added` | — | Comma-separated newly added files (with `--files`) |
| `--stdin` | — | Read changed files, one per line, from stdin |
| `--depth` | `2` | Max import-graph depth to follow |
| `--json` | — | Print the full plan as JSON |
| `--matrix` | — | Print a GitHub Actions matrix JSON |
| `--quiet` | — | Print only selected test paths |
| `--ignore` | — | Extra comma-separated ignore patterns |

Exit code is 0 when a plan is produced (even with unmapped files), 1 on
usage errors or git failures.

## Languages

Go, Python, and JavaScript/TypeScript, detected by extension and test-file
naming. Everything else is reported as an unmapped/other changed file
rather than silently ignored.

## What testpick is not

- Not a test runner — it prints commands, it doesn't execute them.
- Not an LLM — selection is exact, from the import graph and naming.
- Not a CI system — it emits a plan/matrix for the CI you already have.
- Not a coverage tool — it selects by dependency, not by lines covered.
