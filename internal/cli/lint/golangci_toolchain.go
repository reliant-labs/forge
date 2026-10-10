package lint

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/reliant-labs/forge/internal/goexec"
	"github.com/reliant-labs/forge/internal/templates"
)

// golangci-lint can only analyze code written for a Go language version no
// newer than the Go it was BUILT with. Run against a newer module it stops
// before linting anything:
//
//	can't load config: the Go language version (go1.26) used to build
//	golangci-lint is lower than the targeted Go version (1.27)
//
// That is what `forge lint` hit on control-plane and reliant (both `go 1.27`):
// the workspace image ships a golangci-lint release binary built with
// go1.26.2, so the gating lane could not run at all, on every project, until
// someone rebuilt the tool by hand. The binary on PATH is not wrong in
// general — it is wrong for THIS module — so the decision is made per module:
//
//  1. Use golangci-lint from PATH when it was built with a Go at least as
//     new as the module's go directive (the common case: no work, no
//     network).
//  2. Otherwise build forge's pinned golangci-lint with the very toolchain
//     `go` selects for this module, into a forge-owned cache keyed by both
//     versions, and lint with that. It is built once per (pin, toolchain).
//  3. If that build fails (offline, no toolchain), fail the lane with a
//     message naming the binary, both Go versions and the command that fixes
//     it — not golangci-lint's own error, which names neither the binary nor
//     the remedy.

// golangciModulePath is the v2 module forge installs golangci-lint from.
const golangciModulePath = "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"

// golangciCacheEnv overrides where forge builds its own golangci-lint
// (default <UserCacheDir>/forge/tools/golangci-lint), for read-only home
// directories and test isolation.
const golangciCacheEnv = "FORGE_TOOLS_CACHE"

// golangciResolution is the outcome of choosing a golangci-lint for a module.
type golangciResolution struct {
	// path is the binary to run. Empty when none is available.
	path string
	// missing is true when no golangci-lint is on PATH at all — the lane
	// is skipped, exactly as before toolchain matching existed.
	missing bool
	// err is a gating failure: a golangci-lint exists but cannot analyze
	// this module, and forge could not provision one that can.
	err error
}

// golangciToolchain is the outside world resolveGolangciLint consults. The
// zero value is not usable; productionGolangciToolchain wires the real one,
// tests substitute fakes for the toolchain-dependent parts.
type golangciToolchain struct {
	lookPath func(file string) (string, error)
	// binaryGoVersion reports the Go release a binary was built with
	// ("go1.26.2").
	binaryGoVersion func(path string) (string, error)
	// moduleGoVersion reports the toolchain `go` selects for the module
	// at dir ("go1.27.0") — the one that compiles it, so the one whose
	// language golangci-lint must understand.
	moduleGoVersion func(ctx context.Context, dir string) (string, error)
	// install builds golangci-lint@version with toolchain goVersion into
	// gobin.
	install  func(ctx context.Context, gobin, goVersion, version string) error
	cacheDir func() (string, error)
	pin      string
	// notify receives the one line printed before a build starts, so a
	// one-time minute-long build never reads as a hang.
	notify func(msg string)
}

func productionGolangciToolchain() golangciToolchain {
	return golangciToolchain{
		lookPath:        exec.LookPath,
		binaryGoVersion: binaryGoVersion,
		moduleGoVersion: moduleToolchainVersion,
		install:         installGolangciLint,
		cacheDir:        golangciCacheDir,
		pin:             templates.GolangciLintVersion,
		notify:          func(msg string) { fmt.Fprintln(os.Stderr, msg) },
	}
}

