package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/kclplugin"
)

// Render the real scaffold, not a hand-written approximation: a hosted
// binder must retain the browser choice while the shape admits its records.
func TestReplayScaffoldSitesRenderCaptureAndAdmissionTogether(t *testing.T) {
	if testing.Short() {
		t.Skip("renders real KCL through the native runtime; runs in task test")
	}
	kclplugin.Register()
	for _, kind := range []string{"vite-spa", "nextjs"} {
		for _, enabled := range []bool{true, false} {
			t.Run(kind+"/"+map[bool]string{true: "on", false: "off"}[enabled], func(t *testing.T) {
				dir := t.TempDir()
				fe, _ := generator.SiteStarterFrontend(kind, "web")
				if err := generator.WriteSiteStarter(dir, "shop", fe); err != nil {
					t.Fatal(err)
				}
				if err := generator.WriteSiteScaffold(dir, "shop", generator.Harness("reliant"), fe); err != nil {
					t.Fatal(err)
				}
				for _, env := range []string{"dev", "staging", "prod"} {
					path := filepath.Join(dir, "deploy/kcl", env, "main.k")
					if !enabled {
						b, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(b), "_session_replay = True", "_session_replay = False")), 0600); err != nil {
							t.Fatal(err)
						}
					}
					raw, err := renderKCLRaw(context.Background(), dir, env)
					if err != nil {
						t.Fatal(err)
					}
					e, err := parseKCLEntities(raw)
					if err != nil {
						t.Fatal(err)
					}
					if len(e.Frontends) != 1 {
						t.Fatalf("frontends: %v", e.Frontends)
					}
					f := e.Frontends[0]
					want := map[bool]string{true: "true", false: "false"}[enabled]
					if env == "dev" {
						if f.RuntimeConfig["OTEL_REPLAY"] != want {
							t.Fatalf("dev replay: %v", f.RuntimeConfig)
						}
					} else {
						if v := f.RuntimeConfigSpec["OTEL_REPLAY"].Value; v == nil || *v != want {
							t.Fatalf("hosted replay: %v", f.RuntimeConfigSpec)
						}
					}
					if e.Observability == nil || e.Observability.Policy == nil || slices.Contains(e.Observability.Policy.OptIn, "replay") != enabled {
						t.Fatalf("ingest policy: %+v", e.Observability)
					}
					if devReplayRequested(e) != enabled {
						t.Fatal("dev grant intent differs from ingest declaration")
					}
					if len(e.Workloads) != 0 {
						t.Fatal("static site acquired a backend or Docker dependency")
					}
				}
			})
		}
	}
}

func TestReplayLegacyEnvironmentStaysOff(t *testing.T) {
	if testing.Short() {
		t.Skip("renders real KCL; runs in task test")
	}
	for _, runtime := range []string{`runtime = forge.OnHost {}`, `image = "web", runtime = forge.OnHosted {}`} {
		e := renderFrontendEnv(t, `[forge.Frontend {name = "web", type = "vite", path = ".", `+runtime+`}]`, "control_plane = forge.ControlPlane {}\n observability = forge.Observability {}")
		if devReplayRequested(e) {
			t.Fatal("legacy schema default enabled replay")
		}
		f := e.Frontends[0]
		if _, ok := f.RuntimeConfig["OTEL_REPLAY"]; ok {
			t.Fatal("legacy runtime gained replay key")
		}
		if _, ok := f.RuntimeConfigSpec["OTEL_REPLAY"]; ok {
			t.Fatal("legacy hosted artifact gained replay key")
		}
	}
}

func TestReplayScaffoldAppEnvironmentsRender(t *testing.T) {
	if testing.Short() {
		t.Skip("scaffolds and renders a full app; runs in task test")
	}
	kclplugin.Register()
	dir := t.TempDir()
	g := generator.NewProjectGenerator("shop", dir, "example.com/shop")
	g.Kind = config.ProjectKindService
	g.ApplyKindFeatureDefaults(config.ProjectKindService)
	g.ServiceName = "item"
	g.FrontendName = "web"
	if err := g.Generate(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfigFrom(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := generator.LoadChecksums(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := generatePerEnvDeployConfig(dir, cfg, cs, nil); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"dev", "staging", "prod"} {
		raw, err := renderKCLRaw(t.Context(), dir, env)
		if err != nil {
			t.Fatal(err)
		}
		e, err := parseKCLEntities(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !devReplayRequested(e) {
			t.Fatalf("%s not opted in", env)
		}
		f := e.Frontends[0]
		if env == "dev" && f.RuntimeConfig["OTEL_REPLAY"] != "true" {
			t.Fatal("dev capture missing")
		}
		if env != "dev" && (f.RuntimeConfigSpec["OTEL_REPLAY"].Value == nil || *f.RuntimeConfigSpec["OTEL_REPLAY"].Value != "true") {
			t.Fatal("hosted capture missing")
		}
	}
}
