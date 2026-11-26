package hostlaunch

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestWithDevLinkFlags: the runners that compile on launch link without
// DWARF (-ldflags=-w) unless FORGE_DEV_DWARF=1 or an -ldflags somebody
// declared decides it; a declared -X is never dropped, and go-run merges -w
// into a declared value that air could not receive whole.
func TestWithDevLinkFlags(t *testing.T) {
	dir := t.TempDir()
	write := func(name, cmd string) string {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("[build]\ncmd = \""+cmd+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return name
	}
	plainAir := write("plain.toml", "go build $FORGE_GO_BUILD_FLAGS -o ./tmp/app ./cmd/app")
	ownLdflags := write("own.toml", "go build -ldflags '-X main.v=dev' $FORGE_GO_BUILD_FLAGS -o ./tmp/app ./cmd/app")
	scriptAir := write("script.toml", "scripts/air-build.sh ./cmd/app ./tmp/app $FORGE_GO_BUILD_FLAGS")
	psAir := write("ps.toml", "$f = @($env:FORGE_GO_BUILD_FLAGS -split ' '); go build @f -o ./tmp/app.exe ./cmd/app")
	debugAir := write("debug.toml", "go build -gcflags='all=-N -l' -trimpath=false $FORGE_GO_BUILD_FLAGS -o ./tmp/app ./cmd/app")

	goRun := func(flags ...string) RunnerSpec { return RunnerSpec{GoPkg: "./cmd/app", GoBuildFlags: flags} }
	air := func(cfg string, flags ...string) RunnerSpec {
		return RunnerSpec{Runner: "air", AirConfig: cfg, ProjectDir: dir, GoPkg: "./cmd/app", GoBuildFlags: flags}
	}
	cases := []struct {
		name    string
		spec    RunnerSpec
		env     []string
		goflags string
		want    []string
	}{
		{"go-run appends", goRun("-trimpath"), nil, "", []string{"-trimpath", "-ldflags=-w"}},
		{"go-run with no declared flags", goRun(), nil, "", []string{"-ldflags=-w"}},
		{"air appends", air(plainAir, "-trimpath"), nil, "", []string{"-trimpath", "-ldflags=-w"}},
		{"FORGE_DEV_DWARF=1 keeps DWARF", goRun("-trimpath"), []string{"FORGE_DEV_DWARF=1"}, "", []string{"-trimpath"}},
		{"FORGE_DEV_DWARF=1 keeps DWARF under air", air(plainAir, "-trimpath"), []string{"FORGE_DEV_DWARF=1"}, "", []string{"-trimpath"}},
		{"only FORGE_DEV_DWARF=1 opts out", goRun(), []string{"FORGE_DEV_DWARF=true"}, "", []string{"-ldflags=-w"}},
		{"FORGE_DEV_DWARF=0 does not", goRun(), []string{"FORGE_DEV_DWARF=0"}, "", []string{"-ldflags=-w"}},
		{"the last FORGE_DEV_DWARF wins", goRun(), []string{"FORGE_DEV_DWARF=1", "FORGE_DEV_DWARF=0"}, "", []string{"-ldflags=-w"}},
		{"GOFLAGS -ldflags decides", goRun("-trimpath"), nil, "-trimpath -ldflags=-X=main.v=dev", []string{"-trimpath"}},
		{"GOFLAGS --ldflags decides", air(plainAir), nil, "--ldflags=-s", nil},
		{"GOFLAGS without -ldflags", goRun(), nil, "-trimpath -mod=mod", []string{"-ldflags=-w"}},
		{"go-run merges into a declared -X", goRun("-trimpath", "-ldflags=-X main.v=dev"), nil, "",
			[]string{"-trimpath", "-ldflags=-X main.v=dev -w"}},
		{"go-run merges the two-argument form", goRun("-ldflags", "-X main.v=dev"), nil, "",
			[]string{"-ldflags", "-X main.v=dev -w"}},
		{"go-run merges a per-package pattern", goRun("-ldflags=all=-X main.v=dev"), nil, "",
			[]string{"-ldflags=all=-X main.v=dev -w"}},
		{"a declared -w is kept", goRun("-ldflags=-s -w"), nil, "", []string{"-ldflags=-s -w"}},
		{"a declared -w=false keeps DWARF", goRun("-ldflags=-w=false"), nil, "", []string{"-ldflags=-w=false"}},
		{"several -ldflags are go's to resolve", goRun("-ldflags=-X a=b", "-ldflags=main=-X c=d"), nil, "",
			[]string{"-ldflags=-X a=b", "-ldflags=main=-X c=d"}},
		{"a dangling -ldflags is go's to report", goRun("-ldflags"), nil, "", []string{"-ldflags"}},
		{"air keeps a declared -ldflags whole", air(plainAir, "-ldflags=-X=main.v=dev"), nil, "", []string{"-ldflags=-X=main.v=dev"}},
		{"an air debug config keeps DWARF", air(debugAir, "-trimpath"), nil, "", []string{"-trimpath"}},
		{"a debug build's flags keep DWARF", goRun("-gcflags=all=-N -l"), nil, "", []string{"-gcflags=all=-N -l"}},
		{"air through a script gets nothing", air(scriptAir, "-trimpath"), nil, "", []string{"-trimpath"}},
		{"air with an unreadable config gets nothing", air("missing.toml", "-trimpath"), nil, "", []string{"-trimpath"}},
		{"air's PowerShell go build", air(psAir, "-trimpath"), nil, "", []string{"-trimpath", "-ldflags=-w"}},
		{"air config with its own -ldflags", air(ownLdflags, "-trimpath"), nil, "", []string{"-trimpath"}},
		{"the user's FORGE_GO_BUILD_FLAGS wins under air", air(plainAir, "-trimpath"), []string{"FORGE_GO_BUILD_FLAGS=-race"}, "", []string{"-trimpath"}},
		{"binary runner", RunnerSpec{Runner: "binary", GoPkg: "./cmd/app"}, nil, "", nil},
		{"delve runner", RunnerSpec{Runner: "delve", GoPkg: "./cmd/app"}, nil, "", nil},
		{"verbatim command", RunnerSpec{Command: []string{"go", "run", "../sib/cmd/x"}}, nil, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := slices.Clone(c.spec.GoBuildFlags)
			got := WithDevLinkFlags(c.spec, c.env, c.goflags)
			if !slices.Equal(got.GoBuildFlags, c.want) {
				t.Errorf("GoBuildFlags = %q, want %q", got.GoBuildFlags, c.want)
			}
			if !slices.Equal(c.spec.GoBuildFlags, in) {
				t.Errorf("WithDevLinkFlags modified its input: %q, was %q", c.spec.GoBuildFlags, in)
			}
		})
	}
}

