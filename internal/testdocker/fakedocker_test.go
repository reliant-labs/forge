package testdocker

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The test binary doubles as a fake `docker` CLI: installFakeDocker copies it
// to <dir>/docker(.exe) and puts <dir> first on PATH. When TestMain finds it
// was invoked under that name it records its argv and answers from a script,
// so the code that shells out to docker is exercised end to end without a
// daemon (and on every OS).

const (
	fakeLogEnv    = "TESTDOCKER_FAKE_LOG"
	fakeScriptEnv = "TESTDOCKER_FAKE_SCRIPT"
)

// fakeReply is what the fake docker prints and exits with for one subcommand.
type fakeReply struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
}

func TestMain(m *testing.M) {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if base == "docker" {
		os.Exit(runFakeDocker(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeKey names the subcommand a reply is registered under: "port", "ps",
// "rm", or "network create" / "network ls" / "network rm".
func fakeKey(args []string) string {
	if len(args) > 1 && args[0] == "network" {
		return "network " + args[1]
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func runFakeDocker(args []string) int {
	if logPath := os.Getenv(fakeLogEnv); logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			line, _ := json.Marshal(args)
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	var script map[string]fakeReply
	if data, err := os.ReadFile(os.Getenv(fakeScriptEnv)); err == nil {
		_ = json.Unmarshal(data, &script)
	}
	r := script[fakeKey(args)]
	os.Stdout.WriteString(r.Stdout)
	os.Stderr.WriteString(r.Stderr)
	return r.Exit
}

// fakeDocker is an installed fake docker: calls() reads back what ran.
type fakeDocker struct{ log string }

// installFakeDocker puts a fake docker first on PATH for the test, answering
// each subcommand from script (an unlisted one prints nothing and exits 0).
// The caller must not be parallel: it sets environment variables.
func installFakeDocker(t *testing.T, script map[string]fakeReply) *fakeDocker {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	dir := t.TempDir()
	name := "docker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	src, err := os.Open(self)
	if err != nil {
		t.Skipf("cannot read the test binary: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(dir, "script.json")
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{log: filepath.Join(dir, "calls.jsonl")}
	t.Setenv(fakeLogEnv, f.log)
	t.Setenv(fakeScriptEnv, scriptPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// calls returns every recorded docker invocation's argv, in order.
func (f *fakeDocker) calls(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("bad fake docker log line %q: %v", line, err)
		}
		out = append(out, args)
	}
	return out
}

// find returns the first recorded call whose argv starts with prefix.
func (f *fakeDocker) find(t *testing.T, prefix ...string) []string {
	t.Helper()
	for _, c := range f.calls(t) {
		if len(c) >= len(prefix) && strings.Join(c[:len(prefix)], " ") == strings.Join(prefix, " ") {
			return c
		}
	}
	return nil
}
