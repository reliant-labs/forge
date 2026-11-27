package codegen

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateMCPToolsGolden = flag.Bool("update-mcp-tools-golden", false, "rewrite testdata/mcp_tools/mcp_tools_gen.go.golden")

// mcpFixtureServices is testdata/mcp_tools/proto's OrdersService as the
// descriptor records it: ListOrders read-only, CancelOrder a served
// mutation, ShipOrder a mutation without mcp_mutating (absent).
func mcpFixtureServices() []ServiceDef {
	return []ServiceDef{{
		Name: "OrdersService", Package: "shop.v1", PkgName: "shopv1",
		GoPackage: "example.com/x/gen/services/shop/v1",
		ProtoFile: "services/shop/v1/shop.proto",
		Methods: []Method{
			{Name: "ListOrders", InputType: "ListOrdersRequest", OutputType: "ListOrdersResponse",
				InputTypeFQ: "shop.v1.ListOrdersRequest", OutputTypeFQ: "shop.v1.ListOrdersResponse",
				AuthRequired: true, NoSideEffects: true,
				Description: "ListOrders returns the caller's orders, newest first.",
				MCP:         &MethodMCP{OptIn: true}},
			{Name: "CancelOrder", InputType: "CancelOrderRequest", OutputType: "Empty",
				InputTypeFQ: "shop.v1.CancelOrderRequest", OutputTypeFQ: "google.protobuf.Empty",
				AuthRequired: true,
				Description:  "CancelOrder cancels one open order and refunds its payment.",
				MCP:          &MethodMCP{OptIn: true, Mutating: true, MutatingAck: true}},
			{Name: "ShipOrder", InputType: "CancelOrderRequest", OutputType: "Empty",
				InputTypeFQ: "shop.v1.CancelOrderRequest", OutputTypeFQ: "google.protobuf.Empty",
				AuthRequired: true, MCP: &MethodMCP{OptIn: true, Mutating: true}},
		},
	}}
}

func loadMCPFixtureDoc(t *testing.T) MCPSchemaDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "mcp_tools", "mcp.jsonschema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseMCPSchemaDoc(b)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// The rendered tool table, over the plugin's real output for a proto that
// exercises protojson's edge cases, is pinned byte for byte.
func TestRenderMCPToolsGolden(t *testing.T) {
	tools, err := MCPTools(mcpFixtureServices())
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderMCPTools(tools, loadMCPFixtureDoc(t), "example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "mcp_tools", "mcp_tools_gen.go.golden")
	if *updateMCPToolsGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update-mcp-tools-golden)", err)
	}
	if string(got) != string(want) {
		t.Errorf("rendered table differs from %s (run with -update-mcp-tools-golden):\n%s", golden, got)
	}
	if strings.Contains(string(got), "ship_order") {
		t.Error("a mutation without mcp_mutating is in the table")
	}
}

func TestRenderMCPToolsNoTools(t *testing.T) {
	got, err := RenderMCPTools(nil, MCPSchemaDoc{}, "example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "var MCPTools = []mcpkit.Tool{}") || strings.Contains(string(got), "connect\"") || strings.Contains(string(got), "encoding/json") {
		t.Fatalf("RenderMCPTools(no tools) is not an empty table:\n%s", got)
	}
}

// Each bundled schema stands alone: every $ref resolves inside its own
// $defs, and the protojson shapes survive (int64 as a string, enums by
// name, Timestamp/Duration as strings, maps, oneofs, buf.validate rules).
func TestMCPSchemaDocBundle(t *testing.T) {
	doc := loadMCPFixtureDoc(t)
	raw, err := doc.Bundle("shop.v1.ListOrdersRequest")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Not        map[string]any             `json:"not"`
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Type != "object" || len(s.Required) != 1 || s.Required[0] != "id" || s.Not == nil {
		t.Errorf("root: type=%q required=%v not=%v", s.Type, s.Required, s.Not)
	}
	for _, r := range mcpRefs(mustDecode(t, raw), nil) {
		if _, ok := s.Defs[r]; !ok {
			t.Errorf("$ref %q does not resolve inside the bundled schema", r)
		}
	}
	for _, want := range []string{"shop.v1.Status", "shop.v1.Item", "google.protobuf.Timestamp", "google.protobuf.Duration", "google.protobuf.Value"} {
		if _, ok := s.Defs[want]; !ok {
			t.Errorf("$defs lacks %s", want)
		}
	}
	if _, ok := s.Defs["shop.v1.ListOrdersResponse"]; ok {
		t.Error("$defs carries an unreachable definition")
	}
	checks := map[string]string{
		"big":      `"type":"string"`,
		"counts":   `"additionalProperties":{"format":"int64"`,
		"id":       `"minLength":3`,
		"cursor":   `"pattern":"^[a-z]*$"`,
		"pageSize": `"maximum":100`,
		"name":     `"type":"string"`,
	}
	for field, frag := range checks {
		if !strings.Contains(string(s.Properties[field]), frag) {
			t.Errorf("%s = %s, want it to contain %s", field, s.Properties[field], frag)
		}
	}
	if !strings.Contains(string(s.Defs["shop.v1.Status"]), `"STATUS_OPEN"`) {
		t.Errorf("enum not by name: %s", s.Defs["shop.v1.Status"])
	}

	if _, err := doc.Bundle("google.protobuf.Timestamp"); err == nil {
		t.Error("a non-object root must be refused")
	}
	if _, err := doc.Bundle("shop.v1.Nope"); err == nil {
		t.Error("an unknown message must be refused")
	}
}

func mustDecode(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
