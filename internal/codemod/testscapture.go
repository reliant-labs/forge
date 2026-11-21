package codemod

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/semver"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/templates"
)

// The Tests upgrade (docs/proposals/tests-coverage-flakes.md §5.2): a project
// scaffolded since v1.9.0 records every test lane's results and coverage
// (`forge test capture`), builds instrumented binaries for its dev env and
// previews, and gates its release on the recorded run. A project scaffolded
// before owns its Taskfile, Dockerfile, .air.toml, CI workflows and Vitest
// configs, so forge never rewrites them on generate; the `tests` playbook
// (migrations/v1.9.0/tests) is the story, and this is its mechanical half.
//
// Every edit is applied only where the file still has the shape forge
// scaffolded — the anchors below are the scaffold's own text, held equal to
// the templates by testscapture_test.go — and moves it to exactly what a new
// scaffold writes:
//
//   - forge.yaml: the `tests:` block (coverage on), unless the project has a
//     `tests:` block or sets the legacy ci.test.coverage.
//   - Taskfile.yml: the Test records vars; `test`, `test:integration` and
//     `test:e2e` through capture (the project's own go test flags kept);
//     the frontend lanes; `test:report`; `coverage` rendered from the record.
//   - .github/workflows/ci.yml and e2e.yml: a `Report tests` step after each
//     test lane; the k3d e2e lane collects from ENV=e2e; the coverage
//     artifact is coverage.html.
//   - .github/workflows/release.yml: the test gate from the record
//     (`forge test report --gate-json`) instead of verify-test-run.
//   - .air.toml: $FORGE_GO_BUILD_FLAGS, so `forge env up` can build the dev
//     server instrumented.
//   - Dockerfile: the `cover-builder` and `cover` stages, derived from the
//     project's own builder RUNs and production stage.
//   - frontends/*: the Vitest lane block and @vitest/coverage-v8.
//
// A file that does not have the scaffolded shape is left alone, and the
// report's Next says what to write instead. Each edit checks for its own
// result first, so a second run changes nothing.

// testsCaptureRelease is the first forge release with `forge test`: the
// release.yml gate step needs the pinned forge to have it.
const testsCaptureRelease = "v1.9.0"

//go:embed testscapture/*.txt
var testsCaptureBlocks embed.FS

// tcBlock returns one of the scaffold fragments under testscapture/: text cut
// from what the v1.9.0 templates render (or, for an `_old` block, what the
// v1.8 templates rendered), each ending in one newline.
func tcBlock(name string) string {
	b, err := testsCaptureBlocks.ReadFile("testscapture/" + name + ".txt")
	if err != nil {
		panic(fmt.Sprintf("tests-capture: missing block %s: %v", name, err))
	}
	return string(b)
}

func init() {
	register(Codemod{
		Name:    "tests-capture",
		Version: "v1.9.0",
		Short:   "Record test results and coverage: move the scaffolded test lanes, CI, Dockerfile, air and Vitest configs onto forge test",
		Long: `Bring a project scaffolded before v1.9.0 onto what a new scaffold writes for
test records (the tests playbook, migrations/v1.9.0/tests):

  forge.yaml       a tests: block with coverage: true (left out when the project
                   already has one, or sets ci.test.coverage)
  Taskfile.yml     test, test:integration and test:e2e run through forge test
                   capture (their own go test flags kept), the frontend lanes
                   record, test:report is added, coverage renders the record
  ci.yml, e2e.yml  a Report tests step after each test lane (never fails the
                   job); the k3d e2e lane runs task test:e2e ENV=e2e
  release.yml      the tests gate comes from forge test report --gate-json
  .air.toml        go build reads $FORGE_GO_BUILD_FLAGS
  Dockerfile       cover-builder and cover stages, from your own builder RUNs
  frontends/*      the Vitest lane block, and @vitest/coverage-v8 at vitest's range

Each edit is made only where the file still has the shape forge scaffolded.
Anything you changed is left alone and listed as a step to do by hand. It
installs nothing (run the frontend's package manager, or let forge project
upgrade's locks phase do it), and re-running it changes nothing. --dry-run
prints the diff.`,
		CleanTree: true,
		Run:       runTestsCapture,
	})
}

// tcRun is one run: the files it read, what it would write, its report.
type tcRun struct {
	dir   string
	files map[string]*tcFile
	order []string
	rep   Report
	// tfPending are the Taskfile edits made so far, written together.
	tfPending []string
}

type tcFile struct {
	before, after string
}

func runTestsCapture(dir string, opts Options) (Report, error) {
	r := &tcRun{dir: dir, files: map[string]*tcFile{}}
	r.forgeYAML()
	lanes := r.taskfile()
	r.workflows(lanes)
	r.airTOML()
	r.dockerfile()
	r.frontends()

	var diff strings.Builder
	for _, rel := range r.order {
		f := r.files[rel]
		if f.after == f.before {
			continue
		}
		diff.WriteString(tcUnifiedDiff(rel, f.before, f.after))
		if !opts.Write {
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(rel))
		mode := os.FileMode(0o644)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if err := atomicfile.WriteFile(path, []byte(f.after), mode); err != nil {
			return r.rep, err
		}
	}
	r.rep.Diff = diff.String()
	if r.rep.Changed() {
		r.rep.Verify = []string{"task test", "task test:report", "forge lint"}
	}
	return r.rep, nil
}

