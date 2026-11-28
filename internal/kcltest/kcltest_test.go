package kcltest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTrimNoise(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean object", `{"a":1}`, `{"a":1}`},
		{"clean array", `[1,2]`, `[1,2]`},
		{"lock notice before an object", "waiting for package-cache lock...\n{\"a\":1}\n", "{\"a\":1}\n"},
		{"progress chatter before an array", "downloading x\nresolving y\n[1,2]", "[1,2]"},
		{"a brace inside the noise is where the JSON is taken to start", "note {x}\n{\"a\":1}", "{x}\n{\"a\":1}"},
		{"no JSON at all is returned untouched, so the caller can show it", "kcl: error: no such file", "kcl: error: no such file"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(TrimNoise([]byte(tt.in))); got != tt.want {
				t.Errorf("TrimNoise(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The test binary doubles as `kcl`: Run's contract is "stdout minus the noise,
// stderr folded into the error", which needs a program that prints on both.
func TestMain(m *testing.M) {
	if strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == "kcl" {
		os.Stdout.WriteString(os.Getenv("FAKEKCL_STDOUT"))
		os.Stderr.WriteString(os.Getenv("FAKEKCL_STDERR"))
		if os.Getenv("FAKEKCL_FAIL") != "" {
			os.Exit(1)
		}
		if os.Getenv("FAKEKCL_ECHO_CWD") != "" {
			wd, _ := os.Getwd()
			os.Stdout.WriteString("{\"cwd\":\"" + filepath.ToSlash(wd) + "\"}")
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeKCL(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("cannot read the test binary: %v", err)
	}
	dir := t.TempDir()
	name := "kcl"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRun(t *testing.T) {
	fakeKCL(t)
	ctx := context.Background()

	t.Run("the lock notice on stdout never reaches the JSON", func(t *testing.T) {
		t.Setenv("FAKEKCL_STDOUT", "waiting for package-cache lock...\n{\"ok\":true}\n")
		got, err := Run(ctx, t.TempDir(), "run", "main.k")
		if err != nil || string(got) != "{\"ok\":true}\n" {
			t.Errorf("Run = (%q, %v)", got, err)
		}
	})

	t.Run("stderr stays out of the document", func(t *testing.T) {
		t.Setenv("FAKEKCL_STDOUT", `{"ok":true}`)
		t.Setenv("FAKEKCL_STDERR", "warning: deprecated\n")
		got, err := Run(ctx, t.TempDir(), "run")
		if err != nil || strings.Contains(string(got), "deprecated") {
			t.Errorf("Run = (%q, %v): stderr is contamination", got, err)
		}
	})

	t.Run("a failure carries stderr in the error", func(t *testing.T) {
		t.Setenv("FAKEKCL_FAIL", "1")
		t.Setenv("FAKEKCL_STDERR", "error[E3M38]: undefined name 'x'\n")
		t.Setenv("FAKEKCL_STDOUT", "")
		_, err := Run(ctx, t.TempDir(), "run")
		var exit *exec.ExitError
		if err == nil || !strings.Contains(err.Error(), "stderr:") || !strings.Contains(err.Error(), "undefined name 'x'") {
			t.Fatalf("err = %v, want the exit error with kcl's stderr", err)
		}
		if !errors.As(err, &exit) {
			t.Errorf("the exit error must stay reachable: %v", err)
		}
	})

	t.Run("a failure with no stderr is the bare exit error", func(t *testing.T) {
		t.Setenv("FAKEKCL_FAIL", "1")
		t.Setenv("FAKEKCL_STDERR", "")
		_, err := Run(ctx, t.TempDir(), "run")
		if err == nil || strings.Contains(err.Error(), "stderr:") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("it runs in the directory it was given", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("FAKEKCL_STDOUT", "")
		t.Setenv("FAKEKCL_ECHO_CWD", "1")
		got, err := Run(ctx, dir, "run")
		if err != nil {
			t.Fatal(err)
		}
		resolved, _ := filepath.EvalSymlinks(dir)
		if !strings.Contains(string(got), filepath.ToSlash(dir)) && !strings.Contains(string(got), filepath.ToSlash(resolved)) {
			t.Errorf("Run did not execute in %q: %s", dir, got)
		}
	})
}

func TestRun_NoKCLOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Run(context.Background(), t.TempDir(), "version"); err == nil {
		t.Error("a missing kcl must be an error, not an empty document")
	}
}
