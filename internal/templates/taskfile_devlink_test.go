package templates

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The Taskfile's dev-build -ldflags script yields -ldflags=-w, and nothing
// when FORGE_DEV_DWARF=1 (exactly) opts out or GOFLAGS already carries an -ldflags (a
// command-line -ldflags would replace the user's).
func TestTaskfileDevLdflagsScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is POSIX sh, which Task runs through its own interpreter on Windows")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	var v struct {
		Sh string `yaml:"sh"`
	}
	if err := yaml.Unmarshal([]byte("sh: "+TaskfileDevLdflagsScript), &v); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ dwarf, goflags, want string }{
		{"", "", "-ldflags=-w"},
		{"", "-trimpath -mod=mod", "-ldflags=-w"},
		{"0", "-trimpath", "-ldflags=-w"},
		{"1", "", ""},
		{"true", "-trimpath", "-ldflags=-w"},
		{"", "-trimpath -ldflags=-X=main.v=dev", ""},
		{"", "--ldflags=-s", ""},
	}
	for _, c := range cases {
		// `go env GOFLAGS` is stubbed: what it prints is the case's input.
		cmd := exec.Command(sh, "-c", `go() { printf '%s\n' "$FAKE_GOFLAGS"; }; `+v.Sh)
		cmd.Env = []string{"FAKE_GOFLAGS=" + c.goflags, "FORGE_DEV_DWARF=" + c.dwarf, "PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("FORGE_DEV_DWARF=%q GOFLAGS=%q: %v", c.dwarf, c.goflags, err)
		}
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("FORGE_DEV_DWARF=%q GOFLAGS=%q: the script yields %q, want %q", c.dwarf, c.goflags, got, c.want)
		}
	}
}

// Every Taskfile dev build passes DEV_LDFLAGS, and the `dev` task hands air
// the same flags; neither the library Taskfile (no binary) nor `task install`
// gets them.
func TestTaskfileDevBuildsLinkWithoutDWARF(t *testing.T) {
	for name, wants := range map[string][]string{
		"project/Taskfile.yml.tmpl": {
			"go build -trimpath -buildvcs=true {{`{{.DEV_LDFLAGS}}`}} ./...",
			"FORGE_GO_BUILD_FLAGS:\n        sh: {{taskfileDevLdflags}}",
		},
		"project/Taskfile.cli.yml.tmpl": {
			"go build -trimpath -buildvcs=true {{`{{.DEV_LDFLAGS}}`}} -o bin/",
			"go install -trimpath -buildvcs=true ./cmd/",
		},
	} {
		body, err := templateFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "DEV_LDFLAGS:\n        sh: {{taskfileDevLdflags}}") {
			t.Errorf("%s declares no DEV_LDFLAGS from taskfileDevLdflags", name)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
}