// read returns rel's current content: what this run has made of it so far.
func (r *tcRun) read(rel string) (string, bool) {
	if f, ok := r.files[rel]; ok {
		return f.after, true
	}
	b, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel))) //nolint:gosec // a project file
	if err != nil {
		return "", false
	}
	r.files[rel] = &tcFile{before: string(b), after: string(b)}
	r.order = append(r.order, rel)
	return string(b), true
}

// set records rel's new content and one report line for the edit.
func (r *tcRun) set(rel, content, why string) {
	if _, ok := r.files[rel]; !ok {
		r.read(rel)
	}
	if r.files[rel].after == content {
		return
	}
	r.files[rel].after = content
	r.rep.Rewrites = append(r.rep.Rewrites, rel+": "+why)
}

func (r *tcRun) next(format string, args ...any) {
	r.rep.Next = append(r.rep.Next, fmt.Sprintf(format, args...))
}

func tcUnifiedDiff(rel, a, b string) string {
	d, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(a),
		B:        difflib.SplitLines(b),
		FromFile: "a/" + rel,
		ToFile:   "b/" + rel,
		Context:  3,
	})
	if err != nil {
		return fmt.Sprintf("(diff of %s unavailable: %v)\n", rel, err)
	}
	return d
}

// ── forge.yaml ───────────────────────────────────────────────────────────────

// tcForgeYAML is what the codemod reads from forge.yaml.
type tcForgeYAML struct {
	ForgeVersion string         `yaml:"forge_version"`
	Tests        *yaml.Node     `yaml:"tests"`
	CI           map[string]any `yaml:"ci"`
}

func (r *tcRun) loadForgeYAML() (tcForgeYAML, bool) {
	var doc tcForgeYAML
	src, ok := r.read("forge.yaml")
	if !ok {
		return doc, false
	}
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return doc, false
	}
	return doc, true
}

// legacyCoverage reports whether forge.yaml sets ci.test.coverage, and to what.
func (d tcForgeYAML) legacyCoverage() (any, bool) {
	test, ok := d.CI["test"].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := test["coverage"]
	return v, ok
}

func (r *tcRun) forgeYAML() {
	doc, ok := r.loadForgeYAML()
	if !ok {
		return
	}
	if doc.Tests != nil {
		return
	}
	if v, set := doc.legacyCoverage(); set {
		r.next("forge.yaml sets ci.test.coverage: %v, which the lanes and the cover builds read while tests.coverage is unset, so no tests: block was added. "+
			"To move to the new key, replace it with `tests: {coverage: %v}` (forge skill load migrations/v1.9.0/tests)", v, v)
		return
	}
	src, _ := r.read("forge.yaml")
	if !strings.HasSuffix(src, "\n") {
		src += "\n"
	}
	r.set("forge.yaml", src+templates.ForgeYAMLTestsBlock, "a tests: block with coverage: true (the lanes measure coverage; dev and previews build instrumented)")
}

// pinPredatesForgeTest reports whether forge.yaml may pin a forge without
// `forge test`: any version whose release is below v1.9.0, unless it is the
// forge running now (which has it). A pseudo-version of a commit between
// v1.8.0 and v1.9.0 counts as below: whether that commit has the command
// cannot be read from the version, and a wrong guess fails every release.
// An unpinned, dev or unreadable pin is not judged.
func (r *tcRun) pinPredatesForgeTest() (string, bool) {
	doc, ok := r.loadForgeYAML()
	if !ok {
		return "", false
	}
	pin := strings.TrimSpace(doc.ForgeVersion)
	v := pin
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) || strings.Contains(v, "+") {
		return pin, false
	}
	if pin == buildinfo.Version() || v == buildinfo.Version() {
		return pin, false
	}
	core := v
	if i := strings.IndexByte(core, '-'); i >= 0 {
		core = core[:i]
	}
	return pin, semver.Compare(core, testsCaptureRelease) < 0
}

// ── shared text helpers ──────────────────────────────────────────────────────

// tcReindent moves a fragment written at indent from to indent to.
func tcReindent(block string, from, to int) string {
	if from == to {
		return block
	}
	lines := strings.SplitAfter(block, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		lines[i] = strings.Repeat(" ", to) + strings.TrimPrefix(l, strings.Repeat(" ", from))
	}
	return strings.Join(lines, "")
}

// tcIndent is the count of leading spaces on line.
func tcIndent(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// tcSortedDirs lists the directories matching glob under dir, relative and
// slash-separated, sorted.
func tcSortedDirs(dir, glob string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, filepath.FromSlash(glob)))
	var out []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			if rel, err := filepath.Rel(dir, m); err == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
	}
	sort.Strings(out)
	return out
}
