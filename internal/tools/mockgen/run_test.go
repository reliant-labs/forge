package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests below drive run directly over a throwaway project: mockgen only
// parses and writes source, so none of them builds anything.

const forgeYAML = "name: demo\nmodule_path: example.com/demo\n"

func mockPath(root, pkg string) string {
	return filepath.Join(root, "internal", filepath.FromSlash(pkg), "mock_gen.go")
}

func runIn(t *testing.T, root string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(root, &out)
	return out.String(), err
}

// A first run writes a mock for each contract and reports each one; a second
// run over unchanged contracts rewrites nothing and says so.
func TestRunWritesMocksAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "greeter", "contract.go"), greeterContract(false))

	out, err := runIn(t, root)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	for _, want := range []string{"mockgen: refreshed internal/greeter/mock_gen.go", "mockgen: 1 contract mock(s), 1 refreshed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	mock, err := os.ReadFile(mockPath(root, "greeter"))
	if err != nil {
		t.Fatalf("no mock written: %v", err)
	}
	if !strings.Contains(string(mock), "func (m *MockService) Greet(") {
		t.Errorf("mock lacks Greet:\n%s", mock)
	}

	out, err = runIn(t, root)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(out, "1 contract mock(s), 0 refreshed") || strings.Contains(out, "refreshed internal/") {
		t.Errorf("an unchanged contract was reported as refreshed:\n%s", out)
	}
}

// A contract that grows a method gets its mock refreshed, and only that one.
func TestRunRefreshesOnlyTheMockWhoseContractChanged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "greeter", "contract.go"), greeterContract(false))
	writeFile(t, filepath.Join(root, "internal", "other", "contract.go"), strings.ReplaceAll(greeterContract(false), "package greeter", "package other"))
	if out, err := runIn(t, root); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	writeFile(t, filepath.Join(root, "internal", "greeter", "contract.go"), greeterContract(true))
	out, err := runIn(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "refreshed internal/greeter/mock_gen.go") || strings.Contains(out, "refreshed internal/other/") {
		t.Errorf("wrong packages refreshed:\n%s", out)
	}
	if !strings.Contains(out, "2 contract mock(s), 1 refreshed") {
		t.Errorf("summary wrong:\n%s", out)
	}
	if mock, _ := os.ReadFile(mockPath(root, "greeter")); !strings.Contains(string(mock), "Farewell") {
		t.Errorf("the refreshed mock lacks the new method:\n%s", mock)
	}
}

// A package opted out with //forge:exclude-contract gets no mock, and forge.yaml's
// contracts.exclude does the same for a path.
func TestRunSkipsExcludedPackages(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "forge.yaml"), forgeYAML+"contracts:\n  exclude:\n    - internal/skipped\n")
	writeFile(t, filepath.Join(root, "internal", "kept", "contract.go"), strings.ReplaceAll(greeterContract(false), "package greeter", "package kept"))
	writeFile(t, filepath.Join(root, "internal", "skipped", "contract.go"), strings.ReplaceAll(greeterContract(false), "package greeter", "package skipped"))
	writeFile(t, filepath.Join(root, "internal", "directive", "contract.go"),
		"//forge:exclude-contract: not an injected component\n"+strings.ReplaceAll(greeterContract(false), "package greeter", "package directive"))

	out, err := runIn(t, root)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 contract mock(s)") {
		t.Errorf("summary = %q, want exactly one mock", out)
	}
	if _, err := os.Stat(mockPath(root, "kept")); err != nil {
		t.Errorf("the included package got no mock: %v", err)
	}
	for _, pkg := range []string{"skipped", "directive"} {
		if _, err := os.Stat(mockPath(root, pkg)); !os.IsNotExist(err) {
			t.Errorf("excluded package %s got a mock (stat err %v)", pkg, err)
		}
	}
}

// With no internal/ directory there is nothing to do, and that is not an
// error: the tool runs from `task generate` in any checkout.
func TestRunWithNoContractsReportsZero(t *testing.T) {
	out, err := runIn(t, t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "mockgen: 0 contract mock(s), 0 refreshed") {
		t.Errorf("output = %q", out)
	}
}