// resolveGolangciLint picks the golangci-lint that can analyze the module at
// dir. See the file comment for the three outcomes.
func resolveGolangciLint(ctx context.Context, dir string, tc golangciToolchain) golangciResolution {
	onPath, err := tc.lookPath("golangci-lint")
	if err != nil {
		return golangciResolution{missing: true}
	}

	// The language the module is written in, from go.mod. No go.mod (or
	// no go directive) means there is no version to mismatch.
	modLang := moduleGoDirective(dir)
	if modLang == "" {
		return golangciResolution{path: onPath}
	}

	builtWith, err := tc.binaryGoVersion(onPath)
	if err != nil || !version.IsValid(builtWith) {
		// Not a Go binary forge can read (a wrapper script, a stripped
		// build). It may well work; golangci-lint reports a real mismatch
		// itself.
		return golangciResolution{path: onPath}
	}
	if langAtLeast(builtWith, modLang) {
		return golangciResolution{path: onPath}
	}

	// Mismatch. Build the pin with the toolchain that compiles this module.
	toolchain, err := tc.moduleGoVersion(ctx, dir)
	if err != nil || !version.IsValid(toolchain) {
		return golangciResolution{err: golangciMismatchError(onPath, builtWith, modLang, "", tc.pin,
			fmt.Errorf("could not determine the Go toolchain for this module: %w", errOrInvalid(err, toolchain)))}
	}
	if !langAtLeast(toolchain, modLang) {
		return golangciResolution{err: golangciMismatchError(onPath, builtWith, modLang, toolchain, tc.pin,
			fmt.Errorf("the selected Go toolchain %s is itself older than the module's go %s", toolchain, modLang))}
	}

	root, err := tc.cacheDir()
	if err != nil {
		return golangciResolution{err: golangciMismatchError(onPath, builtWith, modLang, toolchain, tc.pin, err)}
	}
	gobin := filepath.Join(root, tc.pin+"-"+toolchain)
	built := filepath.Join(gobin, golangciExeName())
	if v, verr := tc.binaryGoVersion(built); verr == nil && langAtLeast(v, modLang) {
		return golangciResolution{path: built}
	}

	tc.notify(fmt.Sprintf("🔧 golangci-lint at %s was built with %s, older than this module's go %s — building golangci-lint %s with %s into %s (one time)…",
		onPath, builtWith, modLang, tc.pin, toolchain, gobin))
	if err := installAtomically(ctx, tc, root, gobin, toolchain); err != nil {
		return golangciResolution{err: golangciMismatchError(onPath, builtWith, modLang, toolchain, tc.pin, err)}
	}
	if v, verr := tc.binaryGoVersion(built); verr != nil || !langAtLeast(v, modLang) {
		return golangciResolution{err: golangciMismatchError(onPath, builtWith, modLang, toolchain, tc.pin,
			fmt.Errorf("the build produced no usable binary at %s", built))}
	}
	return golangciResolution{path: built}
}

// installAtomically builds into a private staging directory and renames the
// binary into gobin, so concurrent `forge lint` runs (parallel CI jobs, two
// agents) never observe — or exec — a half-written binary. Two racing
// builders each rename a complete binary; the last one wins.
func installAtomically(ctx context.Context, tc golangciToolchain, root, gobin, toolchain string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".build-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := tc.install(ctx, stage, toolchain, tc.pin); err != nil {
		return err
	}
	if err := os.MkdirAll(gobin, 0o755); err != nil {
		return err
	}
	return os.Rename(filepath.Join(stage, golangciExeName()), filepath.Join(gobin, golangciExeName()))
}

// golangciMismatchError is the actionable failure: which binary, which two
// Go versions disagree, why forge could not fix it, and the command that
// does.
func golangciMismatchError(onPath, builtWith, modLang, toolchain, pin string, cause error) error {
	// The fix must build with a toolchain that speaks the module's
	// language: the selected one when it does, else the first release
	// that does (GOTOOLCHAIN downloads it).
	fixTC := toolchain
	if fixTC == "" || !langAtLeast(fixTC, modLang) {
		fixTC = releaseFor(modLang)
	}
	return fmt.Errorf("golangci-lint at %s was built with %s, but this module targets go %s (go.mod) — "+
		"golangci-lint cannot analyze Go newer than the Go it was built with. "+
		"forge could not build golangci-lint %s for it: %v. "+
		"Fix: run `GOTOOLCHAIN=%s go install %s@%s` and put its GOBIN ahead of %s on PATH",
		onPath, builtWith, modLang, pin, cause, fixTC, golangciModulePath, pin, filepath.Dir(onPath))
}