// TestDevLinkFlagReachesAir: what air's build gets in $FORGE_GO_BUILD_FLAGS
// (the contract a project's own air build script reads): the declared flags,
// then -ldflags=-w, space-separated and each free of whitespace.
func TestDevLinkFlagReachesAir(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".air.toml")
	if err := os.WriteFile(cfg, []byte("[build]\ncmd = \"forge storage check ./tmp && CGO_ENABLED=0 go build $FORGE_GO_BUILD_FLAGS -buildvcs=false -o ./tmp/app.tmp ./cmd/app\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := WithDevLinkFlags(RunnerSpec{Runner: "air", AirConfig: cfg, GoBuildFlags: []string{"-trimpath"}}, nil, "")
	env, dropped := WithGoBuildFlags(spec, nil)
	if want := []string{"FORGE_GO_BUILD_FLAGS=-trimpath -ldflags=-w"}; !slices.Equal(env, want) || len(dropped) > 0 {
		t.Errorf("env = %q (dropped %q), want %q", env, dropped, want)
	}
	cmd, err := BuildCmd(t.Context(), "api", WithDevLinkFlags(RunnerSpec{GoPkg: "./cmd/app", GoBuildFlags: []string{"-trimpath"}, Args: []string{"server"}}, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"go", "run", "-trimpath", "-ldflags=-w", "./cmd/app", "server"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("go-run argv = %q, want %q", cmd.Args, want)
	}
}
