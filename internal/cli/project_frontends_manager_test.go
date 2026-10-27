package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/frontenddeps"
	"github.com/reliant-labs/forge/internal/templates"
)

// The rendered Taskfile must consume exactly the CI resolver's decision,
// including migrations that temporarily leave multiple managers' locks.
func TestProjectFrontendsManagerTaskfileParity(t *testing.T) {
	if testing.Short() {
		t.Skip("execs task over rendered Taskfile; runs in full scoped test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake package managers")
	}
	task, err := exec.LookPath("task")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "forge.yaml"), []byte("name: demo\nmodule_path: example.com/demo\nforge_version: v0.1.46\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	var want []string
	for _, pm := range []string{"npm", "pnpm", "yarn"} {
		for _, locks := range []bool{false, true} {
			name := pm + "-empty"
			if locks {
				name = pm + "-mixed"
			}
			dir := filepath.Join(root, "frontends", name)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "next.config.ts"), []byte("export default {};\n"), 0600); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"name": name, "packageManager": pm + "@1.0.0"})
			if err := os.WriteFile(filepath.Join(dir, "package.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			if locks {
				for _, f := range []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"} {
					if err := os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got := frontenddeps.PackageManager(dir, ""); got != pm {
				t.Fatalf("%s resolver=%s", name, got)
			}
			want = append(want, pm+":"+filepath.ToSlash(filepath.Join("frontends", name)))
		}
	}
	cmd := newProjectFrontendsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--with-package-manager"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	store, err := loadProjectStore()
	if err != nil {
		t.Fatal(err)
	}
	for _, fe := range ciFrontends(root, store.Config()) {
		if !strings.Contains(out.String(), fe.PM()+":"+fe.Path+"\n") {
			t.Fatalf("CI and Taskfile metadata disagree for %+v", fe)
		}
	}
	rows := strings.Fields(out.String())
	if len(rows) != len(want) {
		t.Fatalf("rows=%v want=%v", rows, want)
	}
	for _, row := range want {
		if !strings.Contains(out.String(), row+"\n") {
			t.Fatalf("missing %s in %s", row, out.String())
		}
	}
	// Freeze only the discovery result in a fake forge; decisions above were
	// made by the real command. A Taskfile-side lockfile algorithm would pick
	// pnpm for every mixed case and fail the recording assertions below.
	bin := t.TempDir()
	listing := filepath.Join(bin, "listing")
	if err := os.WriteFile(listing, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	fakeForge := filepath.Join(bin, "forge-list")
	if err := os.WriteFile(fakeForge, []byte("#!/bin/sh\ncase \"$*\" in 'project frontends --with-package-manager') cat '"+listing+"';; *) exit 91;; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	marks := filepath.Join(root, "marks")
	for _, pm := range []string{"npm", "pnpm", "yarn"} {
		if err := os.WriteFile(filepath.Join(bin, pm), []byte("#!/bin/sh\nprintf '%s:%s\\n' '"+pm+"' \"${PWD##*/}\" >> '"+marks+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data, err := templates.ProjectTemplates().Render("Taskfile.yml.tmpl", map[string]any{"ProjectName": "demo", "Name": "demo", "FrontendName": "web", "CLI": "forge"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Taskfile.yml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	run := exec.Command(task, "deps:frontend", "FORGE="+fakeForge)
	run.Dir = root
	run.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("task: %v\n%s", err, output)
	}
	got, err := os.ReadFile(marks)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range want {
		pm, path, _ := strings.Cut(row, ":")
		if !strings.Contains(string(got), pm+":"+filepath.Base(path)+"\n") {
			t.Errorf("missing %s in executed managers %s", row, got)
		}
	}
}
