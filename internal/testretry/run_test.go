package testretry

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunnerRetriesOnlyTheUnchangedFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns Git and Go against an isolated module; runs in task test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("requires the same POSIX shell fixture as the merge runners")
	}
	for _, program := range []string{"git", "go"} {
		if _, err := exec.LookPath(program); err != nil {
			t.Skipf("%s unavailable: %v", program, err)
		}
	}
	root := t.TempDir()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-trimpath")
	t.Setenv("GOEXPERIMENT", "")
	t.Setenv("GOCOVERDIR", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.test")
	callGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := `package f
import("os";"testing")
func TestStable(t *testing.T) {
    f, err := os.OpenFile(os.Getenv("STABLE_FILE"),os.O_CREATE|os.O_EXCL,0600)
    if err != nil { t.Fatalf("stable test improperly repeated: %v", err) }; _=f.Close()
}
func TestOriginal(t *testing.T) {
    f, err := os.OpenFile(os.Getenv("ORIGINAL_FILE"),os.O_CREATE|os.O_EXCL,0600)
    if err == nil { _=f.Close(); t.Fatalf("the initial observed failure") }
}
`
	outside := t.TempDir()
	t.Setenv("STABLE_FILE", filepath.Join(outside, "stable"))
	t.Setenv("ORIGINAL_FILE", filepath.Join(outside, "original"))
	write("go.mod", "module example.com/fixture\n\ngo 1.27.0\n")
	write("f_test.go", fixture)
	callGit("init", "-qb", "main")
	callGit("add", ".")
	callGit("commit", "-qm", "base with unchanged test")
	base := callGit("rev-parse", "HEAD")
	callGit("commit", "-q", "--allow-empty", "-m", "candidate")
	var diagnostic bytes.Buffer
	cfg := Config{Root: root, Repository: "acme/lib", Base: base, Head: callGit("rev-parse", "HEAD"),
		RunID: "gate-123", PR: 12, Timeout: time.Minute, Packages: []string{"."}, Stderr: &diagnostic,
		JSONPath: filepath.Join(outside, "initial.ndjson"), ReportPath: filepath.Join(outside, "result.json")}
	if err := Run(t.Context(), cfg); err != nil {
		t.Fatalf("unexpected red: %v\n%s", err, &diagnostic)
	}
	content, err := os.ReadFile(cfg.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Tests) != 1 || report.Tests[0].Test != "TestOriginal" || report.Tests[0].SourceBlob != callGit("rev-parse", "HEAD:f_test.go") ||
		report.Head != cfg.Head || report.Base != cfg.Base || report.Tree != callGit("rev-parse", "HEAD^{tree}") {
		t.Fatalf("unbound or unexpected report: %s", content)
	}
	if !strings.Contains(diagnostic.String(), "retried:TestOriginal") || strings.Contains(diagnostic.String(), "retried:TestStable") {
		t.Fatalf("wrong retry set: %s", &diagnostic)
	}
	if _, err := Verify(t.Context(), cfg, report.Tree, "example.com/fixture"); err != nil {
		t.Fatalf("authentic proof refused by offline verification: %v", err)
	}
	wrongRun := cfg
	wrongRun.RunID = "other-job"
	if _, err := Verify(t.Context(), wrongRun, report.Tree, "example.com/fixture"); err == nil {
		t.Fatal("report replayed under another run")
	}
	report.Tests[0].File = "../../another_test.go"
	tampered, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ReportPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(t.Context(), cfg, report.Tree, "example.com/fixture"); err == nil {
		t.Fatal("tampered deletion path was accepted")
	}

	// The same initially green-able behavior must remain red when the exact
	// source changed in the candidate, even though a second run would pass.
	write("f_test.go", fixture+"\n// candidate edited the test file\n")
	callGit("add", "f_test.go")
	callGit("commit", "-qm", "edited test source")
	for _, name := range []string{"stable", "original"} {
		if err := os.Remove(filepath.Join(outside, name)); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Head = callGit("rev-parse", "HEAD")
	cfg.JSONPath += ".changed"
	cfg.ReportPath += ".changed"
	diagnostic.Reset()
	if err := Run(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "test source is not unchanged") {
		t.Fatalf("edited test did not stay red: %v\n%s", err, &diagnostic)
	}
	if strings.Contains(diagnostic.String(), "checking") {
		t.Fatalf("edited test was retried: %s", &diagnostic)
	}
	if _, err := os.Stat(cfg.ReportPath); !os.IsNotExist(err) {
		t.Fatalf("unexpected changed-test report: %v", err)
	}
}
