// File: internal/cli/scaffold/schema_hardening_test.go
//
// Unit tests for the birth-time schema-hardening derivations that live in
// the CLI layer: the append-only CRUD-piece filter (the wire half of the
// immutable-ledger marker).

package scaffold

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// TestServerSetOmittedFromCreateRequest pins the `// forge:server-set`
// request-omission: a server-set field is carried onto the entityField
// list (entityFieldsFromSchemaDefs) but EXCLUDED from the born
// Create<Entity>Request message, with the surviving fields renumbered
// contiguously (1,2,...) — no gap where the omitted field sat. The Update
// request (AIP-134 entity + mask) never enumerates fields, so it carries no
// per-field trace of the server-set field either.
func TestServerSetOmittedFromCreateRequest(t *testing.T) {
	defs := []codegen.SchemaFieldDef{
		{Name: "customer", Kind: "string"},
		{Name: "status", Kind: "string", ServerSet: true}, // server-authoritative
		{Name: "amount", Kind: "int64"},
	}
	fields, _ := entityFieldsFromSchemaDefs("services.orders.v1", defs)

	// The marker rides through onto the entityField list.
	var sawServerSet bool
	for _, f := range fields {
		if f.Name == "status" && f.ServerSet {
			sawServerSet = true
		}
	}
	if !sawServerSet {
		t.Fatalf("entityFieldsFromSchemaDefs dropped the ServerSet bit: %+v", fields)
	}

	msgs := buildEntityCRUDMessagePieces("Order", fields)
	var createReq string
	for _, p := range msgs {
		if p.name == "CreateOrderRequest" {
			createReq = p.text
		}
	}
	if createReq == "" {
		t.Fatal("CreateOrderRequest piece not built")
	}
	if strings.Contains(createReq, "status") {
		t.Errorf("CreateOrderRequest must OMIT the server-set field:\n%s", createReq)
	}
	for _, keep := range []string{"customer", "amount"} {
		if !strings.Contains(createReq, keep) {
			t.Errorf("CreateOrderRequest dropped a client-settable field %q:\n%s", keep, createReq)
		}
	}
	// Field numbers stay contiguous across the omission (customer = 1,
	// amount = 2 — never a gap at the omitted status slot).
	if !strings.Contains(createReq, "customer = 1;") || !strings.Contains(createReq, "amount = 2;") {
		t.Errorf("CreateOrderRequest field numbers not contiguous after omission:\n%s", createReq)
	}
}

func TestAppendOnlyFilter(t *testing.T) {
	rpcs := buildEntityCRUDRPCPieces("Order")
	msgs := buildEntityCRUDMessagePieces("Order", nil)

	// no-op when appendOnly is false
	if got := appendOnlyFilter(rpcs, "Order", false); len(got) != len(rpcs) {
		t.Fatalf("appendOnlyFilter(false) changed the piece count: %d vs %d", len(got), len(rpcs))
	}

	gotRPCs := appendOnlyFilter(rpcs, "Order", true)
	names := map[string]bool{}
	for _, p := range gotRPCs {
		names[p.name] = true
	}
	for _, keep := range []string{"CreateOrder", "GetOrder", "ListOrders"} {
		if !names[keep] {
			t.Errorf("append-only RPCs must keep %s", keep)
		}
	}
	for _, drop := range []string{"UpdateOrder", "DeleteOrder"} {
		if names[drop] {
			t.Errorf("append-only RPCs must drop %s", drop)
		}
	}

	gotMsgs := appendOnlyFilter(msgs, "Order", true)
	for _, p := range gotMsgs {
		if p.name == "UpdateOrderRequest" || p.name == "UpdateOrderResponse" ||
			p.name == "DeleteOrderRequest" || p.name == "DeleteOrderResponse" {
			t.Errorf("append-only messages must drop %s", p.name)
		}
	}
	// Create/Get/List envelopes survive.
	msgNames := map[string]bool{}
	for _, p := range gotMsgs {
		msgNames[p.name] = true
	}
	for _, keep := range []string{"CreateOrderRequest", "GetOrderResponse", "ListOrdersRequest", "ListOrdersResponse"} {
		if !msgNames[keep] {
			t.Errorf("append-only messages must keep %s", keep)
		}
	}
}
