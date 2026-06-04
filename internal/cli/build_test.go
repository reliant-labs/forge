package cli

import (
	"os"
	"path/filepath"
	"runtime"
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
		{"target-arch", ""},
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

// TestResolveBuildArch covers the three branches of the resolveBuildArch
// helper: host matches target → empty (no cross-compile), host differs
// from target → resolved arch, explicit flag overrides the cfg default.
//
// We use runtime.GOARCH for the "host matches" cases so the test is
// portable across CI archs (both arm64 macOS and amd64 Linux runners).
// We use a guaranteed-mismatch string ("xyz") for the "host differs"
// cases — any non-matching arch token triggers the cross-compile branch.
func TestResolveBuildArch(t *testing.T) {
	otherArch := "amd64"
	if runtime.GOARCH == "amd64" {
		otherArch = "arm64"
	}

	cases := []struct {
		name      string
		cfgArch   string
		flagArch  string
		dockerCtx bool
		want      string
	}{
		{
			name:      "plain go build, no target → host arch (empty)",
			cfgArch:   "",
			flagArch:  "",
			dockerCtx: false,
			want:      "",
		},
		{
			name:      "docker build, no target → amd64 default",
			cfgArch:   "",
			flagArch:  "",
			dockerCtx: true,
			want: func() string {
				if runtime.GOARCH == "amd64" {
					return ""
				}
				return "amd64"
			}(),
		},
		{
			name:      "docker build with cfg arch differs from host → cross-compile",
			cfgArch:   otherArch,
			flagArch:  "",
			dockerCtx: true,
			want:      otherArch,
		},
		{
			name:      "host matches cfg target → empty",
			cfgArch:   runtime.GOARCH,
			flagArch:  "",
			dockerCtx: true,
			want:      "",
		},
		{
			name:      "explicit flag overrides cfg",
			cfgArch:   runtime.GOARCH, // would otherwise match host → empty
			flagArch:  otherArch,
			dockerCtx: false,
			want:      otherArch,
		},
		{
			name:      "flag matching host returns empty (no-op cross-compile)",
			cfgArch:   "",
			flagArch:  runtime.GOARCH,
			dockerCtx: false,
			want:      "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveBuildArch(c.cfgArch, c.flagArch, c.dockerCtx)
			if got != c.want {
				t.Errorf("resolveBuildArch(cfg=%q, flag=%q, docker=%v) = %q, want %q",
					c.cfgArch, c.flagArch, c.dockerCtx, got, c.want)
			}
		})
	}
}
