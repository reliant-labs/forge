// Command consumercanary builds forge's consumers (reliant, control-plane)
// against the forge checkout under test. Run it with
// `go run ./internal/consumercanary` or `task canary`; it is CI plumbing, no
// part of the forge binary.
//
//	consumercanary [-forge .] [-base origin/main] [-consumer name=source[@ref]]...
//	               [-cache DIR] [-force] [-plan] [-no-test]
//
// WHY. forge may break its consumers' build freely (it is pre-1.0), but the
// PR that does so pays for it: fixes forge, or ships the codemod. Without this
// check the cost surfaces at pin time, on whoever pins next (research/pinning,
// failure classes F1/F2). This runs the consumers' own build against the PR's
// forge, on the PR.
//
// WHAT. For each consumer, at its main: `go build ./...`, `go vet ./...`, and
// `go test -short` of the packages that import a forge package the diff
// changed. The consumer is never modified and never committed: its tree is
// exported (git archive) into a scratch directory and pointed at the PR's
// forge with a throwaway `-modfile` that carries a `replace`; its own go.mod
// and go.sum are untouched.
//
// WHEN. Only when the diff can reach a consumer's build (internal/forgereach:
// a non-test Go file under pkg/ or a package pkg/ imports, or go.mod/go.sum).
// A CLI-only or docs-only diff exits in seconds. A pass is cached by the hash
// of the forge sources that matter and the consumer's commit.
//
// BLAME. A step that fails is re-run once against forge at -base (the
// revision the diff is judged against). Failures present there too predate
// the change (the consumer is red on its own, or forge's main already broke
// it): reported, not blamed on the PR. Only failures the change adds fail the
// run. With -force there is no diff and no baseline: every failure counts.
//
// SOURCES. A consumer source is a git URL or a local path (any repository
// with the commit; its working tree is never read), and the ref defaults to
// main. A URL is fetched shallow (--depth 1) into the cache. Credentials are
// git's own: for a private repository set GIT_SSH_COMMAND or use a credential
// helper (the CI workflow writes a deploy key; see dev-docs/consumer-canary.md).
//
//forge:exclude-contract: a CI command, no collaborator to substitute
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/reliant-labs/forge/internal/forgereach"
	"github.com/reliant-labs/forge/pkg/hostslot"
)

// defaultConsumers are the repositories whose build forge keeps green.
var defaultConsumers = []string{
	"reliant=https://github.com/reliant-labs/reliant.git",
	"control-plane=https://github.com/reliant-labs/control-plane.git",
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "consumercanary:", err)
		return 1
	}
	return 0
}

// Consumer is one repository forge must keep building.
type Consumer struct {
	Name   string
	Source string
	Ref    string
}

// ParseConsumer reads name=source[@ref]. The ref follows the LAST '@' that
// comes after the last '/' or ':' of the source, so git@github.com:org/repo.git
// and https://host/org/repo.git@v1 both parse.
func ParseConsumer(spec string) (Consumer, error) {
	name, src, ok := strings.Cut(spec, "=")
	if !ok || name == "" || src == "" {
		return Consumer{}, fmt.Errorf("consumer %q: want name=source[@ref]", spec)
	}
	c := Consumer{Name: name, Source: src, Ref: "main"}
	if i := strings.LastIndex(src, "@"); i > 0 && i > strings.LastIndexAny(src, "/:") {
		c.Source, c.Ref = src[:i], src[i+1:]
	}
	if c.Source == "" || c.Ref == "" {
		return Consumer{}, fmt.Errorf("consumer %q: empty source or ref", spec)
	}
	return c, nil
}

func isURL(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@")
}

type options struct {
	forge     string
	base      string
	consumers []Consumer
	cache     string
	force     bool
	plan      bool
	noTest    bool
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("consumercanary", flag.ContinueOnError)
	fs.SetOutput(stderr)
	forge := fs.String("forge", ".", "the forge checkout under test")
	base := fs.String("base", "origin/main", "git revision the diff is judged against (CI: HEAD^1)")
	cache := fs.String("cache", "", "cache directory (default: the user cache dir)")
	force := fs.Bool("force", false, "run even when the diff cannot reach a consumer")
	plan := fs.Bool("plan", false, "print what would run and stop")
	noTest := fs.Bool("no-test", false, "build and vet only")
	var specs stringList
	fs.Var(&specs, "consumer", "name=source[@ref] (repeatable; default: reliant and control-plane at main)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(specs) == 0 {
		specs = defaultConsumers
	}
	o := options{base: *base, cache: *cache, force: *force, plan: *plan, noTest: *noTest}
	var err error
	if o.forge, err = filepath.Abs(*forge); err != nil {
		return err
	}
	for _, s := range specs {
		c, err := ParseConsumer(s)
		if err != nil {
			return err
		}
		o.consumers = append(o.consumers, c)
	}
	if o.cache == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			dir = os.TempDir()
		}
		o.cache = filepath.Join(dir, "forge-consumer-canary")
	}
	return execute(ctx, o, stdout, stderr)
}

