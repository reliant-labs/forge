package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// renderNoFrontendEnv renders the env main.k a project scaffolded WITHOUT
// --frontend carries — the file `forge scaffold frontend` has to extend.
func renderNoFrontendEnv(t *testing.T, tmpl, env string) string {
	t.Helper()
	data := templates.EnvTemplateData{ProjectName: "acme", EnvName: env, IngressEnabled: true, PrimaryWorkload: "acme"}
	out, err := templates.DeployTemplates().Render(tmpl, data)
	if err != nil {
		t.Fatalf("render %s: %v", tmpl, err)
	}
	return string(out)
}

// scaffoldedEnvTemplate is the template `forge project new` renders an env
// from: dev on the host runtime, every other env on the cluster runtime.
func scaffoldedEnvTemplate(env string) string {
	if env == "dev" {
		return "kcl/dev/main.k.tmpl"
	}
	return "kcl/cloud/main.k.tmpl"
}

// TestSpliceFrontendIntoEnvKCL_EveryScaffoldedEnv pins that the splice finds
// its anchors in every env template forge ships (one per runtime), for a
// project born without a frontend. A template edit that moves
// or renames an anchor must fail here, not silently degrade the scaffold
// into printing a hint nobody reads.
func TestSpliceFrontendIntoEnvKCL_EveryScaffoldedEnv(t *testing.T) {
	for _, tc := range []struct {
		tmpl string
		env  string
	}{
		{"kcl/dev/main.k.tmpl", "dev"},
		{"kcl/cloud/main.k.tmpl", "staging"},
		{"kcl/cloud/main.k.tmpl", "prod"},
	} {
		t.Run(tc.tmpl+"/"+tc.env, func(t *testing.T) {
			in := renderNoFrontendEnv(t, tc.tmpl, tc.env)
			if strings.Contains(in, "forge.Frontend") {
				t.Fatalf("precondition: a no-frontend %s should declare no frontend", tc.tmpl)
			}
			dev := tc.env == "dev"
			out, status := spliceFrontendIntoEnvKCL(in, "acme", tc.env, "web", dev, 0)
			if status != frontendKCLApplied {
				t.Fatalf("splice did not apply (status %d) — an anchor moved in %s", status, tc.tmpl)
			}
			if !strings.Contains(out, "frontends += [forge.Frontend {\n        name = \"web\"\n        path = \"frontends/web\"") {
				t.Errorf("frontend entry missing:\n%s", out)
			}
			wantPort := `_web_frontend_port = plugin.resolve_port("acme-` + tc.env + `-web", 3000)`
			if dev {
				if !strings.Contains(out, wantPort) || !strings.Contains(out, "port = _web_frontend_port") {
					t.Errorf("dev env must declare and use a resolve_port for the frontend:\n%s", out)
				}
				if strings.Index(out, wantPort) > strings.Index(out, "_database_url = ") {
					t.Errorf("port must be declared with the other ports, before _database_url")
				}
			} else if strings.Contains(out, "_web_frontend_port") {
				t.Errorf("%s is not a dev env; it must not get a dev-server port", tc.env)
			}
			// Every frontend binds a runtime; a non-dev bucket is never guessed.
			wantRuntime := `runtime = forge.OnBucket {bucket = "REPLACE_ME_BUCKET"}`
			if dev {
				wantRuntime = "runtime = forge.OnHost {}"
			}
			if !strings.Contains(out, wantRuntime) {
				t.Errorf("%s: the spliced frontend binds no runtime (want %s):\n%s", tc.env, wantRuntime, out)
			}

			// Idempotent: a second scaffold of the same name is a no-op.
			if again, st := spliceFrontendIntoEnvKCL(out, "acme", tc.env, "web", dev, 0); st != frontendKCLAlreadyDeclared || again != out {
				t.Errorf("second splice must be a no-op, status %d", st)
			}
		})
	}
}

