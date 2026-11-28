package browser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFindChromeNamesTheFixWhenNoneIsInstalled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the platform install locations of macOS and Windows are outside the test's control")
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv(ChromePathEnv, "")
	_, err := FindChrome()
	if !errors.Is(err, ErrNoChrome) {
		t.Fatalf("err = %v, want ErrNoChrome", err)
	}
	for _, want := range []string{"never downloads", ChromePathEnv, "Chromium"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestFindChromeHonoursChromePath(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "my-chrome")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ChromePathEnv, exe)
	got, err := FindChrome()
	if err != nil || got != exe {
		t.Fatalf("FindChrome() = %q, %v; want %q", got, err, exe)
	}
	t.Setenv(ChromePathEnv, filepath.Join(t.TempDir(), "absent"))
	if _, err := FindChrome(); !errors.Is(err, ErrNoChrome) {
		t.Fatalf("an absent CHROME_PATH: err = %v, want ErrNoChrome", err)
	}
}

func TestFindAxeScriptWalksUpFromEachDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	site := filepath.Join(root, "site", "dist")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindAxeScript(site); got != "" {
		t.Fatalf("FindAxeScript with no axe-core = %q, want \"\"", got)
	}
	axe := filepath.Join(root, "site", "node_modules", "axe-core", "axe.min.js")
	if err := os.MkdirAll(filepath.Dir(axe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(axe, []byte("window.axe={}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FindAxeScript(site); got != axe {
		t.Fatalf("FindAxeScript = %q, want %q", got, axe)
	}
}
