package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestUpOptionsCarryNoPlacementOverride is the structural guard behind the
// removal of --host-only / --cluster-only.
//
// Those flags did not merely scope a run: --host-only rewrote every
// cluster-declared service into a local `go run` process, so forge overruled
// the environment's own placement decision on the strength of a CLI flag. The
// replacement is declarative — a service that should run on the host says so
// with a `host` block in its KCL, and a render option (`-D host_runner=go-run`)
// selects the runner.
//
// The regression this catches is re-introducing that override under any name.
// upOptions is the only channel from the command line into runUp, so a
// placement-shaped boolean appearing on it is the shape of the mistake, and a
// field list is the one assertion that fails when someone adds one.
func TestUpOptionsCarryNoPlacementOverride(t *testing.T) {
	banned := map[string]string{
		"hostOnly":    "--host-only rewrote cluster-declared services into host processes",
		"clusterOnly": "--cluster-only masked the host/frontend phases off",
		"hostMode":    "placement belongs in the env's KCL, not a flag",
		"forceHost":   "placement belongs in the env's KCL, not a flag",
	}
	typ := reflect.TypeOf(upOptions{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if why, bad := banned[name]; bad {
			t.Errorf("upOptions.%s reintroduces a CLI placement override: %s.\n"+
				"Scope a run with --target; declare placement with a `host` block "+
				"and select the runner with `-D host_runner=...`.", name, why)
		}
	}
}

// TestRunUsesTheWholeLoop pins what `forge run` became. It used to imply
// --host-only, which skipped the cluster build/deploy entirely and collapsed
// cluster services to `go run`. It is now an alias for `forge env up <env>`
// plus dev-server passthrough, so its help must not promise the old
// skip-the-cluster behaviour that no longer happens.
func TestRunUsesTheWholeLoop(t *testing.T) {
	cmd := newRunCmd()
	help := cmd.Short + "\n" + cmd.Long
	for _, stale := range []string{"--host-only", "--cluster-only", "skipping cluster build/deploy", "skipping the cluster build + deploy"} {
		if strings.Contains(help, stale) {
			t.Errorf("`forge run` help still advertises %q, which the command no longer does", stale)
		}
	}
	// The surviving way to narrow a run has to be discoverable from here:
	// this help is where someone lands after the flag they typed went away.
	if !strings.Contains(help, "--target") {
		t.Error("`forge run` help should point at --target as the way to scope a run to one service")
	}
}

// TestRunPassthroughArgs covers the `--`-terminator split `forge run` uses
// to separate dev-server passthrough from (disallowed) positional args.
func TestRunPassthroughArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		dashPos int
		want    []string
		wantErr bool
	}{
		{
			// `forge run` — no args, no `--`.
			name: "no args no dash", args: nil, dashPos: -1, want: nil,
		},
		{
			// `forge run -- --host 0.0.0.0` — cobra: args after `--`, dash at 0.
			name: "passthrough after dash", args: []string{"--host", "0.0.0.0"}, dashPos: 0,
			want: []string{"--host", "0.0.0.0"},
		},
		{
			// `forge run --` — dash present, nothing after it.
			name: "bare dash", args: nil, dashPos: 0, want: nil,
		},
		{
			// `forge run foo` — a stray positional with no `--` is a usage error.
			name: "positional no dash errors", args: []string{"foo"}, dashPos: -1, wantErr: true,
		},
		{
			// `forge run foo -- --host x` — positional before `--` is rejected.
			name: "positional before dash errors", args: []string{"foo", "--host", "x"}, dashPos: 1, wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := runPassthroughArgs(c.args, c.dashPos)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (got=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestBuildFrontendCmd_ForwardsPassthroughArgs pins the load-bearing piece
// of `forge run -- --host 0.0.0.0`: the tokens reach the frontend dev server
// as `npm run dev -- --host 0.0.0.0` (the `--` is required so npm forwards
// them to vite/next rather than consuming them itself).
func TestBuildFrontendCmd_ForwardsPassthroughArgs(t *testing.T) {
	fe := FrontendEntity{Name: "dashboard", Path: "frontends/dashboard", Port: 3000, EnvFile: "/does/not/exist"}
	cmd := buildFrontendCmd(context.Background(), fe, "dev", nil, []string{"--host", "0.0.0.0"}, "")

	want := []string{"run", "dev", "--", "--host", "0.0.0.0"}
	// cmd.Args[0] is the runner (npm); the rest are the run args.
	got := cmd.Args[1:]
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("frontend cmd args: got %v, want [npm %v]", cmd.Args, want)
	}
}

// TestBuildFrontendCmd_NoPassthroughUnchanged confirms the `forge env up`
// default (nil passthrough) leaves the command as the bare `npm run dev`
// — no trailing `--` that some dev servers choke on.
func TestBuildFrontendCmd_NoPassthroughUnchanged(t *testing.T) {
	fe := FrontendEntity{Name: "dashboard", Path: "frontends/dashboard", Port: 3000, EnvFile: "/does/not/exist"}
	cmd := buildFrontendCmd(context.Background(), fe, "dev", nil, nil, "")

	want := []string{"run", "dev"}
	got := cmd.Args[1:]
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("frontend cmd args: got %v, want [npm %v]", cmd.Args, want)
	}
}

// TestNewRunCmd_Surface pins the restored command's shape: name, the dev
// default env, and that `-- <flags>` parse into the post-dash passthrough
// (ArgsLenAtDash == 0) rather than being swallowed as flags.
func TestNewRunCmd_Surface(t *testing.T) {
	cmd := newRunCmd()
	if !strings.HasPrefix(cmd.Use, "run") {
		t.Errorf("Use: got %q, want prefix \"run\"", cmd.Use)
	}
	flag := cmd.Flags().Lookup("env")
	if flag == nil {
		t.Fatal("--env flag missing from forge run")
	}
	if flag.DefValue != "dev" {
		t.Errorf("--env default: got %q, want dev", flag.DefValue)
	}

	// `forge run -- --host 0.0.0.0`: the terminator must land the flags in
	// the positional args (post-dash), not error as unknown flags.
	if err := cmd.ParseFlags([]string{"--", "--host", "0.0.0.0"}); err != nil {
		t.Fatalf("parse `-- --host 0.0.0.0`: %v", err)
	}
	dash := cmd.ArgsLenAtDash()
	if dash != 0 {
		t.Errorf("ArgsLenAtDash: got %d, want 0", dash)
	}
	got, err := runPassthroughArgs(cmd.Flags().Args(), dash)
	if err != nil {
		t.Fatalf("runPassthroughArgs: %v", err)
	}
	if strings.Join(got, " ") != "--host 0.0.0.0" {
		t.Errorf("passthrough: got %v, want [--host 0.0.0.0]", got)
	}
}
