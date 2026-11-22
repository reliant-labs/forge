package goexec

import (
	"os/exec"
	"strings"
	"sync"
)

// CheckEnv is Env for a go command that only CHECKS code — compiles it and
// throws the output away (`go list -export`, a compile check, `go vet`) or
// loads it for an analyzer — with -trimpath added to GOFLAGS (WithTrimpath).
func CheckEnv(extra ...string) []string {
	return WithTrimpath(Env(extra...))
}

// WithTrimpath returns env with -trimpath appended to its effective GOFLAGS,
// unless that already says -trimpath (or -trimpath=false, which is an
// opt-out and is respected). The effective GOFLAGS is env's own GOFLAGS when
// it has one, else the value `go env -w` or the toolchain's go.env sets —
// cmd/go reads exactly one of the layers, so the merge keeps whichever the
// command would have used. env itself is not modified.
//
// # Why
//
// Without -trimpath, cmd/go hashes every main-module package's ABSOLUTE
// directory into its build-cache key. A scratch copy (verify-generated
// regenerates in a fresh `.<repo>.forge-verify-*` every run) or another
// worktree of the same commit therefore compiles every package again while
// identical cache entries sit unused, and the test result cache misses with
// it. On a shared machine that compile happens while holding a heavy slot,
// so it is paid again in every other agent's wait. Measured on a 16-CPU Mac,
// one commit, two worktrees: the second compiled 267 packages and cached 0 of
// 73 test packages; with -trimpath, 0 compiles and 58 of 73 cached.
//
// # Where it applies
//
// Every go command forge starts to compile a project: the checks above, and
// since v1.11.0 `forge build`'s host binaries and the air / go-run dev loop of
// `forge env up` (WithTrimpathPinned) too. A trimmed binary's panic traces
// name module-relative paths (github.com/acme/app/internal/x/x.go:12), the
// same form forge's error stacks report anyway (pkg/svcerr). The exception is
// a binary someone debugs: delve sets breakpoints by absolute source path, so
// every debug build passes -trimpath=false on its command line, which beats
// GOFLAGS (forge debug, the delve runner, the scaffolded air-debug.toml and
// VS Code launch configs).
func WithTrimpath(env []string) []string {
	return withTrimpath(env, goEnvFileFlags)
}

// WithTrimpathPinned is WithTrimpath for an environment that something
// else will add to GOFLAGS after it (hostlaunch.WithPGO appends -pgo=off):
// GOFLAGS is always written into env, with -trimpath merged in unless the
// effective value already says -trimpath either way. WithTrimpath leaves a
// file-layer-only value (`go env -w`, go.env) where it is, and a later
// GOFLAGS=-pgo=off would then hide it from cmd/go — a -trimpath=false
// opt-out included. env itself is not modified.
func WithTrimpathPinned(env []string) []string {
	return withTrimpathPinned(env, goEnvFileFlags)
}

func withTrimpath(env []string, fileFlags func() string) []string {
	flags := effectiveGOFLAGS(env, fileFlags)
	if mentionsTrimpath(flags) {
		return env
	}
	return replaceGOFLAGS(env, strings.TrimSpace(flags+" -trimpath"))
}

func withTrimpathPinned(env []string, fileFlags func() string) []string {
	flags := effectiveGOFLAGS(env, fileFlags)
	if !mentionsTrimpath(flags) {
		flags += " -trimpath"
	}
	return replaceGOFLAGS(env, strings.TrimSpace(flags))
}

// effectiveGOFLAGS is the GOFLAGS a go command started with env reads: env's
// own (the last assignment, as exec resolves it) when non-empty, else the file
// layer — cmd/go treats an empty GOFLAGS as unset.
func effectiveGOFLAGS(env []string, fileFlags func() string) string {
	if flags := lastGOFLAGS(env); strings.TrimSpace(flags) != "" {
		return flags
	}
	return fileFlags()
}

// lastGOFLAGS is env's last GOFLAGS assignment, "" when it has none.
func lastGOFLAGS(env []string) string {
	flags := ""
	for _, kv := range env {
		if key, val, ok := strings.Cut(kv, "="); ok && isGOFLAGSKey(key) {
			flags = val
		}
	}
	return flags
}

// replaceGOFLAGS returns env with every GOFLAGS assignment dropped and
// GOFLAGS=flags appended.
func replaceGOFLAGS(env []string, flags string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if key, _, ok := strings.Cut(kv, "="); ok && isGOFLAGSKey(key) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GOFLAGS="+flags)
}

// mentionsTrimpath reports whether a GOFLAGS value sets -trimpath either way.
func mentionsTrimpath(goflags string) bool {
	for _, f := range strings.Fields(goflags) {
		name, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if name == "trimpath" {
			return true
		}
	}
	return false
}

var (
	fileFlagsOnce sync.Once
	fileFlagsVal  string
)

// goEnvFileFlags is the GOFLAGS cmd/go reads when the environment sets none:
// `go env -w`, else $GOROOT/go.env (where the Reliant workspace image puts
// -trimpath). Asked of go itself, in the current directory, so a go.mod
// toolchain switch reports the switched toolchain's go.env. Once per process.
func goEnvFileFlags() string {
	fileFlagsOnce.Do(func() {
		cmd := exec.Command("go", "env", "GOFLAGS")
		cmd.Env = append(Env(), "GOFLAGS=")
		if out, err := cmd.Output(); err == nil {
			fileFlagsVal = strings.TrimSpace(string(out))
		}
	})
	return fileFlagsVal
}