// A forge.yaml that does not parse stops the run before any mock is written.
func TestRunRejectsAnUnreadableForgeYAML(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "forge.yaml"), "name: [unterminated\n")
	writeFile(t, filepath.Join(root, "internal", "greeter", "contract.go"), greeterContract(false))

	out, err := runIn(t, root)
	if err == nil || !strings.Contains(err.Error(), "load forge.yaml") {
		t.Fatalf("run = %v, want a \"load forge.yaml\" error", err)
	}
	if out != "" {
		t.Errorf("output before the failure: %q", out)
	}
	if _, err := os.Stat(mockPath(root, "greeter")); !os.IsNotExist(err) {
		t.Error("a mock was written despite the forge.yaml error")
	}
}

// A contract that does not compile as Go stops the run with the package named.
func TestRunNamesThePackageWhoseContractCannotBeRead(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "broken", "contract.go"), "package broken\n\ntype Service interface {\n")

	_, err := runIn(t, root)
	if err == nil || !strings.Contains(err.Error(), "generate mock for internal/broken") {
		t.Fatalf("run = %v, want an error naming internal/broken", err)
	}
}

// A //forge:contract type has its contract derived into contract_gen.go before
// its mock is written, so the mock sees the type's current methods.
func TestRunDerivesAContractFromAForgeContractType(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "notify", "service.go"), `package notify

// service sends notices.
//
//forge:contract
type service struct{}

// Send sends one notice.
func (s *service) Send(to string) error { return nil }
`)
	out, err := runIn(t, root)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	derived, err := os.ReadFile(filepath.Join(root, "internal", "notify", "contract_gen.go"))
	if err != nil {
		t.Fatalf("contract_gen.go not derived: %v", err)
	}
	if !strings.Contains(string(derived), "Send(to string) error") {
		t.Errorf("derived contract lacks Send:\n%s", derived)
	}
	if mock, err := os.ReadFile(mockPath(root, "notify")); err != nil || !strings.Contains(string(mock), "Send(") {
		t.Errorf("mock for the derived contract: err %v\n%s", err, mock)
	}
}

// mockgenAsChild re-executes this test binary as the mockgen command, so main
// itself (flag parsing, exit codes, stdout/stderr) is tested without a go build.
func mockgenAsChild(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestMain_AsChild$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "MOCKGEN_AS_MAIN=1")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("start child: %v", err)
	}
	return so.String(), se.String(), exit
}

// TestMain_AsChild is the child half of mockgenAsChild; run on its own it does nothing.
func TestMain_AsChild(t *testing.T) {
	if os.Getenv("MOCKGEN_AS_MAIN") != "1" {
		t.Skip("only meaningful as the child of mockgenAsChild")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Args = append([]string{"mockgen"}, args...)
	main()
	os.Exit(0)
}

func TestMainPrintsASummaryAndExitsZero(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal", "greeter", "contract.go"), greeterContract(false))
	stdout, stderr, code := mockgenAsChild(t, "-root", root)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "mockgen: 1 contract mock(s), 1 refreshed") {
		t.Errorf("stdout = %q", stdout)
	}
	if _, err := os.Stat(mockPath(root, "greeter")); err != nil {
		t.Errorf("no mock written: %v", err)
	}
}

func TestMainFailureExitsOneWithTheErrorOnStderr(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "forge.yaml"), "name: [unterminated\n")
	stdout, stderr, code := mockgenAsChild(t, "-root", root)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.HasPrefix(stderr, "mockgen: load forge.yaml") {
		t.Errorf("stderr = %q, want it to start with the load error", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing on failure", stdout)
	}
}

func TestMainRejectsAnUnknownFlag(t *testing.T) {
	_, stderr, code := mockgenAsChild(t, "-no-such-flag")
	if code != 2 {
		t.Errorf("exit = %d, want flag's usage exit 2", code)
	}
	if !strings.Contains(stderr, "no-such-flag") || !strings.Contains(stderr, "-root") {
		t.Errorf("stderr = %q, want the bad flag named and usage listing -root", stderr)
	}
}
