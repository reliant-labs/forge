package templates

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// The Taskfile's dev-build -ldflags script yields -ldflags=-w, and nothing
// when FORGE_DEV_DWARF=1 (exactly) opts out or GOFLAGS already carries an
// -ldflags entry (a command-line -ldflags would replace the user's). Only a
// whole GOFLAGS entry that is an -ldflags flag counts. It runs through
// mvdan.cc/sh's interpreter, which is what Task runs it with on every OS, and
// through the system sh too where there is one.
func TestTaskfileDevLdflagsScript(t *testing.T) {
	var v struct {
		Sh string `yaml:"sh"`
	}
	if err := yaml.Unmarshal([]byte("sh: "+TaskfileDevLdflagsScript), &v); err != nil {
		t.Fatal(err)
	}
	// `go env GOFLAGS` is stubbed: what it prints is the case's input.
	script := `go() { printf '%s\n' "$FAKE_GOFLAGS"; }; ` + v.Sh
	runners := map[string]func(env []string) (string, error){
		"mvdan.cc/sh": func(env []string) (string, error) {
			file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
			if err != nil {
				return "", err
			}
			var out bytes.Buffer
			r, err := interp.New(interp.Env(expand.ListEnviron(env...)), interp.StdIO(nil, &out, &out))
			if err != nil {
				return "", err
			}
			err = r.Run(context.Background(), file)
			return out.String(), err
		},
	}
	if sh, err := exec.LookPath("sh"); err == nil && runtime.GOOS != "windows" {
		runners["sh"] = func(env []string) (string, error) {
			cmd := exec.Command(sh, "-c", script)
			cmd.Env = append(env, "PATH=/usr/bin:/bin")
			out, err := cmd.Output()
			return string(out), err
		}
	}
	cases := []struct{ dwarf, goflags, want string }{
		{"", "", "-ldflags=-w"},
		{"", "-trimpath -mod=mod", "-ldflags=-w"},
		{"0", "-trimpath", "-ldflags=-w"},
		{"1", "", ""},
		{"true", "-trimpath", "-ldflags=-w"},
		{"", "-trimpath -ldflags=-X=main.v=dev", ""},
		{"", "-ldflags=-s", ""},
		{"", "--ldflags=-s", ""},
		{"", "-mod=mod\t-ldflags=-s", ""},
		{"", "-ldflags", ""},
		// Not an -ldflags flag: the default stays.
		{"", "-ldflagsx=1", "-ldflags=-w"},
		{"", "-gcflags=-ldflags", "-ldflags=-w"},
		{"", "-tags=x-ldflags", "-ldflags=-w"},
		// Globbing is off: an entry that looks like a pattern stays one entry.
		{"", "-tags=*", "-ldflags=-w"},
	}
	for name, run := range runners {
		for _, c := range cases {
			out, err := run([]string{"FAKE_GOFLAGS=" + c.goflags, "FORGE_DEV_DWARF=" + c.dwarf})
			if err != nil {
				t.Fatalf("%s: FORGE_DEV_DWARF=%q GOFLAGS=%q: %v", name, c.dwarf, c.goflags, err)
			}
			if got := strings.TrimSpace(out); got != c.want {
				t.Errorf("%s: FORGE_DEV_DWARF=%q GOFLAGS=%q: the script yields %q, want %q", name, c.dwarf, c.goflags, got, c.want)
			}
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
