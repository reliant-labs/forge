package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/linter/contract"
)

func TestVettoolPrintsContractFixHints(t *testing.T) {
	if testing.Short() {
		t.Skip("builds contractlint and runs go vet with the built tool")
	}
	root := t.TempDir()
	tool := filepath.Join(root, "contractlint")
	if runtime.GOOS == "windows" {
		tool += ".exe"
	}
	if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", tool, ".").CombinedOutput(); err != nil {
		t.Fatalf("build vettool: %v\n%s", err, out)
	}
	for name, body := range map[string]string{
		"go.mod": "module example.com/demo\n\ngo 1.23\n",
		"internal/missing/service.go": `package missing

type Service struct{}
func (Service) Run() {}
`,
		"internal/extra/contract.go": `package extra

type Service interface { Run() }
`,
		"internal/extra/service.go": `package extra

type implementation struct{}
func (implementation) Run() {}
func (implementation) Extra() {}
`,
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "go", "vet", "-vettool="+tool, "./internal/...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("vettool accepted both violations:\n%s", out)
	}
	for _, want := range []string{
		"package missing has exported methods but no contract.go",
		"exported method Extra on type implementation is not declared",
		"Fix: " + contract.RequireContractFixHint,
		"Fix: " + contract.FixHint,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("vettool output lacks %q:\n%s", want, out)
		}
	}
}

// TestMain_AsChild lets the tests below run main itself: they re-execute this
// test binary as the contractlint command (main calls os.Exit through
// multichecker, so it cannot be called in process).
func TestMain_AsChild(t *testing.T) {
	if os.Getenv("CONTRACTLINT_AS_MAIN") != "1" {
		t.Skip("only meaningful as the child of runContractlint")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Args = append([]string{"contractlint"}, args...)
	main()
	os.Exit(0)
}

func runContractlint(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^TestMain_AsChild$", "--"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CONTRACTLINT_AS_MAIN=1", "GOFLAGS=-mod=mod", "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("start contractlint: %v", err)
	}
	return string(out), code
}

// The top-level -exclude flag, in both spellings and comma-separated, removes
// packages from the contract checks; without it the same tree is reported.
func TestExcludeFlagSilencesTheNamedPackages(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":                      "module example.com/demo\n\ngo 1.23\n",
		"internal/missing/service.go": "package missing\n\ntype Service struct{}\nfunc (Service) Run() {}\n",
		"internal/also/service.go":    "package also\n\ntype Service struct{}\nfunc (Service) Run() {}\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, code := runContractlint(t, root, "./internal/...")
	if code == 0 || !strings.Contains(out, "package missing has exported methods but no contract.go") ||
		!strings.Contains(out, "package also has exported methods but no contract.go") {
		t.Fatalf("without -exclude: exit %d, want both packages reported:\n%s", code, out)
	}

	for name, args := range map[string][]string{
		"equals form": {"-exclude=internal/missing,internal/also", "./internal/..."},
		"space form":  {"-exclude", "internal/missing,internal/also", "./internal/..."},
	} {
		if out, code := runContractlint(t, root, args...); code != 0 || strings.Contains(out, "no contract.go") {
			t.Errorf("%s: exit %d, want a clean run with both packages excluded:\n%s", name, code, out)
		}
	}

	out, code = runContractlint(t, root, "-exclude=internal/missing", "./internal/...")
	if code == 0 || strings.Contains(out, "package missing") || !strings.Contains(out, "package also has exported methods") {
		t.Errorf("excluding one package: exit %d, want only the other reported:\n%s", code, out)
	}
}
