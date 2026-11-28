package codegen

import (
	"strings"
	"testing"
)

func mcpSvc(name string, methods ...Method) ServiceDef {
	return ServiceDef{Name: name, Package: "shop.v1", Methods: methods}
}

func TestMCPTools(t *testing.T) {
	optIn := &MethodMCP{OptIn: true}
	cases := []struct {
		name    string
		svcs    []ServiceDef
		want    []string // tool name + flags: "name r d i p"
		wantErr string
	}{
		{
			name: "not opted in is absent",
			svcs: []ServiceDef{mcpSvc("OrdersService", Method{Name: "ListOrders", AuthRequired: true})},
		},
		{
			name: "read with NO_SIDE_EFFECTS is read-only",
			svcs: []ServiceDef{mcpSvc("OrdersService", Method{Name: "ListOrders", AuthRequired: true, NoSideEffects: true, MCP: optIn})},
			want: []string{"orders_list_orders r"},
		},
		{
			name: "read-named without NO_SIDE_EFFECTS is neither read-only nor destructive",
			svcs: []ServiceDef{mcpSvc("OrdersService", Method{Name: "GetOrder", AuthRequired: true, MCP: &MethodMCP{OptIn: true, Idempotent: true}})},
			want: []string{"orders_get_order i"},
		},
		{
			name: "mutating without mcp_mutating is absent",
			svcs: []ServiceDef{mcpSvc("OrdersService", Method{Name: "CancelOrder", AuthRequired: true, MCP: &MethodMCP{OptIn: true, Mutating: true}})},
		},
		{
			name: "mutating with mcp_mutating is destructive",
			svcs: []ServiceDef{mcpSvc("OrdersService", Method{Name: "CancelOrder", AuthRequired: true, MCP: &MethodMCP{OptIn: true, Mutating: true, MutatingAck: true, Idempotent: true}})},
			want: []string{"orders_cancel_order d i"},
		},
		{
			name: "service default and per-rpc opt-out",
			svcs: []ServiceDef{mcpSvc("OrdersService",
				Method{Name: "ListOrders", AuthRequired: true, NoSideEffects: true, MCP: optIn},
				Method{Name: "GetOrder", AuthRequired: true, NoSideEffects: true, MCP: &MethodMCP{OptIn: false}},
			)},
			want: []string{"orders_list_orders r"},
		},
		{
			name: "public rpc only when opted in",
			svcs: []ServiceDef{mcpSvc("StatusService",
				Method{Name: "GetStatus", NoSideEffects: true, MCP: optIn},
				Method{Name: "GetVersion", NoSideEffects: true},
			)},
			want: []string{"status_get_status r p"},
		},
		{
			name:    "streaming refused",
			svcs:    []ServiceDef{mcpSvc("OrdersService", Method{Name: "WatchOrders", ServerStreaming: true, AuthRequired: true, MCP: &MethodMCP{OptIn: true, Explicit: true}})},
			wantErr: "streaming rpc",
		},
		{
			name: "service default skips a streaming rpc",
			svcs: []ServiceDef{mcpSvc("OrdersService",
				Method{Name: "ListOrders", AuthRequired: true, NoSideEffects: true, MCP: optIn},
				Method{Name: "WatchOrders", ServerStreaming: true, AuthRequired: true, MCP: optIn},
			)},
			want: []string{"orders_list_orders r"},
		},
		{
			name: "streaming opted out by default is fine",
			svcs: []ServiceDef{mcpSvc("OrdersService", Method{Name: "WatchOrders", ServerStreaming: true, AuthRequired: true})},
		},
		{
			name: "collision refused",
			svcs: []ServiceDef{
				mcpSvc("OrdersService", Method{Name: "ListOrders", NoSideEffects: true, MCP: optIn}),
				mcpSvc("Orders", Method{Name: "ListOrders", NoSideEffects: true, MCP: optIn}),
			},
			wantErr: "already",
		},
		{
			name:    "too long refused",
			svcs:    []ServiceDef{mcpSvc("OrdersService", Method{Name: "List" + strings.Repeat("Very", 20), NoSideEffects: true, MCP: optIn})},
			wantErr: "is not 1-64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, err := MCPTools(tc.svcs)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, tl := range tools {
				s := tl.Name
				for _, f := range []struct {
					on bool
					c  string
				}{{tl.ReadOnly, "r"}, {tl.Destructive, "d"}, {tl.Idempotent, "i"}, {tl.Public, "p"}} {
					if f.on {
						s += " " + f.c
					}
				}
				got = append(got, s)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("tools = %q, want %q", got, tc.want)
			}
		})
	}
}
