package affected

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/goexec"
)

// FullEnv forces the full unit suite on a branch.
const FullEnv = "CI_FULL"

// BaseEnv names the ref a branch's changes are measured from, for a branch
// stacked on another (default: origin/<default branch>).
const BaseEnv = "CI_BASE"

// Result is the unit lane's mode for one gate run.
type Result struct {
	// Full means run everything; Reason says why.
	Full   bool
	Reason string
	// Packages are the import paths to test (affected mode).
	Packages []string
	// Total is how many packages in the module have tests.
	Total int
	// BaseRef is the ref diffed against (origin/main); Base its merge-base
	// with HEAD, short.
	BaseRef, Base string
	// Changed are the changed files, project-relative.
	Changed []string
	// Why maps each selected package to what selected it (the first few
	// causes), and Claims maps each changed file to what claimed it.
	Why    map[string]string
	Claims map[string]string
	// Tagged maps a build-tag lane (e2e, integration) to the packages whose
	// tagged tests the change reaches. The unit lane does not run them.
	Tagged map[string][]string
}

// Line is the one line the gate prints about the unit lane's mode.
func (a Result) Line() string {
	if a.Full {
		return "unit tests: full suite — " + a.Reason
	}
	if len(a.Packages) == 0 {
		return fmt.Sprintf("unit tests: affected mode — no package with tests reaches the %d changed file(s) since %s (%s); %s=1 runs the full suite",
			len(a.Changed), a.BaseRef, a.Base, FullEnv)
	}
	return fmt.Sprintf("unit tests: affected mode — %d of %d test packages reach the %d changed file(s) since %s (%s); %s=1 runs the full suite",
		len(a.Packages), a.Total, len(a.Changed), a.BaseRef, a.Base, FullEnv)
}

// Input configures Compute.
type Input struct {
	Root string
	// Env is the environment go list runs with; nil is the process's.
	Env []string
	// Force, when non-empty, is the reason to run the full suite regardless
	// (CI_FULL=1, --full).
	Force string
	// BaseRef overrides origin/<default>: the ref a stacked branch diffs
	// against (CI_BASE / --base).
	BaseRef string
	// Ignore are ci.gate.affected_ignore's globs: files no Go test reads.
	Ignore []string
	// Rules are ci.gate.affected_rules: what the files the import graph does
	// not own select.
	Rules []config.CIAffectedRule
	// Files, when non-empty, is the changed-file list to judge instead of
	// asking git (replaying a merged PR, a pre-commit hook's staged files).
	Files []string
	// Git and GoList run the two tools; nil uses the real binaries. Tests
	// substitute.
	Git    func(ctx context.Context, dir string, args ...string) (string, error)
	GoList func(ctx context.Context, dir string, env []string) (io.Reader, error)
	// GoListTagged is GoList with the e2e and integration tags set.
	GoListTagged func(ctx context.Context, dir string, env []string) (io.Reader, error)
}

// Compute decides the unit lane's mode for the work tree at in.Root.
// It never fails: anything it cannot decide is a reason to run everything.
func Compute(ctx context.Context, in Input) Result {
	full := func(reason string) Result { return Result{Full: true, Reason: reason} }
	if in.Force != "" {
		return full(in.Force)
	}
	git := in.Git
	if git == nil {
		git = runGit
	}
	var a Result
	var changed []string
	if len(in.Files) > 0 {
		changed = uniqueLines(strings.Join(in.Files, "\n"))
		a.BaseRef = "the given file list"
	} else {
		var reason string
		a.BaseRef, a.Base, changed, reason = gitChanges(ctx, in, git)
		if reason != "" {
			return full(reason)
		}
	}
	a.Changed = changed

	list := in.GoList
	if list == nil {
		list = runGoList
	}
	r, err := list(ctx, in.Root, in.Env)
	if err != nil {
		return full("go list failed: " + firstLine(err.Error()))
	}
	g, err := readPackageGraph(r, in.Root)
	if err != nil {
		return full("cannot read go list output: " + err.Error())
	}
	// The build-tag graph (e2e, integration) costs a second `go list`, so it
	// is loaded once and only when a tagged test could be reached.
	var tagged *packageGraph
	triedTagged := false
	tagGraph := func() *packageGraph {
		if triedTagged || (in.GoListTagged == nil && len(g.taggedLanes) == 0) {
			return tagged
		}
		triedTagged = true
		list := in.GoListTagged
		if list == nil {
			list = runGoListTagged
		}
		if r, err := list(ctx, in.Root, in.Env); err == nil {
			tagged, _ = readPackageGraph(r, in.Root)
		}
		return tagged
	}
	res := selectAffected(ctx, in, g, tagGraph, git, changed)
	if res.full != "" {
		return full(res.full)
	}
	a.Total = len(g.testDeps)
	a.Packages = res.packages
	a.Why = res.why
	a.Claims = res.claims
	a.Tagged = res.tagged
	return a
}

