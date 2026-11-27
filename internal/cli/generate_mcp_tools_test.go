package cli

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
)

func mcpStepServices(mcp *codegen.MethodMCP) []codegen.ServiceDef {
	return []codegen.ServiceDef{{
		Name: "OrdersService", Package: "shop.v1", PkgName: "shopv1",
		GoPackage: "example.com/x/gen/services/shop/v1",
		ProtoFile: "services/shop/v1/shop.proto",
		Methods: []codegen.Method{
			{Name: "ListOrders", InputTypeFQ: "shop.v1.ListOrdersRequest", OutputTypeFQ: "shop.v1.ListOrdersResponse",
				AuthRequired: true, NoSideEffects: true, Description: "ListOrders lists.", MCP: mcp},
			{Name: "CancelOrder", InputTypeFQ: "shop.v1.CancelOrderRequest", OutputTypeFQ: "google.protobuf.Empty",
				AuthRequired: true, MCP: &codegen.MethodMCP{OptIn: true, Mutating: true}},
		},
	}}
}

// A project that serves no MCP tool gets an empty tool table, and the step
// runs no plugin (so it needs neither buf nor network).
func TestStepMCPTools_NoToolsWritesEmptyTable(t *testing.T) {
	dir := t.TempDir()
	ctx := &pipelineContext{ProjectDir: dir, ModulePath: "example.com/x", Checksums: &checksums.FileChecksums{},
		DeclaredServices: mcpStepServices(nil)}
	if err := stepMCPTools(ctx); err != nil {
		t.Fatalf("stepMCPTools: %v", err)
	}
	got := string(mcpRead(t, filepath.Join(dir, filepath.FromSlash(codegen.MCPToolsGenPath))))
	if !strings.Contains(got, "var MCPTools = []mcpkit.Tool{}") {
		t.Fatalf("want an empty table:\n%s", got)
	}
}

// mcp on a streaming rpc is refused before any plugin runs.
func TestStepMCPTools_RefusesStreaming(t *testing.T) {
	svcs := mcpStepServices(&codegen.MethodMCP{OptIn: true})
	svcs[0].Methods[0].ServerStreaming = true
	ctx := &pipelineContext{ProjectDir: t.TempDir(), Checksums: &checksums.FileChecksums{}, DeclaredServices: svcs}
	if err := stepMCPTools(ctx); err == nil || !strings.Contains(err.Error(), "streaming") {
		t.Fatalf("stepMCPTools = %v, want a streaming refusal", err)
	}
}

// End to end with the real buf and the pinned plugin: an opted-in read
// lands in the table with its bundled schemas, a mutation without
// mcp_mutating does not, and the plugin's document for the fixture proto is
// still the one internal/codegen's golden test renders from.
func TestStepMCPTools_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs buf and protoc-gen-connect-openapi (go-installed once); runs in task test")
	}
	if _, err := exec.LookPath("buf"); err != nil {
		t.Skip("buf is not on PATH")
	}
	dir := t.TempDir()
	fixture := filepath.Join("..", "codegen", "testdata", "mcp_tools")
	mcpCopyTree(t, filepath.Join(fixture, "proto"), filepath.Join(dir, "proto"))
	mcpCopyTree(t, filepath.Join("..", "assets", "proto", "buf"), filepath.Join(dir, "proto", "buf"))
	if err := os.WriteFile(filepath.Join(dir, "buf.yaml"), []byte("version: v2\nmodules:\n  - path: proto\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svcs := mcpStepServices(&codegen.MethodMCP{OptIn: true})
	tools, err := codegen.MCPTools(svcs)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := projectMCPSchemas(dir, tools)
	if err != nil {
		t.Fatalf("projectMCPSchemas: %v", err)
	}
	want, err := codegen.ParseMCPSchemaDoc(mcpRead(t, filepath.Join(fixture, "mcp.jsonschema.json")))
	if err != nil {
		t.Fatal(err)
	}
	for name, def := range want.Defs {
		if !bytes.Equal(mcpCompactJSON(t, doc.Defs[name]), mcpCompactJSON(t, def)) {
			t.Errorf("plugin's %s differs from testdata/mcp_tools/mcp.jsonschema.json (regenerate it):\n got %s\nwant %s", name, doc.Defs[name], def)
		}
	}

	ctx := &pipelineContext{ProjectDir: dir, ModulePath: "example.com/x", Checksums: &checksums.FileChecksums{}, DeclaredServices: svcs}
	if err := stepMCPTools(ctx); err != nil {
		t.Fatalf("stepMCPTools: %v", err)
	}
	got := string(mcpRead(t, filepath.Join(dir, filepath.FromSlash(codegen.MCPToolsGenPath))))
	for _, frag := range []string{`"orders_list_orders"`, "shopv1connect.OrdersServiceListOrdersProcedure", "ReadOnly:", `\"pageSize\"`} {
		if !strings.Contains(got, frag) {
			t.Errorf("tool table lacks %s:\n%s", frag, got)
		}
	}
	if strings.Contains(got, "cancel_order") {
		t.Errorf("a mutation without mcp_mutating is served:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "buf.gen.mcp.yaml")); !os.IsNotExist(err) {
		t.Error("the ephemeral buf template was left behind")
	}
}

func mcpRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mcpCompactJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if len(b) == 0 {
		return nil
	}
	if err := json.Compact(&out, b); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mcpCopyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
