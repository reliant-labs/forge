package storage

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// pruneArgs runs one builder's GC against a fake docker and returns the
// `buildx prune` it asked for ("" when it asked for none), and what it printed.
func pruneArgs(t *testing.T, r Runner, apply bool) (string, string) {
	t.Helper()
	var prune string
	var out bytes.Buffer
	r.Out = &out
	r.Command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "buildx inspect relbuild":
			return []byte(buildxInspectText("relbuild", "docker-container", "unix:///var/run/docker.sock")), nil
		case strings.HasPrefix(joined, "buildx prune "):
			prune = joined
			return []byte("Total:\t0B"), nil
		default:
			t.Fatalf("unexpected command: %s %s", name, joined)
			return nil, nil
		}
	}
	if err := r.builderGC(context.Background(), "relbuild", apply); err != nil {
		t.Fatal(err)
	}
	return prune, out.String()
}

// TestBuilderGCEnforcesTheBudgetBySize is the bug this rule fixes: the prune
// carried --filter until=build_cache_unused (7 days), and BuildKit removes a
// record only when it is older than the filter AND the cache is over budget,
// so a cache a week of builds had filled was never pruned. The budget now
// applies to everything older than the 2h floor.
func TestBuilderGCEnforcesTheBudgetBySize(t *testing.T) {
	t.Setenv(BuildCacheFloorEnv, "")
	gib := func(n uint64) string { return fmt.Sprintf("--max-used-space %dB", n*GiB) }
	for _, tc := range []struct {
		name     string
		edit     func(*Runner)
		env      string
		wantArgs []string
		wantOut  string
	}{
		{name: "default: size rule under the 2h floor",
			wantArgs: []string{gib(20), "--filter until=2h"}, wantOut: "toward 20.0 GiB, never cache used in the last 2h"},
		{name: "pressure: half the budget, same floor",
			edit:     func(r *Runner) { r.Pressure = true },
			wantArgs: []string{gib(10), "--filter until=2h"}, wantOut: "disk pressure: half of build_cache_gib"},
		{name: "pressure never prunes below 1 GiB",
			edit:     func(r *Runner) { r.Pressure = true; r.Policy.BuildCacheGiB = 1 },
			wantArgs: []string{gib(1), "--filter until=2h"}},
		{name: "policy floor",
			edit:     func(r *Runner) { r.Policy.BuildCacheFloor = "6h" },
			wantArgs: []string{gib(20), "--filter until=6h"}},
		{name: "the floor never exceeds build_cache_unused",
			edit:     func(r *Runner) { r.Policy.BuildCacheUnused = "1h" },
			wantArgs: []string{gib(20), "--filter until=1h"}},
		{name: "policy opt-out restores the age-gated budget",
			edit:     func(r *Runner) { r.Policy.BuildCacheFloor = "off" },
			wantArgs: []string{gib(20), "--filter until=168h"}, wantOut: "build_cache_floor off"},
		{name: "opt-out still halves under pressure",
			edit:     func(r *Runner) { r.Policy.BuildCacheFloor = "off"; r.Pressure = true },
			wantArgs: []string{gib(10), "--filter until=168h"}},
		{name: "env opt-out overrides the policy",
			edit: func(r *Runner) { r.Policy.BuildCacheFloor = "4h" }, env: "off",
			wantArgs: []string{gib(20), "--filter until=168h"}},
		{name: "env floor overrides the policy",
			edit: func(r *Runner) { r.Policy.BuildCacheFloor = "4h" }, env: "3h",
			wantArgs: []string{gib(20), "--filter until=3h"}},
		{name: "an invalid env value falls back to the policy and says so",
			edit: func(r *Runner) { r.Policy.BuildCacheFloor = "4h" }, env: "soon",
			wantArgs: []string{gib(20), "--filter until=4h"}, wantOut: "warning: " + BuildCacheFloorEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(BuildCacheFloorEnv, tc.env)
			r := Runner{Policy: DefaultPolicy()}
			if tc.edit != nil {
				tc.edit(&r)
			}
			prune, out := pruneArgs(t, r, true)
			if !strings.HasPrefix(prune, "buildx prune --builder relbuild --force ") {
				t.Fatalf("prune = %q", prune)
			}
			for _, want := range tc.wantArgs {
				if !strings.Contains(prune, want) {
					t.Errorf("prune = %q, want it to contain %q", prune, want)
				}
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output %q does not contain %q", out, tc.wantOut)
			}
		})
	}
}

// TestBuilderGCPreviewPrunesNothing: without --apply the size rule is only
// described.
func TestBuilderGCPreviewPrunesNothing(t *testing.T) {
	t.Setenv(BuildCacheFloorEnv, "")
	prune, out := pruneArgs(t, Runner{Policy: DefaultPolicy(), Pressure: true}, false)
	if prune != "" {
		t.Fatalf("preview ran %q", prune)
	}
	if !strings.Contains(out, "toward 10.0 GiB") {
		t.Fatalf("preview output: %q", out)
	}
}

func TestPolicyValidatesBuildCacheFloor(t *testing.T) {
	for _, v := range []string{"", "off", "OFF", "1h", "2h", "90m", "168h"} {
		p := DefaultPolicy()
		p.BuildCacheFloor = v
		if err := p.Validate(); err != nil {
			t.Errorf("build_cache_floor %q refused: %v", v, err)
		}
	}
	for _, v := range []string{"30m", "0", "soon", "-2h"} {
		p := DefaultPolicy()
		p.BuildCacheFloor = v
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "build_cache_floor") {
			t.Errorf("build_cache_floor %q accepted (err %v)", v, err)
		}
	}
}
