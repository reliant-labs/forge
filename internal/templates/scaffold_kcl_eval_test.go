package templates_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// TestScaffoldedIngressEvaluates scaffolds a fresh service-kind
// project and runs `kcl run` over the dev env. The scaffold must
// produce KCL that evaluates cleanly with empty HTTP_ROUTES/GRPC_ROUTES
// — every commented-out template route stays commented.
//
// Module resolution needs no rewrite: the test binary is a dev build
// (no stamped pkg version), so the scaffold is BORN with the embedded
// forge KCL module vendored into `.forge-kcl/` and deploy/kcl/kcl.mod
// pointing at it by relative path (internal/kclvendor).
func TestScaffoldedIngressEvaluates(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping scaffold KCL eval test")
	}
	tmp := t.TempDir()
	g := generator.NewProjectGenerator("svc-eval", tmp, "example.com/svc-eval")
	g.Kind = config.ProjectKindService
	g.ApplyKindFeatureDefaults(config.ProjectKindService)
	if err := g.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Born-vendored dev scaffold: relative dep + materialized module.
	kclModPath := filepath.Join(tmp, "deploy", "kcl", "kcl.mod")
	kclMod, err := os.ReadFile(kclModPath)
	if err != nil {
		t.Fatalf("read deploy/kcl/kcl.mod: %v", err)
	}
	if !strings.Contains(string(kclMod), `forge = { path = "../../.forge-kcl" }`) {
		t.Fatalf("dev scaffold kcl.mod not vendored:\n%s", kclMod)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".forge-kcl", "kcl.mod")); err != nil {
		t.Fatalf(".forge-kcl not materialized: %v", err)
	}

	// Stub the pipeline-generated KCL-native config trio (`forge
	// generate` hasn't run on this bare scaffold): the typed schema,
	// the projection lambda the env main.k imports, and the per-env
	// user-owned values instance. Minimal shapes with the exact
	// names/signatures the generators emit.
	schemaStub := `schema AppConfig:
    port: int = 8080
`
	projectionStub := `import forge
import config_schema

appConfigEnvMap = lambda c: config_schema.AppConfig -> {str: forge.EnvSource} {
    {"PORT" = {value = str(c.port)}}
}
`
	if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", "config_schema.k"), []byte(schemaStub), 0644); err != nil {
		t.Fatalf("write config_schema.k stub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", "config_projection.k"), []byte(projectionStub), 0644); err != nil {
		t.Fatalf("write config_projection.k stub: %v", err)
	}
	for _, env := range []string{"dev", "staging", "prod"} {
		configStub := `import config_schema

app_config: config_schema.AppConfig = {
}
`
		if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", env, "config.k"), []byte(configStub), 0644); err != nil {
			t.Fatalf("write %s config.k stub: %v", env, err)
		}
	}

	cmd := exec.CommandContext(t.Context(), "kcl", "run",
		"-S", "output",
		"--format", "json",
		filepath.Join(tmp, "deploy/kcl/dev"))
	// Run from the project root so the deploy-as-data main.k's
	// `file.read("deploy/kcl/components_gen.json")` resolves — KCL's
	// file.read is process-cwd-relative, the same contract forge's
	// RenderKCL / RenderManifests honor.
	cmd.Dir = tmp
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kcl run dev: %v\n%s", err, out)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	// Ingress arrays present and empty (no routes uncommented in scaffold).
	for _, k := range []string{"gateways", "http_routes", "grpc_routes"} {
		v, ok := parsed[k]
		if !ok {
			t.Errorf("scaffold JSON missing %q bucket\n%s", k, out)
			continue
		}
		arr, ok := v.([]any)
		if !ok {
			t.Errorf("scaffold JSON %q not an array: %T", k, v)
			continue
		}
		if k == "gateways" && len(arr) != 1 {
			t.Errorf("scaffold gateways count = %d, want 1 (public)\n%s", len(arr), out)
		}
		if (k == "http_routes" || k == "grpc_routes") && len(arr) != 0 {
			t.Errorf("scaffold %s count = %d, want 0\n%s", k, len(arr), out)
		}
	}
	// Sanity: main.k import block references ingress (relative to the
	// deploy/kcl package root).
	mainK, _ := os.ReadFile(filepath.Join(tmp, "deploy/kcl/dev/main.k"))
	if !strings.Contains(string(mainK), "import .ingress as ing") {
		t.Errorf("dev/main.k missing ingress import after Generate:\n%s", mainK)
	}
}
