package connectv2test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sampleGoMod = `module github.com/reliant-labs/forge

go 1.25.0

toolchain go1.25.1

require github.com/direct/single v1.0.0

require (
	connectrpc.com/connect/v2 v2.0.2 // indirect
	github.com/a/b v1.2.3
	github.com/c/d v0.1.0 // indirect
)

require (
	github.com/e/f v2.0.0+incompatible
)

replace github.com/a/b => ../b
`

func TestRequireBlock(t *testing.T) {
	got := requireBlock([]byte(sampleGoMod))
	want := "require github.com/direct/single v1.0.0\n" +
		"require (\n" +
		"\tconnectrpc.com/connect/v2 v2.0.2 \n" +
		"\tgithub.com/a/b v1.2.3\n" +
		"\tgithub.com/c/d v0.1.0 // indirect\n" +
		")\n" +
		"require (\n" +
		"\tgithub.com/e/f v2.0.0+incompatible\n" +
		")\n"
	if got != want {
		t.Errorf("requireBlock =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "connect/v2 v2.0.2 //") {
		t.Error("connect/v2 must be a direct requirement: connectmajor reads an `// indirect` one as no v2 project")
	}
	for _, leaked := range []string{"module ", "replace ", "toolchain", "go 1.25"} {
		if strings.Contains(got, leaked) {
			t.Errorf("requireBlock leaked %q from the source go.mod:\n%s", leaked, got)
		}
	}
}

func TestRequireBlockOfAModuleWithNoRequirements(t *testing.T) {
	if got := requireBlock([]byte("module x\n\ngo 1.25.0\n")); got != "" {
		t.Errorf("requireBlock = %q, want empty", got)
	}
}

func TestWriteCreatesParentDirectories(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "c.txt")
	write(t, p, "hello")
	if b, err := os.ReadFile(p); err != nil || string(b) != "hello" {
		t.Fatalf("read back = %q, %v", b, err)
	}
	write(t, p, "replaced")
	if b, _ := os.ReadFile(p); string(b) != "replaced" {
		t.Errorf("a second write replaces the file, got %q", b)
	}
}

func TestModuleWriteIsRelativeToTheModule(t *testing.T) {
	m := &Module{Dir: t.TempDir(), Path: "example.com/x"}
	m.Write(t, filepath.Join("internal", "x.go"), "package x\n")
	if b, err := os.ReadFile(filepath.Join(m.Dir, "internal", "x.go")); err != nil || string(b) != "package x\n" {
		t.Errorf("read back = %q, %v", b, err)
	}
}

func TestRunReturnsCombinedOutputAndPinsTheModuleEnvironment(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	dir := t.TempDir()
	out := run(t, dir, "go", "env", "GOFLAGS", "GOPROXY", "GOWORK")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "-mod=mod") || !strings.Contains(lines[0], "-trimpath") || lines[1] != "off" || lines[2] != "off" {
		t.Errorf("go env GOFLAGS GOPROXY GOWORK = %q: want -mod=mod and -trimpath, GOPROXY=off, GOWORK=off", lines)
	}
	m := &Module{Dir: dir}
	if got := m.Go(t, "env", "GOPROXY"); strings.TrimSpace(got) != "off" {
		t.Errorf("Module.Go ran with GOPROXY=%q", got)
	}
}

// run fails the calling test with the command line and its output; a helper
// that reports through t.Fatalf is checked by running it on a probe test.
func TestRunFailureNamesTheCommandAndShowsItsOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a sub-process of the test binary; runs in task test")
	}
	if os.Getenv("CONNECTV2TEST_PROBE") == "1" {
		run(t, t.TempDir(), "go", "definitely-not-a-subcommand")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate the test binary: " + err.Error())
	}
	cmd := exec.Command(exe, "-test.run", "^TestRunFailureNamesTheCommandAndShowsItsOutput$")
	cmd.Env = append(os.Environ(), "CONNECTV2TEST_PROBE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the probe passed; a failing command must fail the test:\n%s", out)
	}
	for _, want := range []string{"go definitely-not-a-subcommand:", "unknown command"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("failure output lacks %q:\n%s", want, out)
		}
	}
}

func TestForgeRootIsTheModuleHoldingThisPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("runs `go env`; runs in task test")
	}
	root := forgeRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.HasPrefix(string(b), "module github.com/reliant-labs/forge\n") {
		t.Errorf("forgeRoot = %s: go.mod = %.60q, %v", root, b, err)
	}
}

const itemsProto = `syntax = "proto3";

package items.v1;

option go_package = "example.com/v2app/gen/services/items/v1;itemsv1";

message PingRequest {}
message PingResponse { string pong = 1; }

service ItemsService {
  rpc Ping(PingRequest) returns (PingResponse);
}
`

// NewModule generates real stubs; the module then compiles code that uses the
// generated connect-go v2 handler interface.
func TestNewModuleGeneratesStubsThatBuild(t *testing.T) {
	mod := NewModule(t, "example.com/v2app", map[string]string{"services/items/v1/items.proto": itemsProto})
	if mod.Path != "example.com/v2app" || mod.Dir == "" {
		t.Fatalf("module = %+v", mod)
	}

	gomod, err := os.ReadFile(filepath.Join(mod.Dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	root := forgeRoot(t)
	for _, want := range []string{"module example.com/v2app\n", "require github.com/reliant-labs/forge v0.0.0", "replace github.com/reliant-labs/forge => " + root} {
		if !strings.Contains(string(gomod), want) {
			t.Errorf("go.mod lacks %q:\n%s", want, gomod)
		}
	}
	if _, err := os.Stat(filepath.Join(mod.Dir, "go.sum")); err != nil {
		t.Errorf("go.sum was not copied: %v", err)
	}
	for _, rel := range []string{"buf.yaml", "buf.gen.yaml", filepath.Join("proto", "services", "items", "v1", "items.proto"),
		filepath.Join("gen", "services", "items", "v1", "items.pb.go")} {
		if _, err := os.Stat(filepath.Join(mod.Dir, rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mod.Dir, "gen", "services", "items", "v1", "itemsv1connect", "items.connect.go")); err != nil {
		t.Errorf("no connect-go stub generated: %v", err)
	}

	mod.Write(t, "use.go", `package v2app

import (
	itemsv1 "example.com/v2app/gen/services/items/v1"
	"example.com/v2app/gen/services/items/v1/itemsv1connect"
)

var (
	_ = itemsv1.PingRequest{}
	_ itemsv1connect.ItemsServiceHandler
)
`)
	mod.Go(t, "build", "./...")
}
