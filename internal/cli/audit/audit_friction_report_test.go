package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/audittype"
)

// TestAuditReport_FrictionCategoryRoundTrips pins the additive-report
// contract for the friction category: buildAuditReport always carries a
// `friction` entry and the whole report survives a JSON marshal/unmarshal
// so existing consumers (which iterate categories) keep working. The
// friction roll-up itself (count-by-severity, malformed-warn) is tested in
// package cli where auditFriction lives; here we only assert the report
// shape, so the stub Friction (ok) is sufficient.
func TestAuditReport_FrictionCategoryRoundTrips(t *testing.T) {
	dir := t.TempDir()
	yamlBody := `name: demo
module_path: github.com/example/demo
forge_version: dev
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	writeComponentsJSONTest(t, dir) // empty → service kind; lets buildAuditReport load config

	report, err := buildAuditReport(testFactory(auditAPIConfig{}), dir)
	if err != nil {
		t.Fatalf("buildAuditReport: %v", err)
	}
	cat, ok := report.Categories["friction"]
	if !ok {
		t.Fatal("audit report missing friction category")
	}
	if cat.Status != audittype.StatusOK {
		t.Errorf("friction category status = %s, want ok", cat.Status)
	}
	// Additivity: the JSON shape must survive marshal/unmarshal so existing
	// consumers (which iterate categories) keep working.
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded.Categories["friction"]; !ok {
		t.Error("friction category lost in JSON round-trip")
	}
}
