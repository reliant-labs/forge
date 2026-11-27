package forgeconv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

const ordersProto = `syntax = "proto3";

package orders.v1;

service OrderService {
  rpc ListOrders(ListOrdersRequest) returns (ListOrdersResponse);
  rpc GetOrder(GetOrderRequest) returns (GetOrderResponse);
  rpc CancelOrder(GetOrderRequest) returns (GetOrderResponse);
  rpc RefundOrder(GetOrderRequest) returns (GetOrderResponse);
  rpc GetOrderStatus(GetOrderRequest) returns (GetOrderResponse);
  rpc WatchOrders(GetOrderRequest) returns (stream GetOrderResponse);
}

message ListOrdersRequest {
  // Maximum orders to return, 1-100; 50 when unset.
  int32 page_size = 1;
  string page_token = 2;
  optional string customer_id = 3; // Only this customer's orders.
  message Nested {
    string inner = 1;
  }
}

message ListOrdersResponse {}

message GetOrderRequest {
  /* The order's id, as returned by ListOrders. */
  string id = 1;
}

message GetOrderResponse {}
`

const goodDesc = "Lists the caller's orders newest first; call it to find an order id before acting on an order."

func lintMCP(t *testing.T, services []codegen.ServiceDef) []Finding {
	t.Helper()
	dir := t.TempDir()
	pdir := filepath.Join(dir, "proto", "services", "orders", "v1")
	must(t, mkdirAllImpl(pdir))
	must(t, writeFile(filepath.Join(pdir, "orders.proto"), ordersProto))
	b, err := json.Marshal(codegen.ForgeDescriptor{Services: services})
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(dir, "gen", "forge_descriptor"), 0o755))
	must(t, writeFile(filepath.Join(dir, "gen", "forge_descriptor", "fixture.json"), string(b)))
	res, err := LintProtoTree(filepath.Join(dir, "proto"))
	if err != nil {
		t.Fatalf("LintProtoTree: %v", err)
	}
	var out []Finding
	for _, r := range []string{ruleMCPDescribed, ruleMCPMutating, ruleMCPShape} {
		out = append(out, findingsForRule(res.Findings, r)...)
	}
	return out
}

const ordersFile = "services/orders/v1/orders.proto"

func orderMethod(name string, mcp *codegen.MethodMCP, desc string) codegen.Method {
	in := "orders.v1.GetOrderRequest"
	if name == "ListOrders" {
		in = "orders.v1.ListOrdersRequest"
	}
	return codegen.Method{Name: name, InputType: in[strings.LastIndexByte(in, '.')+1:], InputTypeFQ: in,
		InputProtoFile: ordersFile, Description: desc, MCP: mcp}
}

func ordersService(methods ...codegen.Method) codegen.ServiceDef {
	return codegen.ServiceDef{Name: "OrderService", Package: "orders.v1", ProtoFile: ordersFile, Methods: methods}
}

func byRule(fs []Finding) map[string][]Finding {
	m := map[string][]Finding{}
	for _, f := range fs {
		m[f.Rule] = append(m[f.Rule], f)
	}
	return m
}

func TestMCPDescribed(t *testing.T) {
	read := &codegen.MethodMCP{OptIn: true}
	got := byRule(lintMCP(t, []codegen.ServiceDef{ordersService(
		orderMethod("ListOrders", read, goodDesc),
		orderMethod("GetOrder", read, "GetOrder gets an order."),
		orderMethod("GetOrderStatus", read, ""),
		orderMethod("CancelOrder", nil, ""), // not opted in: never judged
	)}))
	d := got[ruleMCPDescribed]
	if len(d) != 3 {
		t.Fatalf("described findings = %d, want 3:\n%+v", len(d), d)
	}
	want := map[int]string{
		7:  "only restates its name",
		10: "has no leading comment",
		6:  "page_token have no comment", // page_size and customer_id are commented; Nested.inner is not top-level
	}
	for _, f := range d {
		if f.Severity != SeverityError || f.File != filepath.FromSlash(ordersFile) {
			t.Errorf("finding %+v: want an error in %s", f, ordersFile)
		}
		if w, ok := want[f.Line]; !ok || !strings.Contains(f.Message, w) {
			t.Errorf("line %d finding %q, want it to contain %q", f.Line, f.Message, want[f.Line])
		}
	}
}

func TestMCPDescribed_Short(t *testing.T) {
	got := byRule(lintMCP(t, []codegen.ServiceDef{ordersService(
		orderMethod("GetOrder", &codegen.MethodMCP{OptIn: true}, "Fetches one order with line items."),
	)}))[ruleMCPDescribed]
	if len(got) != 1 || !strings.Contains(got[0].Message, "under 8") {
		t.Fatalf("findings = %+v, want one short-comment finding", got)
	}
}

