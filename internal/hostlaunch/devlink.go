package hostlaunch

import (
	"regexp"
	"slices"
	"strings"
)

// DevDWARFEnv is the variable that keeps DWARF in dev-loop builds:
// FORGE_DEV_DWARF=1 (exactly "1", as the air build scripts of reliant and
// control-plane read it) makes forge add no -w (see WithDevLinkFlags), for a developer who attaches a debugger to
// the binary air or go-run built. The debug paths (the delve runner, forge
// build --debug, forge debug start, .air-debug.toml) never get -w either way.
const DevDWARFEnv = "FORGE_DEV_DWARF"

// DevLinkFlag is the flag forge adds to the builds of the runners that compile
// on launch (air, go-run): link without DWARF debug info. It halves the link
// of a large server binary (24 s to 13 s, 5.0 to 4.3 GB peak memory, measured
// on reliant's ./cmd/reliant) and changes nothing a running dev server does:
// panics, stack traces, runtime.Caller and pprof read the symbol table and
// pclntab, which -w keeps. Only a debugger reading DWARF loses (DevDWARFEnv).
// It never contains whitespace, so it passes through $FORGE_GO_BUILD_FLAGS.
const DevLinkFlag = "-ldflags=-w"

// DevDWARF reports whether env asks for DWARF in dev builds (DevDWARFEnv=1;
// the last assignment wins, as it does for exec).
func DevDWARF(env []string) bool {
	v, _ := LookupEnv(env, DevDWARFEnv)
	return v == "1"
}

// WithDevLinkFlags returns spec with its dev-loop build linked without DWARF:
// DevLinkFlag in GoBuildFlags for the runners that compile on launch, which
// go-run puts on its `go run` and air gets in $FORGE_GO_BUILD_FLAGS
// (WithGoBuildFlags). env is the environment the build runs with, and
// goflags the GOFLAGS its go command reads (env's own, else the go env
// file's). spec is not modified.
//
// -ldflags is one flag whose last occurrence replaces every earlier one (and
// one in GOFLAGS), so forge never adds a second one beside an -ldflags
// somebody declared, and never drops a declared -X:
//
//   - the binary and delve runners, and a verbatim spec.command, run what
//     they run: nothing is added;
//   - FORGE_DEV_DWARF=1 in env: nothing is added;
//   - GOFLAGS carries -ldflags: the user's ldflags decide, nothing is added;
//   - a debug build — its flags, or its air config's command, disable
//     optimizations with -gcflags …-N… (a .air-debug.toml): a debugger needs
//     its DWARF, so nothing is added;
//   - air whose build command is not itself a `go build` (a script, make:
//     forge cannot see what -ldflags it passes, and a -ldflags=-w after its
//     -X would replace it), or cannot be read, or passes its own -ldflags,
//     or whose env already sets $FORGE_GO_BUILD_FLAGS (the user's, which
//     WithGoBuildFlags keeps): nothing is added;
//   - GoBuild.flags declares -ldflags: go-run gets -w merged into that value
//     (`-ldflags=-X main.v=dev -w`), unless it already decides -w; air cannot,
//     because the merged value contains a space and $FORGE_GO_BUILD_FLAGS is
//     split on spaces, so air keeps the declared value as it is;
//   - otherwise DevLinkFlag is appended.
//
// GoBuild.ldflags reach only `forge build` (and its variants), never these
// runners, so there is nothing of theirs to merge here.
func WithDevLinkFlags(spec RunnerSpec, env []string, goflags string) RunnerSpec {
	if !spec.CompilesOnLaunch() || DevDWARF(env) || goflagsSetLdflags(goflags) {
		return spec
	}
	air := strings.TrimSpace(spec.Runner) == "air"
	if air {
		if _, set := LookupEnv(env, GoBuildFlagsEnv); set {
			return spec
		}
		if cmd := spec.AirBuildCmd(); !goBuildCmd.MatchString(cmd) || strings.Contains(cmd, "-ldflags") || debugGcflags(cmd) {
			return spec
		}
	}
	if debugGcflags(strings.Join(spec.GoBuildFlags, " ")) {
		return spec
	}
	var at []int // the indexes of the declared -ldflags flags
	for i, f := range spec.GoBuildFlags {
		if isLdflagsFlag(f) {
			at = append(at, i)
		}
	}
	switch {
	case len(at) == 0:
		spec.GoBuildFlags = append(slices.Clone(spec.GoBuildFlags), DevLinkFlag)
	case air || len(at) > 1:
		// air: the merged value cannot pass through $FORGE_GO_BUILD_FLAGS.
		// Several -ldflags (per-package patterns): which one links the
		// binary is go's to decide, not forge's.
	default:
		flags := slices.Clone(spec.GoBuildFlags)
		i := at[0]
		name, value, inline := strings.Cut(flags[i], "=")
		if !inline {
			if i+1 >= len(flags) {
				return spec // a dangling -ldflags: go reports it
			}
			i, value = i+1, flags[i+1]
		}
		if ldflagsDecideW(value) {
			return spec
		}
		value = strings.TrimSpace(value + " -w")
		if inline {
			flags[i] = name + "=" + value
		} else {
			flags[i] = value
		}
		spec.GoBuildFlags = flags
	}
	return spec
}

// isLdflagsFlag reports whether f is a go command's -ldflags flag, in any of
// its spellings (-ldflags, --ldflags, with or without =value).
func isLdflagsFlag(f string) bool {
	if !strings.HasPrefix(f, "-") {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(f, "-"), "-"), "=")
	return name == "ldflags"
}

// goflagsSetLdflags reports whether GOFLAGS (space-separated -flag=value
// entries) carries an -ldflags.
func goflagsSetLdflags(goflags string) bool {
	return slices.ContainsFunc(strings.Fields(goflags), isLdflagsFlag)
}

// ldflagsDecideW reports whether an -ldflags value (an optional
// `pattern=` prefix, then linker flags) already says -w either way: a -w=false
// in it is a choice to keep DWARF.
func ldflagsDecideW(value string) bool {
	return slices.ContainsFunc(strings.Fields(value), func(f string) bool {
		name, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
		return strings.HasPrefix(f, "-") && name == "w"
	})
}

// goBuildCmd matches a build command that runs `go build` itself (the
// scaffolded .air.toml, in its sh and PowerShell forms), as opposed to one
// that hands the flags to a script.
var goBuildCmd = regexp.MustCompile(`(^|[\s;&|(/\\])go(\.exe)?\s+build(\s|$)`)

// debugGcflags reports whether a build command (or a space-joined flag list)
// passes -gcflags that disable optimizations (-N), the mark of a build made
// for a debugger: `-gcflags='all=-N -l'`, `"-gcflags=all=-N -l"`.
func debugGcflags(cmd string) bool {
	_, after, ok := strings.Cut(cmd, "-gcflags")
	return ok && strings.Contains(after, "-N")
}

// LookupEnv is os.LookupEnv over an exec-style env: the last assignment of
// key wins, as it does for the process it starts.
func LookupEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v, true
		}
	}
	return "", false
}
