package hostlaunch

import (
	"slices"
	"strings"
	"testing"
)

// TestWithTrimpath: the runners that compile on launch (go-run, air) get
// -trimpath merged into GOFLAGS, so a second worktree's dev loop reuses the
// first's build-cache entries; a -trimpath=false opt-out is kept, and runners
// that execute a forge-built binary (or a verbatim command) are left alone.
// Composed with WithPGO after it, both flags survive.
func TestWithTrimpath(t *testing.T) {
	t.Cleanup(func(prev func([]string) []string) func() {
		return func() { trimpathGOFLAGS = prev }
	}(trimpathGOFLAGS))
	// The file layer (`go env -w`) says nothing, so env alone decides; the
	// merge itself is goexec's and tested there.
	trimpathGOFLAGS = func(env []string) []string {
		var out []string
		flags := ""
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "GOFLAGS="); ok {
				flags = v
				continue
			}
			out = append(out, kv)
		}
		if !strings.Contains(flags, "trimpath") {
			flags = strings.TrimSpace(flags + " -trimpath")
		}
		return append(out, "GOFLAGS="+flags)
	}
	base := []string{"PATH=/bin", "GOFLAGS=-mod=vendor"}
	cases := []struct {
		name string
		spec RunnerSpec
		env  []string
		want []string
	}{
		{"go-run", RunnerSpec{GoPkg: "./cmd/app"}, base, []string{"PATH=/bin", "GOFLAGS=-mod=vendor -trimpath"}},
		{"air", RunnerSpec{Runner: "air"}, []string{"PATH=/bin"}, []string{"PATH=/bin", "GOFLAGS=-trimpath"}},
		{"opt-out kept", RunnerSpec{Runner: "air"}, []string{"GOFLAGS=-trimpath=false"}, []string{"GOFLAGS=-trimpath=false"}},
		{"binary runner", RunnerSpec{Runner: "binary", GoPkg: "./cmd/app"}, base, base},
		{"delve runner", RunnerSpec{Runner: "delve", GoPkg: "./cmd/app"}, base, base},
		{"verbatim command", RunnerSpec{Command: []string{"go", "run", "../sib/cmd/x"}}, base, base},
	}
	for _, c := range cases {
		in := slices.Clone(c.env)
		if got := WithTrimpath(c.spec, c.env); !slices.Equal(got, c.want) {
			t.Errorf("%s: env = %q, want %q", c.name, got, c.want)
		}
		if !slices.Equal(c.env, in) {
			t.Errorf("%s: WithTrimpath modified its input", c.name)
		}
	}

	spec := RunnerSpec{Runner: "air", PGOOff: true}
	got := WithPGO(spec, WithTrimpath(spec, []string{"PATH=/bin"}))
	if want := []string{"PATH=/bin", "GOFLAGS=-trimpath -pgo=off"}; !slices.Equal(got, want) {
		t.Errorf("WithPGO after WithTrimpath: env = %q, want %q", got, want)
	}
}