func TestMCPMutating(t *testing.T) {
	got := byRule(lintMCP(t, []codegen.ServiceDef{ordersService(
		orderMethod("CancelOrder", &codegen.MethodMCP{OptIn: true, Mutating: true}, goodDesc),
		orderMethod("RefundOrder", &codegen.MethodMCP{OptIn: true, Mutating: true, MutatingAck: true}, goodDesc),
		orderMethod("GetOrder", &codegen.MethodMCP{OptIn: true, MutatingAck: true}, goodDesc),
	)}))[ruleMCPMutating]
	if len(got) != 2 {
		t.Fatalf("mutating findings = %+v, want 2", got)
	}
	for _, f := range got {
		switch f.Line {
		case 8:
			if !strings.Contains(f.Message, "NOT served") || !strings.Contains(f.Remediation, "mcp_mutating: true") {
				t.Errorf("CancelOrder finding = %+v", f)
			}
		case 7:
			if !strings.Contains(f.Message, "does not mutate") {
				t.Errorf("GetOrder finding = %+v", f)
			}
		default:
			t.Errorf("unexpected finding %+v", f)
		}
	}
}

func TestMCPShape_StreamingAndCollision(t *testing.T) {
	read := &codegen.MethodMCP{OptIn: true}
	stream := orderMethod("WatchOrders", read, goodDesc)
	stream.ServerStreaming = true
	// "Order" + "Service" and "OrderService" both project to order_*.
	other := codegen.ServiceDef{Name: "Order", Package: "orders.v1", ProtoFile: ordersFile,
		Methods: []codegen.Method{orderMethod("GetOrder", read, goodDesc)}}
	got := byRule(lintMCP(t, []codegen.ServiceDef{ordersService(stream, orderMethod("GetOrder", read, goodDesc)), other}))[ruleMCPShape]
	if len(got) != 2 {
		t.Fatalf("shape findings = %+v, want streaming + collision", got)
	}
	var msgs []string
	for _, f := range got {
		msgs = append(msgs, f.Message)
	}
	all := strings.Join(msgs, "\n")
	if !strings.Contains(all, "is streaming") || !strings.Contains(all, `"order_get_order", already OrderService.GetOrder's`) {
		t.Errorf("shape findings:\n%s", all)
	}
}

func TestMCPShape_TooManyTools(t *testing.T) {
	var ms []codegen.Method
	for i := 0; i < mcpMaxToolsPerListener+1; i++ {
		ms = append(ms, orderMethod(fmt.Sprintf("GetThing%d", i), &codegen.MethodMCP{OptIn: true}, goodDesc))
	}
	// A mutating rpc without the acknowledgement is not served, so not counted.
	ms[0].MCP.Mutating = true
	got := byRule(lintMCP(t, []codegen.ServiceDef{ordersService(ms...)}))[ruleMCPShape]
	if len(got) != 0 {
		t.Fatalf("40 served tools: findings = %+v, want none", got)
	}
	ms = append(ms, orderMethod("GetThingLast", &codegen.MethodMCP{OptIn: true}, goodDesc))
	got = byRule(lintMCP(t, []codegen.ServiceDef{ordersService(ms...)}))[ruleMCPShape]
	if len(got) != 1 || got[0].Severity != SeverityWarning || !strings.Contains(got[0].Message, `"user" listener serves 41`) {
		t.Fatalf("41 served tools: findings = %+v, want one warning", got)
	}
}

func TestMCP_NothingDeclaredNoFindings(t *testing.T) {
	if got := lintMCP(t, []codegen.ServiceDef{ordersService(orderMethod("GetOrder", nil, ""))}); len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
}

func TestMCP_StaleDescriptor(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "proto", "services", "orders", "v1")
	must(t, mkdirAllImpl(pdir))
	src := strings.Replace(ordersProto, "rpc GetOrder(GetOrderRequest) returns (GetOrderResponse);",
		"rpc GetOrder(GetOrderRequest) returns (GetOrderResponse) { option (forge.v1.method) = { mcp: true }; }", 1)
	must(t, writeFile(filepath.Join(pdir, "orders.proto"), src))
	b, err := json.Marshal(codegen.ForgeDescriptor{Services: []codegen.ServiceDef{ordersService(orderMethod("GetOrder", nil, ""))}})
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(dir, "gen", "forge_descriptor"), 0o755))
	must(t, writeFile(filepath.Join(dir, "gen", "forge_descriptor", "fixture.json"), string(b)))
	res, err := LintProtoTree(filepath.Join(dir, "proto"))
	must(t, err)
	got := findingsForRule(res.Findings, ruleMCPShape)
	if len(got) != 1 || !strings.Contains(got[0].Remediation, "forge generate") {
		t.Fatalf("findings = %+v, want one asking for forge generate", got)
	}
}

func TestRestatesName(t *testing.T) {
	for desc, want := range map[string]bool{
		"ListOrders lists orders.":                true,
		"Lists all the orders.":                   true,
		"Returns the order by id for the caller.": false,
		"Gets an order.":                          true,
		"Fetches one order with its line items.":  false,
	} {
		name := "ListOrders"
		if strings.Contains(desc, "order.") || strings.Contains(desc, "by id") || strings.Contains(desc, "line items") {
			name = "GetOrder"
		}
		if got := restatesName(name, strings.Fields(desc)); got != want {
			t.Errorf("restatesName(%s, %q) = %v, want %v", name, desc, got, want)
		}
	}
}
