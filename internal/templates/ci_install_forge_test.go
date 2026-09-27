package templates

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The forge a CI job installs is decided by the PROJECT at run time, never
// stamped into the workflow when it was scaffolded.
//
// Every workflow here is "yours": written once, never rewritten. A version
// stamped into one is therefore frozen at scaffold time while go.mod moves on
// with every forge bump. houndersclub is the incident: ci.yml pinned
// `cmd/forge@7787cb0e…` in four places after go.mod had moved to 7355bcb3,
// and verify-generated failed with "refusing to overwrite .forge-kcl/ with an
// OLDER forge's KCL module" — CI was checking the tree with a different
// generator than the one that wrote it.

// installForgeScripts returns the `run:` body of every "Install forge" step
// in a rendered workflow.
func installForgeScripts(t *testing.T, workflow []byte) []string {
	t.Helper()
	var parsed struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &parsed); err != nil {
		t.Fatalf("workflow is not valid YAML: %v\n%s", err, workflow)
	}
	var scripts []string
	for _, job := range parsed.Jobs {
		for _, step := range job.Steps {
			if step.Name == "Install forge" {
				scripts = append(scripts, step.Run)
			}
		}
	}
	return scripts
}

// fullCIData turns on every job that installs forge.
func fullCIData() CIWorkflowData {
	return CIWorkflowData{
		ProjectName:         "demo",
		HasFrontends:        true,
		Frontends:           []FrontendCIConfig{{Name: "web", Path: "frontends/web"}},
		HasServices:         true,
		LintGolangci:        true,
		LintBuf:             true,
		LintMigrationSafety: true,
		VulnGo:              true,
		VulnNPM:             true,
		E2EEnabled:          true,
		E2ERuntime:          "k3d",
		HasKCL:              true,
		VerifyGenerated:     true,
		PermContents:        "read",
	}
}

var stampedForgeRefRE = regexp.MustCompile(`cmd/forge@[^"$\s]`)

// No install site may carry a literal ref, and every site installs the same
// way — a second, divergent install recipe is how one job ends up checking
// with a different forge than its neighbour.
func TestCIWorkflows_InstallForgeFromProjectAtRunTime(t *testing.T) {
	ci, err := CITemplates("github").Render("ci.yml.tmpl", fullCIData())
	if err != nil {
		t.Fatalf("render ci.yml: %v", err)
	}
	reconcile, err := CITemplates("github").Render("reconcile.yml.tmpl", ReconcileWorkflowData{
		ProjectName:  "demo",
		Environments: []DeployEnv{{Name: "staging"}},
	})
	if err != nil {
		t.Fatalf("render reconcile.yml: %v", err)
	}

	for name, wf := range map[string][]byte{"ci.yml": ci, "reconcile.yml": reconcile} {
		if m := stampedForgeRefRE.Find(wf); m != nil {
			t.Errorf("%s stamps a forge ref at scaffold time (%q) — it will drift from go.mod on the next forge bump", name, m)
		}
	}

	ciScripts := installForgeScripts(t, ci)
	// lint (migration safety), verify-generated, deployability, vuln-scan, e2e (k3d).
	if len(ciScripts) != 5 {
		t.Fatalf("ci.yml has %d Install forge steps, want 5 (one per job that runs forge)", len(ciScripts))
	}
	scripts := append(ciScripts, installForgeScripts(t, reconcile)...)
	for i, s := range scripts {
		if s != scripts[0] {
			t.Errorf("Install forge step %d differs from step 0:\n--- step 0\n%s\n--- step %d\n%s", i, scripts[0], i, s)
		}
		for _, want := range []string{
			// The exact read the houndersclub workflow uses, so a project
			// whose workflow was hand-fixed and one scaffolded fresh agree.
			`v=$(GOWORK=off go list -m -f '{{.Version}}' github.com/reliant-labs/forge`,
			`CGO_ENABLED=1 go install "github.com/reliant-labs/forge/cmd/forge@${v}"`,
		} {
			if !strings.Contains(s, want) {
				t.Errorf("Install forge step %d is missing %q:\n%s", i, want, s)
			}
		}
	}
}

