package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote. cobra prints a command's output to os.Stdout, which is what main
// hands it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		defer func() { os.Stdout = orig }()
		fn()
	}()
	_ = w.Close()
	return <-done
}

// main on a command that succeeds returns without exiting; the build identity
// the release ldflags stamp into Version, BuildDate and GitCommit is what
// `forge version` reports.
func TestMainStampsTheBuildIdentityIntoVersion(t *testing.T) {
	t.Setenv("FORGE_NO_PATH_CHECK", "1") // the check execs every forge on PATH
	t.Setenv("FORGE_VERSION_DELEGATED", "1")
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	oldArgs, oldV, oldD, oldC := os.Args, Version, BuildDate, GitCommit
	t.Cleanup(func() { os.Args, Version, BuildDate, GitCommit = oldArgs, oldV, oldD, oldC })
	Version, BuildDate, GitCommit = "v9.8.7", "2031-01-02", "0123456789abcdef"
	os.Args = []string{"forge", "version"}

	out := captureStdout(t, main)
	if !strings.Contains(out, "v9.8.7") {
		t.Errorf("`forge version` printed %q, want the stamped version v9.8.7", out)
	}
	if !strings.Contains(out, "0123456") {
		t.Errorf("`forge version` printed %q, want the stamped commit", out)
	}
}

// Built the way a release is (go build -ldflags -X), the binary reports the
// stamped version, answers --help with exit 0, and turns a command error into
// "Error: ..." on stderr with exit status 1 and nothing on stdout.
func TestBuiltBinaryExitCodesAndOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("go build of the forge binary; runs in task test")
	}
	bin := filepath.Join(t.TempDir(), "forge")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build",
		"-ldflags", "-X main.Version=v7.6.5 -X main.GitCommit=abcdef0123456789", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	run := func(args ...string) (stdout, stderr string, code int) {
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "FORGE_NO_PATH_CHECK=1", "FORGE_VERSION_DELEGATED=1", "FORGE_TELEMETRY=off", "HOME="+t.TempDir())
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return so.String(), se.String(), code
	}

	t.Run("version reports the stamped build", func(t *testing.T) {
		stdout, stderr, code := run("version")
		if code != 0 || !strings.Contains(stdout, "v7.6.5") || !strings.Contains(stdout, "abcdef0") {
			t.Errorf("exit %d\nstdout: %s\nstderr: %s\nwant exit 0 and the stamped version and commit", code, stdout, stderr)
		}
	})
	t.Run("help exits zero and lists the commands", func(t *testing.T) {
		stdout, stderr, code := run("--help")
		if code != 0 || !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "Available Commands:") {
			t.Errorf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
	})
	t.Run("an unknown command is exit 1 with an Error line", func(t *testing.T) {
		stdout, stderr, code := run("definitely-not-a-command")
		if code != 1 {
			t.Errorf("exit = %d, want 1", code)
		}
		if !strings.Contains(stderr, "Error: ") || !strings.Contains(stderr, "definitely-not-a-command") {
			t.Errorf("stderr = %q, want an \"Error: \" line naming the command", stderr)
		}
		if strings.Contains(stdout, "Error: ") {
			t.Errorf("the error went to stdout: %q", stdout)
		}
	})
	t.Run("a bad flag is exit 1", func(t *testing.T) {
		_, stderr, code := run("version", "--no-such-flag")
		if code != 1 || !strings.Contains(stderr, "no-such-flag") {
			t.Errorf("exit %d, stderr %q; want 1 naming the flag", code, stderr)
		}
	})
}
