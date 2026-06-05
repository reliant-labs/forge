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
// project, points its kcl.mod at the in-tree forge module, and runs
// `kcl run` over the dev env. The scaffold must produce KCL that
// evaluates cleanly with empty HTTP_ROUTES/GRPC_ROUTES — every
// commented-out template route stays commented.
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

	// Point the project's kcl.mod at the in-tree forge module so the
	// import resolves without network/git access.
	wd, _ := os.Getwd()
	root := wd
	for range []int{1, 2, 3} {
		if _, err := os.Stat(filepath.Join(root, "kcl", "kcl.mod")); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	forgeKCL := filepath.Join(root, "kcl")

	// Rewrite the scaffolded kcl.mod to point at the in-tree forge
	// module via a local path so `kcl run` resolves without a network
	// or git-cache fetch of the pinned upstream tag.
	kclMod := "[package]\nname = \"svc-eval\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \"" + forgeKCL + "\" }\n"
	if err := os.WriteFile(filepath.Join(tmp, "kcl.mod"), []byte(kclMod), 0644); err != nil {
		t.Fatalf("rewrite kcl.mod: %v", err)
	}

	// Stub the env's auto-generated config_gen.k since `forge generate`
	// hasn't been run on this scaffold. APP_ENV+CONFIG_MAPS minimal.
	for _, env := range []string{"dev", "staging", "prod"} {
		stub := `import forge
APP_ENV: [forge.EnvVar] = []
CONFIG_MAPS: [forge.ConfigMap] = []
`
		if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", env, "config_gen.k"), []byte(stub), 0644); err != nil {
			t.Fatalf("write %s stub: %v", env, err)
		}
	}

	cmd := exec.Command("kcl", "run",
		"-E", "forge="+forgeKCL,
		"-S", "output",
		"--format", "json",
		filepath.Join(tmp, "deploy/kcl/dev"))
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
	// Sanity: main.k import block references ingress.
	mainK, _ := os.ReadFile(filepath.Join(tmp, "deploy/kcl/dev/main.k"))
	if !strings.Contains(string(mainK), "import deploy.kcl.dev.ingress as ing") {
		t.Errorf("dev/main.k missing ingress import after Generate:\n%s", mainK)
	}
}
