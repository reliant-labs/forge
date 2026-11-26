package otelcoltest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestImageIsPinnedByDigest(t *testing.T) {
	if !strings.Contains(Image, "opentelemetry-collector-contrib:0.162.0@sha256:") {
		t.Errorf("Image = %q: the collector must be pinned by version AND digest", Image)
	}
}

func TestCommand_BinaryRunsDirectly(t *testing.T) {
	ctx := context.Background()
	c := Collector{Binary: "/opt/otelcol-contrib"}

	cmd := Command(ctx, c, "/work", "ignored-name", nil, "validate", "--config=/work/c.yaml")
	if cmd.Path != "/opt/otelcol-contrib" && filepath.Base(cmd.Path) != "otelcol-contrib" {
		t.Errorf("Path = %q, want the binary", cmd.Path)
	}
	if want := []string{"/opt/otelcol-contrib", "validate", "--config=/work/c.yaml"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if cmd.Env != nil {
		t.Errorf("with no extra environment the child inherits ours unchanged, got %d entries", len(cmd.Env))
	}

	cmd = Command(ctx, c, "/work", "n", []string{"TOKEN=abc", "OTHER=1"}, "run")
	if !slices.Contains(cmd.Env, "TOKEN=abc") || !slices.Contains(cmd.Env, "OTHER=1") {
		t.Errorf("Env lacks the requested variables: %v", cmd.Env)
	}
	if len(cmd.Env) <= 2 {
		t.Errorf("the requested variables must be ADDED to the inherited environment, got %d entries", len(cmd.Env))
	}
}

func TestCommand_DockerRunsTheImageAsTheCallingUser(t *testing.T) {
	ctx := context.Background()
	cmd := Command(ctx, Collector{}, "/tmp/cfg", "otelcol-test", []string{"TOKEN=abc"}, "--config=/tmp/cfg/c.yaml")

	if filepath.Base(cmd.Args[0]) != "docker" {
		t.Fatalf("Args[0] = %q, want docker", cmd.Args[0])
	}
	args := cmd.Args[1:]
	if args[0] != "run" || !slices.Contains(args, "--rm") {
		t.Errorf("args = %v, want `run --rm`", args)
	}
	for _, pair := range [][2]string{{"--name", "otelcol-test"}, {"-v", "/tmp/cfg:/tmp/cfg"}, {"-e", "TOKEN=abc"}} {
		i := slices.Index(args, pair[0])
		if i < 0 || args[i+1] != pair[1] {
			t.Errorf("args %v lack %s %s", args, pair[0], pair[1])
		}
	}
	if runtime.GOOS != "windows" {
		want := itoa(os.Getuid()) + ":" + itoa(os.Getgid())
		if i := slices.Index(args, "--user"); i < 0 || args[i+1] != want {
			t.Errorf("args %v: the container must run as %s so files it writes are the caller's", args, want)
		}
	}
	if slices.Contains(args, "--network") {
		t.Errorf("host networking is opt-in, got %v", args)
	}
	// The image comes after every docker flag and before the collector's own arguments.
	img := slices.Index(args, Image)
	if img < 0 || args[len(args)-1] != "--config=/tmp/cfg/c.yaml" || img != len(args)-2 {
		t.Errorf("args = %v, want the image directly before the collector's arguments", args)
	}
}

func TestCommand_HostNetwork(t *testing.T) {
	cmd := Command(context.Background(), Collector{HostNetwork: true}, "/d", "n", nil, "run")
	args := cmd.Args
	i := slices.Index(args, "--network")
	if i < 0 || args[i+1] != "host" {
		t.Errorf("args = %v, want --network host", args)
	}
	if slices.Index(args, Image) < i {
		t.Errorf("--network must come before the image or docker hands it to the collector: %v", args)
	}
	// Host networking is a docker flag; a local binary already shares the host's network.
	bin := Command(context.Background(), Collector{Binary: "/b", HostNetwork: true}, "/d", "n", nil, "run")
	if slices.Contains(bin.Args, "--network") {
		t.Errorf("a binary run has no docker flags: %v", bin.Args)
	}
}

func TestFind_PrefersABinaryOnPath(t *testing.T) {
	dir := t.TempDir()
	name := "otelcol-contrib"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	c := Find(t)
	if c.Binary != bin {
		t.Errorf("Find = %+v, want the binary %s", c, bin)
	}
	if c.HostNetwork {
		t.Error("Find never opts into host networking")
	}
}

func TestFind_SkipsWhenThereIsNothingToRun(t *testing.T) {
	if os.Getenv("OTELCOLTEST_FIND_CHILD") == "1" {
		t.Setenv("PATH", t.TempDir())
		Find(t)
		t.Fatal("Find returned a collector though neither a binary nor docker exists")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFind_SkipsWhenThereIsNothingToRun$", "-test.v")
	cmd.Env = append(os.Environ(), "OTELCOLTEST_FIND_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the child must skip, not fail: %v\n%s", err, out)
	}
	for _, want := range []string{"--- SKIP", "no collector to run", "otelcol-contrib", "docker info"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("child output lacks %q:\n%s", want, out)
		}
	}
}

func TestDockerUsable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := dockerUsable()
	if err == nil || !strings.Contains(err.Error(), "docker info") {
		t.Errorf("with no docker on PATH, dockerUsable = %v, want an error naming `docker info`", err)
	}
}

func itoa(n int) string { return fmt.Sprint(n) }