// releaseFor names the first toolchain release that speaks language
// version lang: "1.27" → "go1.27.0" (GOTOOLCHAIN wants a release, and
// "go1.27" is not one); "1.27.3" → "go1.27.3".
func releaseFor(lang string) string {
	if strings.Count(lang, ".") == 1 {
		return "go" + lang + ".0"
	}
	return "go" + lang
}

func errOrInvalid(err error, got string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("unrecognised version %q", got)
}

// langAtLeast reports whether Go release have speaks at least language
// version want ("1.27", "1.27.0" or "go1.27"). Only the language (major.minor)
// matters: that is the check golangci-lint itself makes.
func langAtLeast(have, want string) bool {
	if !strings.HasPrefix(want, "go") {
		want = "go" + want
	}
	return version.Compare(version.Lang(have), version.Lang(want)) >= 0
}

// moduleGoDirective returns the go directive of the module enclosing dir
// ("1.27"), or "" when there is none.
func moduleGoDirective(dir string) string {
	for d := dir; ; {
		data, err := os.ReadFile(filepath.Join(d, "go.mod"))
		if err == nil {
			f, perr := modfile.ParseLax("go.mod", data, nil)
			if perr != nil || f.Go == nil {
				return ""
			}
			return f.Go.Version
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// binaryGoVersion reads the Go release a binary was built with from its
// embedded build info — no subprocess, works for any Go executable.
func binaryGoVersion(path string) (string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", err
	}
	return info.GoVersion, nil
}

// moduleToolchainVersion asks `go` which toolchain it runs for the module at
// dir — after GOTOOLCHAIN switching, so a module that requires a newer Go
// reports (and downloads) that one.
func moduleToolchainVersion(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOVERSION")
	cmd.Dir = dir
	cmd.Env = goexec.Env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOVERSION: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// installGolangciLint builds golangci-lint@ver with exactly toolchain
// goVersion into gobin. It runs from gobin (a fresh staging directory),
// outside any module, so the project's go.mod and go.work cannot steer the
// build.
func installGolangciLint(ctx context.Context, gobin, goVersion, ver string) error {
	if err := os.MkdirAll(gobin, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "go", "install", golangciModulePath+"@"+ver)
	cmd.Dir = gobin
	cmd.Env = goexec.Env("GOBIN="+gobin, "GOTOOLCHAIN="+goVersion, "GOWORK=off", "GOFLAGS=")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(out.String())
		if detail == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, detail)
	}
	return nil
}

// golangciCacheDir is where forge keeps the golangci-lint builds it makes.
func golangciCacheDir() (string, error) {
	if dir := os.Getenv(golangciCacheEnv); dir != "" {
		return filepath.Join(dir, "golangci-lint"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", errors.New("locate the user cache directory for forge's golangci-lint build: " + err.Error() +
			" (set " + golangciCacheEnv + ")")
	}
	return filepath.Join(base, "forge", "tools", "golangci-lint"), nil
}

func golangciExeName() string {
	if filepath.Separator == '\\' {
		return "golangci-lint.exe"
	}
	return "golangci-lint"
}

// golangciUnresolvedFinding is the --json / --quiet form of a golangci-lint
// that cannot analyze this module and could not be replaced: the actionable
// message rather than golangci-lint's own exit-3 output.
func golangciUnresolvedFinding(err error, sev string) lintJSONFinding {
	return lintJSONFinding{
		Severity: sev,
		Rule:     "external",
		Message:  err.Error(),
		FixHint:  "install golangci-lint " + templates.GolangciLintVersion + " built with a Go at least as new as this module's go directive (the command is in the message)",
	}
}
