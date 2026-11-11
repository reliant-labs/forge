package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/benefits"
)

// The front door's promise: in a repo with no forge.yaml, `forge project
// benefits` offers only what works there (scope: any), each with a recipe
// and an estimate to quote, and counts the rest; in a forge project it offers
// every shipped benefit.
func TestBuildBenefitsReport(t *testing.T) {
	reg, err := benefits.Load()
	if err != nil {
		t.Fatal(err)
	}
	outside := buildBenefitsReport(reg, false, false, "")
	if outside.ForgeProject || len(outside.Benefits) == 0 || outside.More == 0 {
		t.Fatalf("outside a forge project: got forge_project=%v, %d benefits, %d more; want false, some, some",
			outside.ForgeProject, len(outside.Benefits), outside.More)
	}
	for _, b := range outside.Benefits {
		if b.Scope != benefits.ScopeAny || b.Adopt == "" || b.Estimate == "" {
			t.Errorf("benefit %q offered outside a forge project without scope: any, a recipe and an estimate", b.ID)
		}
	}
	inside := buildBenefitsReport(reg, true, false, "")
	if !inside.ForgeProject || inside.More != 0 || len(inside.Benefits) != len(outside.Benefits)+outside.More {
		t.Errorf("in a forge project: %d benefits, %d more; want every shipped one (%d) and none more",
			len(inside.Benefits), inside.More, len(outside.Benefits)+outside.More)
	}
	if all := buildBenefitsReport(reg, false, true, ""); len(all.Benefits) != len(inside.Benefits) || all.More != 0 {
		t.Errorf("--all outside a forge project: %d benefits, %d more; want %d and none more", len(all.Benefits), all.More, len(inside.Benefits))
	}
	for _, b := range buildBenefitsReport(reg, true, false, "run").Benefits {
		if b.Category != "run" {
			t.Errorf("--category run returned %q from %q", b.ID, b.Category)
		}
	}
}

// The MCP `benefits` tool answers for the directory it is pointed at.
func TestMCP_BenefitsForADirectoryWithNoForgeYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"shop"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := connectMCP(t, dir)
	out, isErr := callMCP(t, cs, "benefits", map[string]any{"dir": dir})
	if isErr {
		t.Fatalf("benefits failed:\n%s", out)
	}
	var rep benefitsReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("benefits did not print JSON: %v\n%s", err, out)
	}
	if rep.ForgeProject {
		t.Error("a directory with no forge.yaml reported as a forge project")
	}
	if len(rep.Benefits) == 0 {
		t.Fatal("no benefits offered to a repo with no forge.yaml")
	}
	for _, b := range rep.Benefits {
		if b.Scope != benefits.ScopeAny {
			t.Errorf("benefit %q (scope %q) offered to a repo with no forge.yaml", b.ID, b.Scope)
		}
	}
}
