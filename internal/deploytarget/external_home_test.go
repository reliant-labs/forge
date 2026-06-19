package deploytarget

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExpandHomePath covers the ~ / $HOME expansion that loadExternalEnvFile
// applies so a KCL `env_file = "~/src/app/.env"` actually resolves (os.ReadFile
// does not expand ~; the shell normally would, but forge reads the path).
func TestExpandHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	cases := []struct {
		in, want string
	}{
		{"~/src/kalshi/.env", filepath.Join(home, "src/kalshi/.env")},
		{"~", home},
		{"$HOME/x", filepath.Join(home, "x")},
		{"$HOME", home},
		{"/abs/path/.env", "/abs/path/.env"},
		{"relative/.env", "relative/.env"},
		{"", ""},
		{"~notme/x", "~notme/x"}, // ~user form is not expanded — left as-is
	}
	for _, c := range cases {
		if got := expandHomePath(c.in); got != c.want {
			t.Errorf("expandHomePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLoadExternalEnvFile_TildePath proves the end-to-end path: a ~-prefixed
// env_file under the user's home is found and parsed (the exact failure the
// live `forge deploy prod` hit: "env_file ~/src/kalshi/.env not found").
func TestLoadExternalEnvFile_TildePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	dir := filepath.Join(home, ".forge-test-envfile")
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, ".env")
	if wErr := os.WriteFile(p, []byte("SECRET_TOKEN=abc123\n"), 0o600); wErr != nil {
		t.Fatalf("write: %v", wErr)
	}

	m, err := loadExternalEnvFile("~/.forge-test-envfile/.env")
	if err != nil {
		t.Fatalf("loadExternalEnvFile: %v", err)
	}
	if m["SECRET_TOKEN"] != "abc123" {
		t.Fatalf("SECRET_TOKEN = %q, want abc123 (tilde path not resolved)", m["SECRET_TOKEN"])
	}
}