func execute(ctx context.Context, o options, stdout, stderr io.Writer) error {
	changed, err := forgereach.ChangedFiles(o.forge, o.base)
	if err != nil {
		return err
	}
	reach, err := forgereach.Compute(o.forge, o.base, changed)
	if err != nil {
		return err
	}
	if o.force && !reach.API() {
		reach.All = true
	}
	if !reach.API() {
		fmt.Fprintf(stdout, "consumercanary: skipped — the diff reaches no consumer build (%s)\n", reach.Summary())
		if len(reach.Generated) > 0 {
			fmt.Fprintln(stdout, "  note: it changes generated output; the canary does not regenerate consumers (see dev-docs/consumer-canary.md, \"What it does not cover\")")
		}
		return nil
	}
	fmt.Fprintf(stdout, "consumercanary: %s\n", reach.Summary())
	if o.plan {
		for _, c := range o.consumers {
			fmt.Fprintf(stdout, "  would build %s (%s @ %s) against %s\n", c.Name, c.Source, c.Ref, o.forge)
		}
		for _, p := range reach.Packages {
			fmt.Fprintf(stdout, "  changed: %s\n", p)
		}
		return nil
	}

	key, err := forgeKey(o.forge)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.cache, 0o755); err != nil {
		return err
	}

	// One heavy slot for the whole run: it is a whole-module build per consumer.
	slot, err := hostslot.Acquire(ctx, hostslot.Heavy, hostslot.Options{Out: stderr, Env: os.Environ(), Label: "forge consumer canary"})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(stderr, "consumercanary: running without a heavy slot: %v\n", err)
	} else {
		defer func() { _ = slot.Release() }()
	}

	var results []Result
	for _, c := range o.consumers {
		results = append(results, canary(ctx, o, c, reach, key, stdout, stderr))
	}
	return report(stdout, results)
}

// testFlags are the flags of the consumer test step: the short tier, as a pull
// request runs it.
var testFlags = []string{"-short", "-timeout", "20m"}

// Result is the outcome for one consumer.
type Result struct {
	Consumer string
	Commit   string
	Status   string // PASS, PASS (cached), PASS (failures predate this change), FAIL, ERROR
	Detail   string
	Took     time.Duration
}

func (r Result) failed() bool { return r.Status == "FAIL" || r.Status == "ERROR" }

func report(w io.Writer, rs []Result) error {
	var b strings.Builder
	b.WriteString("\n### Consumer canary\n\n| consumer | commit | result | time |\n|---|---|---|---|\n")
	failed := false
	for _, r := range rs {
		c := r.Commit
		if len(c) > 9 {
			c = c[:9]
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.Consumer, c, r.Status, r.Took.Round(time.Second))
		failed = failed || r.failed()
	}
	for _, r := range rs {
		if r.Detail != "" {
			fmt.Fprintf(&b, "\n**%s**\n\n```\n%s\n```\n", r.Consumer, r.Detail)
		}
	}
	fmt.Fprint(w, b.String())
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			_, _ = f.WriteString(b.String())
			_ = f.Close()
		}
	}
	if failed {
		return errors.New("a consumer does not build against this forge: fix forge, or ship the `forge fix` codemod and declare the break (dev-docs/api-breaks.md)")
	}
	return nil
}

