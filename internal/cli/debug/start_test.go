package debug

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMainPackageForService_NoWildcard pins bug #2: a multi-binary repo must
// resolve a service to a single concrete main package, never `./cmd/...`
// (which makes `go build -o <file> ./cmd/...` fail with "cannot write
// multiple packages to non-directory"). The forge/control-plane shape is a
// multi-service dispatcher binary (has a `server` subcommand) alongside one
// or more standalone binaries.
func TestMainPackageForService_NoWildcard(t *testing.T) {
	dir := t.TempDir()
	// cmd/app — the multi-service dispatcher (declares a `server` subcommand
	// in a sibling file under its tree).
	writeMain(t, filepath.Join(dir, "cmd", "app", "main.go"))
	writeFile(t, filepath.Join(dir, "cmd", "app", "cmd", "server.go"),
		"package cmd\n\nvar serverCmd = cobra.Command{\n\tUse:   \"server [services...]\",\n}\n")
	// cmd/proxy — a standalone binary, no `server` subcommand.
	writeMain(t, filepath.Join(dir, "cmd", "proxy", "main.go"))

	withWD(t, dir, func() {
		// A SERVICE_NAME-dispatched service ("admin-server") has no own cmd
		// dir; it must resolve to the dispatcher binary, NOT a wildcard.
		got, err := mainPackageForService("admin-server", "")
		if err != nil {
			t.Fatalf("mainPackageForService(admin-server): %v", err)
		}
		if got != "./cmd/app" {
			t.Fatalf("got %q, want ./cmd/app (the dispatcher binary)", got)
		}
		if got == "./cmd/..." {
			t.Fatal("resolver returned a wildcard — the exact bug #2 regression")
		}

		// A service whose name IS a binary dir resolves to that binary.
		got, err = mainPackageForService("proxy", "")
		if err != nil {
			t.Fatalf("mainPackageForService(proxy): %v", err)
		}
		if got != "./cmd/proxy" {
			t.Fatalf("got %q, want ./cmd/proxy", got)
		}
	})
}

// TestMainPackageForService_AmbiguousErrors verifies that when a service can't
// be disambiguated (multiple dispatcher-less binaries), the resolver errors
// with an actionable message instead of emitting a broken wildcard.
func TestMainPackageForService_AmbiguousErrors(t *testing.T) {
	dir := t.TempDir()
	writeMain(t, filepath.Join(dir, "cmd", "one", "main.go"))
	writeMain(t, filepath.Join(dir, "cmd", "two", "main.go"))

	withWD(t, dir, func() {
		_, err := mainPackageForService("mystery", "")
		if err == nil {
			t.Fatal("expected an error for an ambiguous multi-binary repo")
		}
	})
}

// TestServiceRunSpec verifies the run args + env that make a debugged service
// SERVE rather than exit 0: `server` subcommand + SERVICE_NAME/PORT.
func TestServiceRunSpec(t *testing.T) {
	dir := t.TempDir()
	writeMain(t, filepath.Join(dir, "cmd", "app", "main.go"))
	writeFile(t, filepath.Join(dir, "cmd", "app", "cmd", "server.go"),
		"package cmd\n\nvar serverCmd = cobra.Command{\n\tUse: \"server\",\n}\n")
	// A standalone binary with no `server` subcommand.
	writeMain(t, filepath.Join(dir, "cmd", "plain", "main.go"))

	withWD(t, dir, func() {
		args, env := serviceRunSpec("admin-server", "./cmd/app", 38745)
		if len(args) != 1 || args[0] != "server" {
			t.Fatalf("args = %v, want [server]", args)
		}
		if !contains(env, "SERVICE_NAME=admin-server") {
			t.Fatalf("env %v missing SERVICE_NAME=admin-server", env)
		}
		if !contains(env, "PORT=38745") {
			t.Fatalf("env %v missing PORT=38745", env)
		}

		// A binary without a server subcommand runs as-is (no args/env).
		args, env = serviceRunSpec("plain", "./cmd/plain", 38745)
		if args != nil || env != nil {
			t.Fatalf("standalone binary should get no run spec, got args=%v env=%v", args, env)
		}
	})
}

func TestDeclaresServerUse(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`Use:   "server [services...]",`, true},
		{`Use: "server",`, true},
		{`	Use:  "server",`, true},
		{`Use: "serve",`, false},
		{`Use: "db",`, false},
		{`Short: "run the server",`, false},
	}
	for _, tc := range cases {
		if got := declaresServerUse(tc.src); got != tc.want {
			t.Errorf("declaresServerUse(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// --- helpers ---

func writeMain(t *testing.T, path string) {
	t.Helper()
	writeFile(t, path, "package main\n\nfunc main() {}\n")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func withWD(t *testing.T, dir string, fn func()) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	defer func() { _ = os.Chdir(orig) }()
	fn()
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