// TestSpliceFrontendIntoEnvKCL_ComposesWithExistingFrontend pins that a
// second frontend is added beside the first rather than replacing it.
func TestSpliceFrontendIntoEnvKCL_ComposesWithExistingFrontend(t *testing.T) {
	in := renderNoFrontendEnv(t, "kcl/dev/main.k.tmpl", "dev")
	one, _ := spliceFrontendIntoEnvKCL(in, "acme", "dev", "web", true, 0)
	two, status := spliceFrontendIntoEnvKCL(one, "acme", "dev", "admin", true, 0)
	if status != frontendKCLApplied {
		t.Fatalf("second frontend not applied: %d", status)
	}
	for _, want := range []string{`name = "web"`, `name = "admin"`, "_admin_frontend_port = plugin.resolve_port(\"acme-dev-admin\", 3000)"} {
		if !strings.Contains(two, want) {
			t.Errorf("missing %q after adding a second frontend", want)
		}
	}
}

// TestSpliceFrontendIntoEnvKCL_PinnedPort pins that --port lands in KCL as
// the literal, with no resolve_port that could step it elsewhere.
func TestSpliceFrontendIntoEnvKCL_PinnedPort(t *testing.T) {
	in := renderNoFrontendEnv(t, "kcl/dev/main.k.tmpl", "dev")
	out, status := spliceFrontendIntoEnvKCL(in, "acme", "dev", "web", true, 4123)
	if status != frontendKCLApplied {
		t.Fatalf("status %d", status)
	}
	if !strings.Contains(out, "port = 4123") || strings.Contains(out, "_web_frontend_port") {
		t.Errorf("pinned port must be written verbatim:\n%s", out)
	}
}

func TestSpliceFrontendIntoEnvKCL_NoAnchorLeavesFileAlone(t *testing.T) {
	in := "output = {}\n"
	out, status := spliceFrontendIntoEnvKCL(in, "acme", "dev", "web", true, 0)
	if status != frontendKCLNoAnchor || out != in {
		t.Fatalf("an unrecognised file must be left untouched, status %d", status)
	}
}

// TestRunAddFrontend_DeclaresFrontendInKCL is the end-to-end guard for the
// reported defect: `forge scaffold frontend web` in a project created without
// --frontend left deploy/kcl/dev/main.k with no frontend and no resolved port,
// so `forge env up` preflighted forge.yaml's literal 3000 and refused to start.
func TestRunAddFrontend_DeclaresFrontendInKCL(t *testing.T) {
	skipNpmInstall(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "forge.yaml"), []byte(freshServiceForgeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/example/demo\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"dev", "staging", "prod"} {
		dir := filepath.Join(root, "deploy", "kcl", env)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte(renderNoFrontendEnv(t, scaffoldedEnvTemplate(env), env)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)

	if err := runFrontend(t.Context(), "web", 0, "", "", "", "", nil); err != nil {
		t.Fatalf("runFrontend: %v", err)
	}

	dev, err := os.ReadFile(filepath.Join(root, "deploy", "kcl", "dev", "main.k"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dev), `plugin.resolve_port("demo-dev-web", 3000)`) || !strings.Contains(string(dev), "port = _web_frontend_port") {
		t.Errorf("dev/main.k must declare the frontend with a KCL-resolved port")
	}
	for _, env := range []string{"staging", "prod"} {
		b, _ := os.ReadFile(filepath.Join(root, "deploy", "kcl", env, "main.k"))
		if !strings.Contains(string(b), `name = "web"`) {
			t.Errorf("%s/main.k must declare the frontend", env)
		}
	}

	// forge.yaml must NOT carry a literal port the KCL did not choose.
	yml, _ := os.ReadFile(filepath.Join(root, "forge.yaml"))
	if strings.Contains(string(yml), "port: 3000") {
		t.Errorf("forge.yaml must not pin port 3000 — the dev port is KCL's:\n%s", yml)
	}
}