// forgeKey hashes what a consumer's build can depend on: every non-test Go
// file, go.mod and go.sum of the forge module. Test files, docs and testdata
// cannot change a consumer's build, so editing them keeps a cached pass valid.
func forgeKey(root string) (string, error) {
	h := sha256.New()
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// The go tool ignores directories that start with "." or "_"
			// (a consumer checked out as _consumers/ is not forge's source).
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			switch name {
			case "node_modules", "testdata", "web-runtime", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if name == "go.mod" || name == "go.sum" || (strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
	}
	fmt.Fprintf(h, "go\x00%s", runtime.Version())
	return hex.EncodeToString(h.Sum(nil))[:24], nil
}

// materialize exports the consumer's tree at its ref into dst and returns the
// commit. A URL is fetched shallow into the cache; a local path is read with
// git archive, so its working tree and refs are never changed.
func materialize(ctx context.Context, o options, c Consumer, dst string) (string, error) {
	repo, rev := c.Source, c.Ref
	if !isURL(repo) {
		abs, err := filepath.Abs(repo)
		if err != nil {
			return "", err
		}
		repo = abs
	}
	if isURL(c.Source) {
		repo = filepath.Join(o.cache, "src", c.Name+".git")
		if _, err := os.Stat(repo); err != nil {
			if err := os.MkdirAll(repo, 0o755); err != nil {
				return "", err
			}
			if out, err := runCmd(ctx, "", nil, "git", "init", "--bare", "-q", repo); err != nil {
				return "", fmt.Errorf("git init: %w\n%s", err, out)
			}
		}
		if out, err := runCmd(ctx, "", nil, "git", "-C", repo, "fetch", "--depth", "1", "--no-tags", "-q", c.Source, c.Ref); err != nil {
			return "", fmt.Errorf("fetch %s %s: %w\n%s", c.Source, c.Ref, err, strings.TrimSpace(out))
		}
		rev = "FETCH_HEAD"
	}
	sha, err := forgereach.Git(repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve %s: %w", c.Name, c.Ref, err)
	}
	commit := strings.TrimSpace(string(sha))
	if err := forgereach.ExportRev(repo, commit, dst); err != nil {
		return "", err
	}
	return commit, nil
}

func canary(ctx context.Context, o options, c Consumer, reach forgereach.Reach, key string, stdout, stderr io.Writer) (res Result) {
	start := time.Now()
	res.Consumer = c.Name
	defer func() { res.Took = time.Since(start) }()
	fail := func(status string, format string, args ...any) Result {
		res.Status, res.Detail = status, fmt.Sprintf(format, args...)
		return res
	}

	scratch, err := os.MkdirTemp("", "canary-"+c.Name+"-")
	if err != nil {
		return fail("ERROR", "%v", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	tree := filepath.Join(scratch, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		return fail("ERROR", "%v", err)
	}
	commit, err := materialize(ctx, o, c, tree)
	if err != nil {
		return fail("ERROR", "%v", err)
	}
	res.Commit = commit

	marker := filepath.Join(o.cache, "verdicts", key+"-"+c.Name+"-"+commit)
	if _, err := os.Stat(marker); err == nil {
		fmt.Fprintf(stdout, "== %s @ %.9s: PASS (cached for this forge and this consumer commit)\n", c.Name, commit)
		res.Status = "PASS (cached)"
		return res
	}

	mod, ok, err := pointAtForge(ctx, tree, scratch, "head", o.forge, reach.Module)
	switch {
	case err != nil:
		return fail("ERROR", "%v", err)
	case !ok:
		fmt.Fprintf(stdout, "== %s @ %.9s: does not depend on %s, nothing to check\n", c.Name, commit, reach.Module)
		res.Status = "PASS (no forge dependency)"
		return res
	}

	// -trimpath: the scratch directory is new every run, and without it the
	// directory is part of every package's build-cache key, so nothing would
	// ever be reused.
	flags := os.Getenv("GOFLAGS")
	if !strings.Contains(" "+flags+" ", " -trimpath ") {
		flags = joinFlags(flags, "-trimpath")
	}
	pr := goEnv(append(os.Environ(), "GOFLAGS="+joinFlags(flags, "-modfile="+mod, "-mod=mod")))

	// The baseline is forge at the revision the diff is judged against, so a
	// failure is blamed on the change only when the base forge did not have it:
	// a consumer that is already red on its own, or already broken by forge's
	// main, does not turn an unrelated pull request red. A forced run has no
	// diff to blame (the nightly asks "do the consumers build against forge
	// main?"), so it has no baseline and every failure counts.
	var base []string
	if !o.force {
		baseForge := filepath.Join(scratch, "forge-base")
		if err := os.Mkdir(baseForge, 0o755); err != nil {
			return fail("ERROR", "%v", err)
		}
		if err := forgereach.ExportRev(o.forge, o.base, baseForge); err != nil {
			return fail("ERROR", "export forge at %s: %v", o.base, err)
		}
		baseMod, _, err := pointAtForge(ctx, tree, scratch, "base", baseForge, reach.Module)
		if err != nil {
			return fail("ERROR", "%v", err)
		}
		base = goEnv(append(os.Environ(), "GOFLAGS="+joinFlags(flags, "-modfile="+baseMod, "-mod=mod")))
	}

	fmt.Fprintf(stdout, "== %s @ %.9s\n", c.Name, commit)
	var preexisting []string
	logs := filepath.Join(o.cache, "logs")
	_ = os.MkdirAll(logs, 0o755)
	keep := func(name, which, out string) string {
		p := filepath.Join(logs, c.Name+"-"+name+"-"+which+".log")
		_ = os.WriteFile(p, []byte(out), 0o644)
		return p
	}
	// step runs `go <args...>`; for a test step, flags are the test flags and
	// pkgs the selection, so a baseline re-run can be narrowed to the packages
	// that failed instead of repeating the whole selection.
	step := func(name string, args ...string) (string, bool) {
		t := time.Now()
		fmt.Fprintf(stdout, "-- %s: go %s\n", name, strings.Join(args, " "))
		out, err := runCmd(ctx, tree, pr, "go", args...)
		took := time.Since(t).Round(time.Second)
		if err == nil {
			fmt.Fprintf(stdout, "   ok (%s)\n", took)
			return "", true
		}
		prLog := keep(name, "forge-under-test", out)
		if base == nil {
			fmt.Fprintf(stdout, "   FAILED (%s), full output %s\n", took, prLog)
			return tail(failureSummary(out), 80), false
		}
		fmt.Fprintf(stdout, "   FAILED (%s), full output %s; comparing with forge at %s\n", took, prLog, o.base)
		baseArgs := args
		if name == "test" {
			if failed := failedPackages(out); len(failed) > 0 {
				baseArgs = append(append([]string{"test"}, testFlags...), failed...)
			}
		}
		baseOut, berr := runCmd(ctx, tree, base, "go", baseArgs...)
		baseLog := keep(name, "base", baseOut)
		if berr == nil {
			return tail(failureSummary(out), 80), false
		}
		fresh := NewFailures(out, baseOut)
		if len(fresh) == 0 && ctx.Err() == nil {
			preexisting = append(preexisting, name)
			fmt.Fprintf(stdout, "   the same failures occur with forge at %s (%s): not caused by this change\n", o.base, baseLog)
			return "", true
		}
		return tail(strings.Join(fresh, "\n"), 80), false
	}

	if detail, ok := step("build", "build", "./..."); !ok {
		return fail("FAIL", "go build ./... broke against this forge:\n%s", detail)
	}
	if detail, ok := step("vet", "vet", "./..."); !ok {
		return fail("FAIL", "go vet ./... broke against this forge:\n%s", detail)
	}
	if !o.noTest {
		pkgs, err := SelectTests(ctx, tree, pr, reach)
		if err != nil {
			return fail("ERROR", "select tests: %v", err)
		}
		if len(pkgs) == 0 {
			fmt.Fprintf(stdout, "-- test: no %s package imports a changed forge package\n", c.Name)
		} else {
			fmt.Fprintf(stdout, "-- test: %d package(s) import a changed forge package\n", len(pkgs))
			if detail, ok := step("test", append(append([]string{"test"}, testFlags...), pkgs...)...); !ok {
				return fail("FAIL", "go test -short broke against this forge:\n%s", detail)
			}
		}
	}

	if len(preexisting) > 0 {
		res.Status = "PASS (failures predate this change)"
		res.Detail = fmt.Sprintf("%s: %s fail with forge at %s too, so this change did not cause them.", c.Name, strings.Join(preexisting, ", "), o.base)
		return res
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err == nil {
		_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	}
	res.Status = "PASS"
	return res
}

// pointAtForge writes a copy of the consumer's go.mod and go.sum into scratch
// with a replace of forgeModule by the forge checkout, and returns the copy's
// path for -modfile. ok is false when the consumer does not require forge at
// all. The consumer's own files are never written.
func pointAtForge(ctx context.Context, tree, scratch, name, forge, forgeModule string) (mod string, ok bool, err error) {
	if _, err := os.Stat(filepath.Join(tree, "go.mod")); err != nil {
		return "", false, fmt.Errorf("the consumer has no go.mod at its root")
	}
	mod = filepath.Join(scratch, name+".mod")
	sum := filepath.Join(scratch, name+".sum")
	for _, f := range [][2]string{{filepath.Join(tree, "go.mod"), mod}, {filepath.Join(tree, "go.sum"), sum}} {
		b, err := os.ReadFile(f[0])
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		if err := os.WriteFile(f[1], b, 0o644); err != nil {
			return "", false, err
		}
	}
	if out, err := runCmd(ctx, tree, nil, "go", "mod", "edit", "-modfile="+mod, "-replace", forgeModule+"="+forge); err != nil {
		return "", false, fmt.Errorf("go mod edit: %w\n%s", err, out)
	}
	return mod, strings.Contains(string(mustRead(mod)), forgeModule+" "), nil
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}

// goEnv turns the go.work file off: a workspace beside the consumer would
// override the replace this tool is testing.
func goEnv(env []string) []string {
	return append(env, "GOWORK=off")
}

func joinFlags(existing string, add ...string) string {
	return strings.TrimSpace(existing + " " + strings.Join(add, " "))
}

func runCmd(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// failureSummary is the lines of out that report a failure, or out's tail when
// none matches (a failure the patterns do not know must still be shown).
func failureSummary(out string) string {
	var res []string
	for _, l := range strings.Split(out, "\n") {
		if signalRE.MatchString(l) {
			res = append(res, strings.TrimRight(l, " \t"))
		}
	}
	if len(res) == 0 {
		return out
	}
	return strings.Join(res, "\n")
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("... (%d earlier lines)", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}
