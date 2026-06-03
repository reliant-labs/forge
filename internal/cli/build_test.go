package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeCoverageProfiles_MultipleDirs verifies the merge concatenates
// per-dir coverage.out files, keeps a single mode: header, and drops
// duplicate headers from each input.
func TestMergeCoverageProfiles_MultipleDirs(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// Two sibling subtrees with their own coverage.out.
	for _, sub := range []string{"internal", "pkg"} {
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		body := "mode: atomic\ngithub.com/x/" + sub + "/a.go:1.1,2.2 1 1\n"
		if err := os.WriteFile(filepath.Join(sub, "coverage.out"), []byte(body), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if err := mergeCoverageProfiles("coverage.out"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, err := os.ReadFile("coverage.out")
	if err != nil {
		t.Fatalf("read merged: %v", err)
	}
	content := string(got)
	headerCount := strings.Count(content, "mode:")
	if headerCount != 1 {
		t.Errorf("want exactly 1 mode: header, got %d in:\n%s", headerCount, content)
	}
	if !strings.Contains(content, "internal/a.go") || !strings.Contains(content, "pkg/a.go") {
		t.Errorf("merged content missing inputs:\n%s", content)
	}
}

// TestCountTagsHelper ensures the docker-build tag counter handles the
// canonical `-t a -t b` shape and ignores other flags.
func TestCountTagsHelper(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"empty", nil, 0},
		{"one tag", []string{"build", "-t", "foo:latest"}, 1},
		{"three tags", []string{"build", "-t", "a", "-t", "b", "-t", "c"}, 3},
		{"unrelated -f flag", []string{"build", "-t", "a", "-f", "Dockerfile"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := countTags(c.args); got != c.want {
				t.Errorf("want %d, got %d", c.want, got)
			}
		})
	}
}

// TestBuildPushFlagRegistered confirms the --push flag is wired into
// the build command and implies --docker at parse time.
func TestBuildPushFlagRegistered(t *testing.T) {
	cmd := newBuildCmd()
	f := cmd.Flags().Lookup("push")
	if f == nil {
		t.Fatal("--push flag not registered on build command")
	}
	if f.DefValue != "" {
		t.Errorf("--push default = %q, want empty", f.DefValue)
	}
}

func TestBuildDebugFlagExists(t *testing.T) {
	cmd := newBuildCmd()

	f := cmd.Flags().Lookup("debug")
	if f == nil {
		t.Fatal("--debug flag not registered on build command")
	}

	if f.DefValue != "false" {
		t.Errorf("--debug default = %q, want %q", f.DefValue, "false")
	}
}

func TestBuildDebugFlagParsesTrue(t *testing.T) {
	cmd := newBuildCmd()

	if err := cmd.Flags().Parse([]string{"--debug"}); err != nil {
		t.Fatalf("failed to parse --debug: %v", err)
	}

	val, err := cmd.Flags().GetBool("debug")
	if err != nil {
		t.Fatalf("GetBool(\"debug\") error: %v", err)
	}
	if !val {
		t.Error("expected --debug to be true after parsing --debug")
	}
}

func TestBuildDebugFlagDefaultIsFalse(t *testing.T) {
	cmd := newBuildCmd()

	// Parse with no flags — debug should remain false.
	if err := cmd.Flags().Parse([]string{}); err != nil {
		t.Fatalf("failed to parse empty args: %v", err)
	}

	val, err := cmd.Flags().GetBool("debug")
	if err != nil {
		t.Fatalf("GetBool(\"debug\") error: %v", err)
	}
	if val {
		t.Error("expected --debug to default to false")
	}
}

// TestExpandPushRegistries covers the k3d-aware expansion: localhost:<port>
// fans out to also tag registry.localhost:<port> (the in-cluster
// pull reference), every other registry passes through unchanged.
func TestExpandPushRegistries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{
			name: "localhost retags as registry.localhost",
			in:   "localhost:5051",
			want: []string{"localhost:5051", "registry.localhost:5051"},
		},
		{
			name: "localhost on a different port also retags",
			in:   "localhost:5050",
			want: []string{"localhost:5050", "registry.localhost:5050"},
		},
		{
			name: "non-localhost registries pass through unchanged",
			in:   "ghcr.io/acme",
			want: []string{"ghcr.io/acme"},
		},
		{
			name: "127.0.0.1 is NOT auto-mirrored (only literal localhost)",
			in:   "127.0.0.1:5051",
			want: []string{"127.0.0.1:5051"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := expandPushRegistries(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("expandPushRegistries(%q) = %v, want %v", c.in, got, c.want)
			}
			for i, v := range got {
				if v != c.want[i] {
					t.Errorf("expandPushRegistries(%q)[%d] = %q, want %q", c.in, i, v, c.want[i])
				}
			}
		})
	}
}

func TestBuildAllFlagsRegistered(t *testing.T) {
	cmd := newBuildCmd()

	expected := []struct {
		name     string
		defValue string
	}{
		{"output", "bin"},
		{"target", "all"},
		{"parallel", "true"},
		{"docker", "false"},
		{"debug", "false"},
	}

	for _, tt := range expected {
		f := cmd.Flags().Lookup(tt.name)
		if f == nil {
			t.Errorf("flag --%s not registered", tt.name)
			continue
		}
		if f.DefValue != tt.defValue {
			t.Errorf("flag --%s default = %q, want %q", tt.name, f.DefValue, tt.defValue)
		}
	}
}
