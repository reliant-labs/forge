package templates_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// TestScaffoldedIngressEvaluates scaffolds a fresh service-kind
// project and renders the dev env. The scaffold must produce KCL that
// evaluates cleanly with empty HTTP_ROUTES/GRPC_ROUTES — every
// commented-out template route stays commented.
//
// Module resolution needs no rewrite: the test binary is a dev build
// (no stamped pkg version), so the scaffold is BORN with the embedded
// forge KCL module vendored into `.forge-kcl/` and deploy/kcl/kcl.mod
// pointing at it by relative path (internal/kclvendor).
//
// It renders through kclrender — forge's OWN evaluation seam — rather
// than by shelling out to the `kcl` binary, and that is a real part of
// what is being asserted. The scaffolded dev env declares its ports with
// the `plugin.allocate_port` / `plugin.resolve_port` builtins, which live
// in the `kcl_plugin.forge` namespace forge registers in-process; the
// standalone `kcl` binary has no plugin and cannot evaluate them. So
// "renders from a clean checkout" means renders THE WAY FORGE RENDERS IT,
// which is the only way any forge command ever reads this file. Shelling
// out would test a path no user takes and would fail on a scaffold that
// is completely correct.
func TestScaffoldedIngressEvaluates(t *testing.T) {
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
	// ONE generated module carrying both halves, matching what
	// codegen.GenerateConfigKCL emits (schema then lambda, same file).
	configGenStub := `import forge

schema AppConfig:
    port: int = 8080

appConfigEnvMap = lambda c: AppConfig -> {str: forge.EnvSource} {
    {"PORT" = {value = str(c.port)}}
}
`
	if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", "config_gen.k"), []byte(configGenStub), 0644); err != nil {
		t.Fatalf("write config_gen.k stub: %v", err)
	}
	for _, env := range []string{"dev", "staging", "prod"} {
		configStub := `import config_gen

app_config: config_gen.AppConfig = {
}
`
		if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", env, "config.k"), []byte(configStub), 0644); err != nil {
			t.Fatalf("write %s config.k stub: %v", env, err)
		}
	}

	// workDir is the project root so the main.k's relative imports
	// (`..workloads`, `..ingress`) and its kcl.mod dependency path resolve
	// — the same cwd contract forge's RenderKCL / RenderManifests honor.
	out, err := kclrender.Run(tmp, filepath.Join(tmp, "deploy/kcl/dev"), []string{"env=dev"})
	if err != nil {
		t.Fatalf("render dev env: %v", err)
	}
	// The rendered document wraps the contract as `output = forge.render(...)`
	// alongside `manifests`, so unwrap the same way parseKCLEntities does.
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	parsed, ok := doc["output"].(map[string]any)
	if !ok {
		t.Fatalf("rendered document has no `output` contract:\n%s", out)
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