// ignoredByGlobs matches rel against affected_ignore globs: a pattern with no
// slash matches the base name anywhere (`*.md`), `dir/**` matches everything
// under dir, anything else matches the whole path (path.Match syntax).
func ignoredByGlobs(rel string, globs []string) bool {
	rel = filepath.ToSlash(rel)
	for _, g := range globs {
		g = strings.TrimPrefix(filepath.ToSlash(g), "./")
		switch {
		case strings.HasSuffix(g, "/**"):
			if strings.HasPrefix(rel, strings.TrimSuffix(g, "**")) {
				return true
			}
		case !strings.Contains(g, "/"):
			if ok, _ := path.Match(g, path.Base(rel)); ok {
				return true
			}
		default:
			if ok, _ := path.Match(g, rel); ok {
				return true
			}
		}
	}
	return false
}

func underTestdata(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "testdata" {
			return true
		}
	}
	return false
}

// stripTestVariant turns "p [q.test]" into "p".
func stripTestVariant(path string) string {
	if i := strings.Index(path, " ["); i >= 0 {
		return path[:i]
	}
	return path
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func uniqueLines(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func runGoList(ctx context.Context, dir string, env []string) (io.Reader, error) {
	return goList(ctx, dir, env)
}

func runGoListTagged(ctx context.Context, dir string, env []string) (io.Reader, error) {
	return goList(ctx, dir, env, "-tags=e2e,integration")
}

func goList(ctx context.Context, dir string, env []string, extra ...string) (io.Reader, error) {
	args := append([]string{"list", "-e", "-deps", "-test",
		"-json=ImportPath,Name,Dir,ForTest,Standard,Deps,EmbedFiles,TestEmbedFiles,XTestEmbedFiles,EmbedPatterns,TestEmbedPatterns,XTestEmbedPatterns,IgnoredGoFiles"},
		extra...)
	cmd := exec.CommandContext(ctx, "go", append(args, "./...")...)
	cmd.Dir = dir
	if env == nil {
		env = goexec.Env()
	}
	// The caller's env is already scrubbed (goexec.Env); a go list that
	// inherited GOFLAGS=-mod=mod would rewrite go.mod while choosing tests.
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return bytes.NewReader(out), nil
}

// WritePlan prints the decision for one lane: the mode, then every selected
// package with the changed file that selected it, then every changed file
// with what accounted for it.
func (a Result) WritePlan(w io.Writer, lane string, selected []string) {
	if a.Full {
		fmt.Fprintf(w, "affected (%s): FULL — %s\n", lane, a.Reason)
		return
	}
	total := fmt.Sprintf("of %d test packages", a.Total)
	if lane != "unit" {
		total = "with -tags " + lane + " tests"
	}
	fmt.Fprintf(w, "affected (%s): %d %s reach the %d changed file(s) since %s %s\n", lane, len(selected), total, len(a.Changed), a.BaseRef, a.Base)
	pkgs := append([]string(nil), selected...)
	sort.Strings(pkgs)
	for _, p := range pkgs {
		why := a.Why[p]
		if why == "" {
			why = "a changed file of this package carries the " + lane + " build tag"
		}
		fmt.Fprintf(w, "  run  %-48s %s\n", shortPkg(p), why)
	}
	for _, f := range a.Changed {
		fmt.Fprintf(w, "  file %-48s %s\n", f, a.Claims[f])
	}
}

func shortPkg(p string) string {
	if i := strings.Index(p, "/internal/"); i >= 0 {
		return p[i+1:]
	}
	if i := strings.Index(p, "/pkg/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// gitChanges is the work tree's diff against the merge-base with the default
// branch: committed, staged, unstaged and untracked. A non-empty reason is why
// it cannot say, and the whole suite runs.
func gitChanges(ctx context.Context, in Input, git func(context.Context, string, ...string) (string, error)) (baseRef, short string, changed []string, reason string) {
	baseRef = in.BaseRef
	defaultBranch := ""
	if baseRef == "" {
		if ref, err := git(ctx, in.Root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
			baseRef = strings.TrimPrefix(ref, "refs/remotes/")
		} else {
			for _, cand := range []string{"origin/main", "origin/master"} {
				if _, err := git(ctx, in.Root, "rev-parse", "--verify", "--quiet", "refs/remotes/"+cand); err == nil {
					baseRef = cand
					break
				}
			}
		}
		if baseRef == "" {
			return "", "", nil, "no origin/<default branch> to diff against"
		}
		defaultBranch = strings.TrimPrefix(baseRef, "origin/")
	}
	if branch, err := git(ctx, in.Root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && defaultBranch != "" && branch == defaultBranch {
		return "", "", nil, "on the default branch (" + branch + ")"
	}
	base, err := git(ctx, in.Root, "merge-base", "HEAD", baseRef)
	if err != nil || base == "" {
		return "", "", nil, "no merge-base with " + baseRef
	}
	diff, err := git(ctx, in.Root, "diff", "--name-only", "--no-renames", base)
	if err != nil {
		return "", "", nil, "cannot diff against " + baseRef
	}
	untracked, err := git(ctx, in.Root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", "", nil, "cannot list untracked files"
	}
	changed = uniqueLines(diff + "\n" + untracked)
	short = base
	if len(short) > 12 {
		short = short[:12]
	}
	if len(changed) == 0 {
		// Nothing to narrow by: a branch identical to its base proves
		// nothing about the base unless the suite actually runs.
		return "", "", nil, "nothing changed since " + baseRef + " (" + short + ")"
	}
	return baseRef, short, changed, ""
}
