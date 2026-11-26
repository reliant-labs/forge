package templates

// TaskfileDevLdflagsScript is the `sh:` value (YAML source, single-quoted) of
// the DEV_LDFLAGS var a scaffolded Taskfile's dev builds (`task build`) pass to
// `go build`, and of the FORGE_GO_BUILD_FLAGS `task dev` hands air. Unquoted,
// it prints -ldflags=-w: the dev build links without DWARF debug info, which
// roughly halves the link of a large binary and changes nothing but what a
// debugger can read (panics, stack traces and pprof use the symbol table, which
// -w keeps). It prints nothing when FORGE_DEV_DWARF=1 (exactly 1: the opt-out,
// for attaching a debugger; hostlaunch.DevDWARFEnv) or when GOFLAGS already
// carries an -ldflags: -ldflags is one flag whose last value replaces the
// others, so a second one on the command line would drop the user's -X.
//
// It is the same default `forge env up` gives air and go-run
// (hostlaunch.WithDevLinkFlags). Release builds (`forge build`, the
// Dockerfile) and debug builds keep their own flags.
const TaskfileDevLdflagsScript = `'case "$FORGE_DEV_DWARF" in 1) ;; *) case " $(go env GOFLAGS 2>/dev/null) " in *" -ldflags"*|*" --ldflags"*) ;; *) echo "-ldflags=-w" ;; esac ;; esac'`
