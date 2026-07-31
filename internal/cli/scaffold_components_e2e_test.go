//go:build e2e

package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EComponentsGenJSONDerivedFromSources pins that the generate pipeline
// reads the project's component inventory before it emits the deploy data. If
// it does not, deploy/kcl/components_gen.json comes out as
// `{"components": []}` and `forge run` dies with "no services/... declared in
// deploy/kcl/dev/". Discovery therefore runs right after the descriptor is
// extracted (codegen.DiscoverProjectComponents) and right before the emit.
//
// This drives the REAL pipeline (`forge project new`, which runs generate, then
// an explicit `forge generate`) on a multi-service project and asserts the
// emitted components_gen.json carries a server entry per service.
func TestE2EComponentsGenJSONDerivedFromSources(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel()

	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// Two mounted services on one shared binary — the multi-entity dogfood
	// shape that surfaced the empty-component-list defect.
	runCmd(t, dir, forgeBin,
		"project", "new", "multiapp",
		"--mod", "example.com/multiapp",
		"--binary", "shared",
		"--service", "order",
		"--service", "intake",
	)
	projectDir := filepath.Join(dir, "multiapp")

	// Re-run the pipeline explicitly so the assertion covers `forge generate`,
	// not only the one-shot scaffold path.
	runCmd(t, projectDir, forgeBin, "generate")

	assertComponentsGen := func(stage string) {
		raw := readFileE2E(t, filepath.Join(projectDir, "deploy", "kcl", "components_gen.json"))
		var doc struct {
			Project    string `json:"project"`
			Components []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"components"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("[%s] components_gen.json is not valid JSON: %v\n%s", stage, err, raw)
		}
		if len(doc.Components) == 0 {
			t.Fatalf("[%s] components_gen.json has an EMPTY component list — the introspection path did not populate it:\n%s", stage, raw)
		}
		got := map[string]string{}
		for _, c := range doc.Components {
			got[c.Name] = c.Kind
		}
		for _, svc := range []string{"order", "intake"} {
			if kind, ok := got[svc]; !ok {
				t.Fatalf("[%s] components_gen.json is missing the %q service; got %v", stage, svc, got)
			} else if kind != "server" {
				t.Fatalf("[%s] service %q has kind %q, want \"server\"", stage, svc, kind)
			}
		}
	}
	assertComponentsGen("forge generate")

	// BUG 2 rides the same scaffold: the KCL-native dev config.k must carry the
	// runtime MODE marker so the env is positively "development" (the seed gate
	// + dev-auth classifier read it). Staging inherits the schema default.
	devConfigK := readFileE2E(t, filepath.Join(projectDir, "deploy", "kcl", "dev", "config.k"))
	if !strings.Contains(devConfigK, `environment = "development"`) {
		t.Fatalf("dev config.k must mark the env development; got:\n%s", devConfigK)
	}
	stagingConfigK := readFileE2E(t, filepath.Join(projectDir, "deploy", "kcl", "staging", "config.k"))
	if strings.Contains(stagingConfigK, `environment = "development"`) {
		t.Fatalf("staging config.k must NOT be marked development; got:\n%s", stagingConfigK)
	}

	// Deepest headless layer: the dev KCL must derive the two workloads from
	// the now-populated components_gen.json (the exact check `forge run` /
	// `forge env up` make via entitiesEmpty). kcl is REQUIRED for this
	// render: skipped by name on a laptop, hard failure in CI.
	requireTool(t, "kcl")
	devManifest := filepath.Join(projectDir, "deploy", "kcl", "dev", "main.k")
	out := runCmdOutput(t, projectDir, "kcl", "run", "-D", "env=dev", "-S", "manifests", devManifest)
	for _, svc := range []string{"order", "intake"} {
		if !strings.Contains(out, svc) {
			t.Fatalf("dev KCL render did not derive a workload for %q:\n%s", svc, out)
		}
	}
}
