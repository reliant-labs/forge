package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// writeMCPAppDescriptor writes a project dir whose descriptor declares the
// given methods on orders.v1.OrderService.
func writeMCPAppDescriptor(t *testing.T, methods ...codegen.Method) string {
	t.Helper()
	dir := t.TempDir()
	b, err := json.Marshal(codegen.ForgeDescriptor{Services: []codegen.ServiceDef{{
		Name: "OrderService", Package: "orders.v1", ProtoFile: "services/orders/v1/orders.proto", Methods: methods,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "gen", "forge_descriptor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gen", "forge_descriptor", "fixture.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeMCPAppAPI(t *testing.T, api mcpAppAPI, err error) {
	t.Helper()
	prev := mcpAppResolveAPI
	mcpAppResolveAPI = func(context.Context, string) (mcpAppAPI, error) { return api, err }
	t.Cleanup(func() { mcpAppResolveAPI = prev })
}

func mcpAppFixture(t *testing.T) string {
	return writeMCPAppDescriptor(t,
		codegen.Method{Name: "ListOrders", NoSideEffects: true, AuthRequired: true, Description: "Lists orders.", MCP: &codegen.MethodMCP{OptIn: true}},
		codegen.Method{Name: "CancelOrder", AuthRequired: true, MCP: &codegen.MethodMCP{OptIn: true, Mutating: true, MutatingAck: true, Idempotent: true}},
		codegen.Method{Name: "RefundOrder", AuthRequired: true, MCP: &codegen.MethodMCP{OptIn: true, Mutating: true}}, // not served
		codegen.Method{Name: "GetOrder", AuthRequired: true},                                                          // not opted in
	)
}

func TestMCPApp_Text(t *testing.T) {
	dir := mcpAppFixture(t)
	fakeMCPAppAPI(t, mcpAppAPI{Project: "Shop", URL: "http://localhost:8080/", Source: "host"}, nil)
	var out bytes.Buffer
	if err := runMCPApp(context.Background(), &out, dir, "dev", mcpAppOptions{}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"MCP endpoint (env dev): http://localhost:8080/mcp",
		"Tools (2):",
		"order_list_orders (read-only)",
		"order_cancel_order (destructive)",
		`claude mcp add --transport http shop-dev http://localhost:8080/mcp --header "Authorization: Bearer $MCP_TOKEN"`,
		"[mcp_servers.shop_dev]",
		`bearer_token_env_var = "MCP_TOKEN"`,
		".cursor/mcp.json",
		".vscode/mcp.json",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "refund") || strings.Contains(s, "get_order") {
		t.Errorf("output lists an unserved rpc:\n%s", s)
	}
}

func TestMCPApp_JSON(t *testing.T) {
	dir := mcpAppFixture(t)
	fakeMCPAppAPI(t, mcpAppAPI{Project: "shop", URL: "http://unused"}, nil)
	var out bytes.Buffer
	if err := runMCPApp(context.Background(), &out, dir, "prod", mcpAppOptions{JSON: true, URL: "https://api.shop.example/base"}); err != nil {
		t.Fatal(err)
	}
	var doc mcpAppDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out.String())
	}
	if !doc.OK || doc.ExitCode != 0 || doc.SchemaVersion != 1 || doc.URL != "https://api.shop.example/base/mcp" || doc.URLSource != "flag" {
		t.Errorf("doc head = %+v", doc)
	}
	if doc.Name != "app-prod" || doc.Auth.Kind != "bearer" || doc.Auth.TokenEnv != "MCP_TOKEN" {
		t.Errorf("name/auth = %q %+v", doc.Name, doc.Auth)
	}
	if len(doc.Tools) != 2 || doc.Tools[0].Procedure != "/orders.v1.OrderService/ListOrders" || !doc.Tools[0].ReadOnly ||
		!doc.Tools[1].Destructive || !doc.Tools[1].Idempotent || doc.Tools[0].Description != "Lists orders." {
		t.Errorf("tools = %+v", doc.Tools)
	}
	if len(doc.Clients) != 4 {
		t.Errorf("clients = %+v", doc.Clients)
	}
}

func TestMCPApp_NoTools(t *testing.T) {
	dir := writeMCPAppDescriptor(t, codegen.Method{Name: "GetOrder"})
	fakeMCPAppAPI(t, mcpAppAPI{}, errors.New("must not resolve an env with nothing to serve"))
	var out bytes.Buffer
	if err := runMCPApp(context.Background(), &out, dir, "dev", mcpAppOptions{JSON: true}); err != nil {
		t.Fatalf("no tools: err = %v, want nil", err)
	}
	var doc mcpAppDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.OK || len(doc.Tools) != 0 || doc.Tools == nil || !strings.Contains(doc.Message, "mcp: true") {
		t.Errorf("doc = %+v", doc)
	}
}

func TestMCPApp_UnresolvedEnv(t *testing.T) {
	dir := mcpAppFixture(t)
	fakeMCPAppAPI(t, mcpAppAPI{}, errors.New("could not resolve env dev's API URL"))
	var out bytes.Buffer
	err := runMCPApp(context.Background(), &out, dir, "dev", mcpAppOptions{JSON: true})
	if err == nil {
		t.Fatal("want an error")
	}
	var doc mcpAppDoc
	if jerr := json.Unmarshal(out.Bytes(), &doc); jerr != nil {
		t.Fatal(jerr)
	}
	if doc.OK || doc.ExitCode != exitWrong || !strings.Contains(doc.Error, "could not resolve") || len(doc.Tools) != 2 {
		t.Errorf("doc = %+v", doc)
	}
}

func TestMCPApp_IsSubcommandOfBareMCP(t *testing.T) {
	cmd := newMCPCmd()
	app, _, err := cmd.Find([]string{"app", "dev"})
	if err != nil || app.Name() != "app" {
		t.Fatalf("forge mcp app not found: %v", err)
	}
	if cmd.Flags().Lookup("tools") == nil {
		t.Error("bare forge mcp lost --tools")
	}
}