// Runs the rendered install script against real modules, with `go install`
// stubbed to print what it WOULD install. `go list` is the real toolchain —
// the script's correctness is exactly how it reads go.mod.
func TestCIWorkflows_InstallForgeScriptResolvesFromProject(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain against fixture modules")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	ci, err := CITemplates("github").Render("ci.yml.tmpl", fullCIData())
	if err != nil {
		t.Fatalf("render ci.yml: %v", err)
	}
	scripts := installForgeScripts(t, ci)
	if len(scripts) == 0 {
		t.Fatal("ci.yml has no Install forge step")
	}
	script := scripts[0]

	// A `go` shim: `go install` reports its argument, everything else is
	// the real toolchain. Resolution happens offline: every fixture's
	// requirement is satisfied by `replace`-free pins go list can answer
	// from go.mod alone (-m on a direct requirement reads no module source).
	shimDir := t.TempDir()
	shim := "#!/bin/sh\nif [ \"$1\" = install ]; then echo \"INSTALL $2\"; exit 0; fi\nexec " + goBin + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "go"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	const pin = "v0.1.18-0.20260927181843-7355bcb3af9c"
	cases := []struct {
		name      string
		goMod     string
		forgeYAML string
		want      string // INSTALL line, or "" when the step must fail
		wantErr   string
	}{
		{
			name:      "service: the version go.mod requires, not forge.yaml's",
			goMod:     "module example.com/svc\n\ngo 1.24\n\nrequire github.com/reliant-labs/forge " + pin + "\n",
			forgeYAML: "forge_version: v0.1.17\n",
			want:      "INSTALL github.com/reliant-labs/forge/cmd/forge@" + pin,
		},
		{
			name:      "cli/library: go.mod does not require forge, forge.yaml pins it",
			goMod:     "module example.com/cli\n\ngo 1.24\n",
			forgeYAML: "name: cli\nforge_version: " + pin + "\n",
			want:      "INSTALL github.com/reliant-labs/forge/cmd/forge@" + pin,
		},
		{
			name:      "replace: refuses rather than installing a different forge",
			goMod:     "module example.com/svc\n\ngo 1.24\n\nrequire github.com/reliant-labs/forge " + pin + "\n\nreplace github.com/reliant-labs/forge => ../forge\n",
			forgeYAML: "forge_version: " + pin + "\n",
			wantErr:   "::error file=go.mod::go.mod replaces github.com/reliant-labs/forge with ../forge",
		},
		{
			name:      "an uninstallable (+dirty) forge_version fails by name",
			goMod:     "module example.com/cli\n\ngo 1.24\n",
			forgeYAML: "forge_version: " + pin + "+dirty\n",
			wantErr:   "::error file=go.mod::no installable forge version (got '" + pin + "+dirty')",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(c.goMod), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(c.forgeYAML), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GOFLAGS=-mod=mod", "GOPROXY=off", "GOTOOLCHAIN=local")
			out, err := cmd.CombinedOutput()
			got := strings.TrimSpace(string(out))
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("install step succeeded, want failure %q; output:\n%s", c.wantErr, got)
				}
				if !strings.Contains(got, c.wantErr) {
					t.Fatalf("install step failed without naming the cause; want %q in:\n%s", c.wantErr, got)
				}
				if strings.Contains(got, "INSTALL ") {
					t.Fatalf("install step installed something despite refusing:\n%s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("install step failed: %v\n%s", err, got)
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("install step installed the wrong forge; want %q in:\n%s", c.want, got)
			}
		})
	}
}

// verify-generated can only verify what it can regenerate. The job used to
// install forge and nothing else: no protoc-gen-go / protoc-gen-connect-go /
// goimports (so the Go stubs regenerated differently or not at all), and no
// Node + `npm ci` (so forge skipped every frontend's TypeScript stubs with a
// warning and certified the tree anyway) — houndersclub PR #8.
func TestCIWorkflow_VerifyGeneratedHasItsToolchain(t *testing.T) {
	ci, err := CITemplates("github").Render("ci.yml.tmpl", fullCIData())
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Jobs map[string]struct {
			Steps []struct {
				Name             string `yaml:"name"`
				Uses             string `yaml:"uses"`
				Run              string `yaml:"run"`
				WorkingDirectory string `yaml:"working-directory"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(ci, &parsed); err != nil {
		t.Fatal(err)
	}
	job, ok := parsed.Jobs["verify-generated"]
	if !ok {
		t.Fatal("no verify-generated job")
	}
	order := map[string]int{}
	for i, s := range job.Steps {
		switch {
		case s.Run == "forge tools install --force":
			order["tools"] = i
		case strings.HasPrefix(s.Uses, "actions/setup-node@"):
			order["node"] = i
		case s.Run == "npm ci" && s.WorkingDirectory == "frontends/web":
			order["npm"] = i
		case s.Run == "forge ci verify-generated":
			order["verify"] = i
		}
	}
	for _, step := range []string{"tools", "node", "npm", "verify"} {
		if _, ok := order[step]; !ok {
			t.Errorf("verify-generated is missing its %s step", step)
		}
	}
	for _, before := range []string{"tools", "node", "npm"} {
		if order[before] > order["verify"] {
			t.Errorf("%s runs after verify-generated", before)
		}
	}
}
