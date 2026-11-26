package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writePkg(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"`+name+`","version":"`+version+`","main":"index.js","files":["index.js"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("module.exports = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/@acme%2flib" {
			_, _ = io.WriteString(w, `{"name":"@acme/lib","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDirsFlagAccumulates(t *testing.T) {
	var d dirs
	for _, v := range []string{"a", "b"} {
		if err := d.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(d) != 2 || d[0] != "a" || d[1] != "b" {
		t.Errorf("dirs = %v, want [a b]", []string(d))
	}
	if got := d.String(); got != "a,b" {
		t.Errorf("String() = %q, want a,b", got)
	}
}

// run reports a checkout it cannot read, or whose npm pack fails, before it
// writes the routing environment: a CI step that starts the overlay must fail
// there, not at a later install.
func TestRunFailsBeforeWritingTheEnvFile(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	for name, tc := range map[string]struct {
		packs []string
		want  string
	}{
		"no package.json": {[]string{t.TempDir()}, "package.json"},
		"unscoped":        {[]string{writePkg(t, "unscoped", "1.0.0")}, "not a scoped package"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(tc.packs, envFile, "http://127.0.0.1:1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %v, want an error containing %q", err, tc.want)
			}
			if _, err := os.Stat(envFile); !os.IsNotExist(err) {
				t.Errorf("env file written despite the failure (stat err %v)", err)
			}
		})
	}
}

func TestRunReportsAFailedPack(t *testing.T) {
	if testing.Short() {
		t.Skip("runs npm pack; runs in task test")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}
	dir := writePkg(t, "@acme/lib", "1.0.0")
	// npm pack fails on a package whose "files" entry does not exist only
	// loosely; a prepack script that exits non-zero fails it for certain.
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"@acme/lib","version":"1.0.0","scripts":{"prepack":"exit 3"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env")
	err := run([]string{dir}, envFile, "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "npm pack") {
		t.Fatalf("run = %v, want the failed npm pack", err)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Errorf("env file written although the pack failed (stat err %v)", err)
	}
}

// The whole command in process: it packs, writes the routing environment,
// serves, and returns nil on SIGINT. SIGINT is also caught by the test's own
// channel so a signal that lands before run is listening cannot kill the test
// process; it is resent until run returns.
func TestRunServesUntilInterrupted(t *testing.T) {
	if testing.Short() {
		t.Skip("runs npm pack; runs in task test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("cannot deliver SIGINT to the test's own process on Windows")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}
	up := fakeRegistry(t)
	dir := writePkg(t, "@acme/lib", "1.2.0")
	envFile := filepath.Join(t.TempDir(), "env")

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	guard := make(chan os.Signal, 64)
	signal.Notify(guard, os.Interrupt)
	t.Cleanup(func() { signal.Stop(guard) })

	done := make(chan error, 1)
	go func() { done <- run([]string{dir}, envFile, up.URL) }()

	deadline := time.After(2 * time.Minute)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	sawEnv := ""
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run returned %v, want nil after SIGINT", err)
			}
			goto finished
		case <-deadline:
			t.Fatal("run did not return after SIGINT")
		case <-tick.C:
			if sawEnv == "" {
				if b, err := os.ReadFile(envFile); err == nil && len(b) > 0 {
					sawEnv = string(b)
				}
			}
			if sawEnv != "" {
				_ = self.Signal(os.Interrupt)
			}
		}
	}
finished:
	for _, want := range []string{"npm_config_globalconfig=", "pnpm_config_userconfig=", `pnpm_config_minimum_release_age_exclude=["@acme/lib"]`} {
		if !strings.Contains(sawEnv, want) {
			t.Errorf("env file lacks %q:\n%s", want, sawEnv)
		}
	}
	if !strings.HasSuffix(sawEnv, "\n") || strings.Contains(sawEnv, "\n\n") {
		t.Errorf("env file must be KEY=VALUE lines, newline-terminated: %q", sawEnv)
	}
}

// TestMain_AsChild is the child half of the subprocess tests below.
func TestMain_AsChild(t *testing.T) {
	if os.Getenv("NPM_OVERLAY_AS_MAIN") != "1" {
		t.Skip("only meaningful as the child of a subprocess test")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Args = append([]string{"npm-overlay"}, args...)
	main()
	os.Exit(0)
}

func child(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^TestMain_AsChild$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "NPM_OVERLAY_AS_MAIN=1")
	return cmd
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run child: %v", err)
	}
	return ee.ExitCode()
}

func TestMainUsageErrors(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	for name, args := range map[string][]string{
		"no arguments":      nil,
		"no -pack":          {"-env-file", envFile},
		"no -env-file":      {"-pack", t.TempDir()},
		"unknown flag":      {"-nope"},
		"-pack without dir": {"-pack"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := child(t, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			code := exitCode(t, cmd.Run())
			if code != 2 {
				t.Errorf("exit = %d, want 2 (usage)\nstderr: %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "usage: npm-overlay -pack DIR") && !strings.Contains(stderr.String(), "flag") {
				t.Errorf("stderr = %q, want the usage line", stderr.String())
			}
			if _, err := os.Stat(envFile); !os.IsNotExist(err) {
				t.Errorf("env file written on a usage error (stat err %v)", err)
			}
		})
	}
}

func TestMainExitsOneWithTheErrorOnStderr(t *testing.T) {
	cmd := child(t, "-pack", t.TempDir(), "-env-file", filepath.Join(t.TempDir(), "env"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if code := exitCode(t, cmd.Run()); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.HasPrefix(stderr.String(), "npm-overlay: ") || !strings.Contains(stderr.String(), "package.json") {
		t.Errorf("stderr = %q, want \"npm-overlay: \" and the missing package.json", stderr.String())
	}
}

var servingLine = regexp.MustCompile(`npm-overlay: serving (.+) at (http://\S+); routing environment in (.+)`)

// The command as CI runs it: it announces its URL on stderr once the env file
// exists, serves the overlay's registry on that URL, and exits 0 on SIGTERM.
func TestMainServesAndStopsOnSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("runs npm pack in a child process; runs in task test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM cannot be delivered to a child on Windows")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}
	up := fakeRegistry(t)
	dir := writePkg(t, "@acme/lib", "1.2.0")
	envFile := filepath.Join(t.TempDir(), "env")

	cmd := child(t, "-pack", dir, "-env-file", envFile, "-upstream", up.URL)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		exited <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	var url string
	timeout := time.After(2 * time.Minute)
	for url == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("the overlay exited before announcing itself")
			}
			if m := servingLine.FindStringSubmatch(line); m != nil {
				url = m[2]
				if m[1] != dir || m[3] != envFile {
					t.Errorf("announcement %q names %q and %q, want the pack dir and env file", line, m[1], m[3])
				}
			}
		case <-timeout:
			t.Fatal("the overlay never announced itself")
		}
	}

	// The env file is complete by the time the announcement is printed.
	env, err := os.ReadFile(envFile)
	if err != nil || !strings.Contains(string(env), "npm_config_globalconfig=") {
		t.Fatalf("env file after the announcement = %q, %v", env, err)
	}
	resp, err := http.Get(url + "@acme%2flib")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"1.2.0"`) || !strings.Contains(string(body), `"1.0.0"`) {
		t.Errorf("packument: HTTP %d %s, want the local 1.2.0 beside the published 1.0.0", resp.StatusCode, body)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if code := exitCode(t, err); code != 0 {
			t.Errorf("exit after SIGTERM = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the overlay did not exit after SIGTERM")
	}
	if resp, err := http.Get(url + "@acme%2flib"); err == nil {
		_ = resp.Body.Close()
		t.Error("the registry still answers after the process exited")
	}
}
