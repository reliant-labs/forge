package frontenddeps

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The test binary doubles as a fake npm/pnpm/yarn: installFakePM copies it to
// <dir>/<name>(.exe) and puts <dir> first on PATH. Invoked under that name,
// TestMain logs its argv and answers from environment variables, so the code
// that execs the package manager is exercised end to end on every OS.
//
//	FAKEPM_LOG         file the argv of every call is appended to (JSON lines)
//	FAKEPM_STDOUT/ERR  what the call prints
//	FAKEPM_EXIT        exit code (default 0)
//	FAKEPM_FAIL_FIRST  the first N calls fail with exit 1 and FAKEPM_FAIL_MSG
//	FAKEPM_COUNT       file counting calls, for FAKEPM_FAIL_FIRST
//	FAKEPM_NO_NODE_MODULES  skip creating node_modules (an install that failed early)
func TestMain(m *testing.M) {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	switch base {
	case "npm", "pnpm", "yarn":
		os.Exit(runFakePM(base, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runFakePM(name string, args []string) int {
	if p := os.Getenv("FAKEPM_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			cwd, _ := os.Getwd()
			line, _ := json.Marshal(append([]string{name, cwd}, args...))
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	// A real install populates node_modules; the stamp lands inside it.
	if os.Getenv("FAKEPM_NO_NODE_MODULES") == "" {
		_ = os.MkdirAll("node_modules", 0o755)
	}
	os.Stdout.WriteString(os.Getenv("FAKEPM_STDOUT"))
	os.Stderr.WriteString(os.Getenv("FAKEPM_STDERR"))
	if n, _ := strconv.Atoi(os.Getenv("FAKEPM_FAIL_FIRST")); n > 0 {
		counter := os.Getenv("FAKEPM_COUNT")
		b, _ := os.ReadFile(counter)
		calls, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		calls++
		_ = os.WriteFile(counter, []byte(strconv.Itoa(calls)), 0o644)
		if calls <= n {
			os.Stderr.WriteString(os.Getenv("FAKEPM_FAIL_MSG"))
			return 1
		}
		return 0
	}
	code, _ := strconv.Atoi(os.Getenv("FAKEPM_EXIT"))
	return code
}

type fakePM struct{ log string }

// installFakePM puts fake package managers named names first on PATH.
func installFakePM(t *testing.T, names ...string) *fakePM {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	dir := t.TempDir()
	for _, name := range names {
		file := name
		if runtime.GOOS == "windows" {
			file += ".exe"
		}
		src, err := os.Open(self)
		if err != nil {
			t.Skipf("cannot read the test binary: %v", err)
		}
		dst, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_WRONLY, 0o755)
		if err != nil {
			src.Close()
			t.Fatal(err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			t.Fatal(err)
		}
		src.Close()
		if err := dst.Close(); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakePM{log: filepath.Join(dir, "calls.jsonl")}
	t.Setenv("FAKEPM_LOG", f.log)
	t.Setenv("FAKEPM_COUNT", filepath.Join(dir, "count"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// An inherited proxy would add concurrency flags to every call.
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "FORGE_SKIP_NPM_INSTALL"} {
		t.Setenv(k, "")
	}
	return f
}

// calls returns every recorded invocation: [name, cwd, args...].
func (f *fakePM) calls(t *testing.T) [][]string {
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
		var c []string
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}
