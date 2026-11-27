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
// GOFLAGS is split into its entries the way go splits it (on whitespace; go
// supports no quoting there), with globbing off, and only an entry that IS an
// -ldflags flag counts (-ldflags, --ldflags, with or without =value), as
// hostlaunch.goflagsSetLdflags decides it: an -ldflagsx, or an -ldflags inside
// another flag's value, leaves the default in place. It uses shell builtins
// alone, because Task runs it with mvdan.cc/sh's interpreter on every OS.
//
// It is the same default `forge env up` gives air and go-run
// (hostlaunch.WithDevLinkFlags). Release builds (`forge build`, the
// Dockerfile) and debug builds keep their own flags.
const TaskfileDevLdflagsScript = `'w=-ldflags=-w; case "$FORGE_DEV_DWARF" in 1) w= ;; esac; set -f; for f in $(go env GOFLAGS 2>/dev/null); do case "$f" in -ldflags|-ldflags=*|--ldflags|--ldflags=*) w= ;; esac; done; echo "$w"'`
