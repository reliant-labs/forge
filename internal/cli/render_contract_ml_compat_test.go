package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v1.13.0 released forge.MLStack with the scrape on the component itself
// (forge.Adopt / forge.Install selector, port, allow, service_name). v1.14.0
// deprecates those fields and keeps them working: a project written for
// v1.13.0 renders byte-identically. ml-stack-v1.13.0.{k,json} are v1.13.0's
// render-contract fixture and golden, copied verbatim (`git show
// v1.13.0:internal/cli/testdata/render_contract/ml-stack.{k,json}`).
// internal/templates TestKCLModule_RenderContract renders the .k and
// requires the .json; this test pins the .json itself to v1.13.0's bytes, so
// regenerating the goldens (FORGE_UPDATE_GOLDEN=1) can never quietly move
// the compat baseline.
const mlStackV1130GoldenSHA256 = "97ee26b386d054a790781b8f5e9c58683d3cdbc6a46e37317d06333d4684a4d8"

func TestRenderContract_MLStackV1130IsFrozen(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "render_contract", "ml-stack-v1.13.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != mlStackV1130GoldenSHA256 {
		t.Fatalf("ml-stack-v1.13.0.json is sha256 %s, want v1.13.0's %s: it is the v1.13.0 compat baseline, "+
			"so a render that differs from it breaks a released project. Restore it with "+
			"`git show v1.13.0:internal/cli/testdata/render_contract/ml-stack.json` and fix the render", got, mlStackV1130GoldenSHA256)
	}
}

// A component scraped through its own (deprecated) fields warns on `forge
// env render`, naming the stack, the component, the fields and the
// replacement; one without them, and a stack scraped through
// Observability.telemetry, does not.
func TestMLScrapeFieldsWarnDeprecated(t *testing.T) {
	old, _ := loadContract(t, "ml-stack-v1.13.0")
	var errOut bytes.Buffer
	if err := checkEnvTelemetry(&errOut, "prod", old); err != nil {
		t.Fatal(err)
	}
	got := errOut.String()
	for _, want := range []string{
		`warning: ml stack "gke_acme_us-central1_gpu": forge.Adopt "dcgm-exporter" (accelerators) sets selector, port, allow, deprecated in v1.14.0`,
		"declare the scrape as a forge.Telemetry entry in Observability.telemetry",
		`cluster = "gke_acme_us-central1_gpu"`,
		"They still work; removal only in a future major",
		`it scrapes as service "ml-dcgm-exporter", and a forge.Telemetry of that service_name replaces it`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render warnings lack %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "deprecated in v1.14.0"); n != 1 {
		t.Errorf("%d deprecation warnings, want 1 (only the scraped Adopt; kueue and the Installs set no scrape fields):\n%s", n, got)
	}

	cur, _ := loadContract(t, "ml-stack")
	errOut.Reset()
	if err := checkEnvTelemetry(&errOut, "prod", cur); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errOut.String(), "deprecated") {
		t.Errorf("a stack scraped through Observability.telemetry warned:\n%s", errOut.String())
	}

	// An Install scraped through its own fields says forge.Install.
	inst := &KCLEntities{MLStacks: []MLStackOutput{{Cluster: "gpu", Components: []MLComponentOutput{
		{Slot: "accelerators", Mode: "install", Kind: "dcgm-exporter", Selector: map[string]string{"app": "dcgm"}, Port: 9400},
		{Slot: "queue", Mode: "adopt", Kind: "kueue"},
	}}}}
	w := mlScrapeDeprecations(inst)
	if len(w) != 1 || !strings.Contains(w[0], `forge.Install "dcgm-exporter" (accelerators) sets selector, port, deprecated`) || strings.Contains(w[0], "scrapes as service") {
		t.Errorf("Install warnings = %q", w)
	}
}
