//go:build e2e

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EMCPOptedInRPCBuilds: an rpc opted in with (forge.v1.method).mcp
// lands in the generated tool table, the scaffolded API listener hands that
// table to /mcp, and the project builds. A project born with no opted-in rpc
// gets the empty table and the same providers.go line, and builds too.
func TestE2EMCPOptedInRPCBuilds(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "mcpapp", "--mod", "example.com/mcpapp", "--service", "widget")
	projectDir := filepath.Join(dir, "mcpapp")
	addCorpusForgePkgReplace(t, projectDir)

	providers := readFileE2E(t, filepath.Join(projectDir, "internal", "app", "providers.go"))
	if !strings.Contains(providers, "Tools:          middleware.MCPTools,") {
		t.Fatalf("providers.go does not hand the API listener the MCP table:\n%s", providers)
	}
	tablePath := filepath.Join(projectDir, "pkg", "middleware", "mcp_tools_gen.go")
	if table := readFileE2E(t, tablePath); strings.Contains(table, "Procedure:") {
		t.Errorf("a project with no opted-in rpc has a non-empty table:\n%s", table)
	}

	protoPath := filepath.Join(projectDir, "proto", "services", "widget", "v1", "widget.proto")
	src := readFileE2E(t, protoPath)
	const anchor = "  // TODO: Add your RPC methods here.\n"
	if !strings.Contains(src, anchor) {
		t.Fatalf("widget.proto has no %q anchor — fixture is stale:\n%s", anchor, src)
	}
	src = strings.Replace(src, anchor, `  // Look up a gizmo's status by name.
  rpc LookupGizmo(LookupGizmoRequest) returns (LookupGizmoResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
    option (forge.v1.method) = {mcp: true};
  }
`, 1) + "\nmessage LookupGizmoRequest { string name = 1; }\nmessage LookupGizmoResponse { string status = 1; }\n"
	if err := os.WriteFile(protoPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, projectDir, forgeBin, "scaffold")
	runCmd(t, projectDir, forgeBin, "generate")

	table := readFileE2E(t, tablePath)
	for _, want := range []string{`"widget_lookup_gizmo"`, "LookupGizmoProcedure", "ReadOnly:"} {
		if !strings.Contains(table, want) {
			t.Errorf("mcp_tools_gen.go lacks %q:\n%s", want, table)
		}
	}
	runCmd(t, projectDir, "go", "build", "./...")
}
