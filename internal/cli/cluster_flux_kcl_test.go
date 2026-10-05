package cli

// Rendering `forge.flux_chart()` through forge's real KCL seam, so the parity
// test in cluster_flux_test.go compares against what a CONSUMER would actually
// get rather than against a transcription of it.
//
// It goes through kclrender.Run — the module embedded in the binary, with
// `kcl_plugin.forge` registered — which is the same path every forge command
// renders through. Reading the `.k` file as text and parsing it by hand would
// prove nothing about what KCL evaluates, which is the thing that has to agree
// with the Go map.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/flux"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// renderKCLFluxChartValues is the `values` dict `forge.flux_chart()` produces.
func renderKCLFluxChartValues(t *testing.T) map[string]any {
	t.Helper()
	kclplugin.Register()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("kcl.mod", "[package]\nname = \"flux_parity\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n")
	// The chart is declared on a Bundle and read back off the RENDER, not
	// off the lambda's return value, so this exercises the whole path a
	// consumer's declaration takes — including the dict-to-HelmChart
	// coercion that runs the schema's `check:` block.
	write("main.k", `import forge

_bundle = forge.Bundle {
    project = "flux_parity"
    env = "dev"
    helm_charts = [forge.flux_chart()]
}

output = forge.render(_bundle)
`)

	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		t.Fatalf("render forge.flux_chart(): %v", err)
	}
	var doc struct {
		Output struct {
			HelmCharts []struct {
				Name      string         `json:"name"`
				OCI       string         `json:"oci"`
				Version   string         `json:"version"`
				Namespace string         `json:"namespace"`
				Values    map[string]any `json:"values"`
			} `json:"helm_charts"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse render: %v\n%s", err, out)
	}
	if len(doc.Output.HelmCharts) != 1 {
		t.Fatalf("render produced %d charts, want 1", len(doc.Output.HelmCharts))
	}
	chart := doc.Output.HelmCharts[0]

	// The chart REFERENCE and VERSION are shared with Go too
	// (internal/flux.ChartOCI / ChartVersion), and a drift there is worse
	// than a values drift: the reference is the SHARED-FLUX DETECTOR, so a
	// divergence makes a consumer's Flux undetectable and forge installs a
	// colliding second one.
	assertKCLGoString(t, "chart oci", chart.OCI, flux.ChartOCI)
	assertKCLGoString(t, "chart version", chart.Version, flux.ChartVersion)
	assertKCLGoString(t, "chart namespace", chart.Namespace, flux.Namespace)
	return chart.Values
}

func assertKCLGoString(t *testing.T, what, kcl, goSide string) {
	t.Helper()
	if kcl != goSide {
		t.Errorf("%s: kcl/lib/flux.k says %q and internal/flux says %q — these are one pinned value "+
			"in two places; update both", what, kcl, goSide)
	}
}
