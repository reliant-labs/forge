package lint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/linter/finding"
)

func TestIdempotencyFindingsFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		rpc  idempotencyRPC
		want string // rule ID, "" for no finding
		msg  string // a substring of the message
	}{
		{name: "read without the option", rpc: idempotencyRPC{Name: "GetOrder"}, want: ruleIdempotencyReadUndeclared, msg: "named as a read (Get)"},
		{name: "read with the option", rpc: idempotencyRPC{Name: "GetOrder", NoSideEffects: true}},
		{name: "hook-generator read verb", rpc: idempotencyRPC{Name: "HasAccess"}, want: ruleIdempotencyReadUndeclared, msg: "(Has)"},
		{name: "activity read verb", rpc: idempotencyRPC{Name: "DescribeCluster"}, want: ruleIdempotencyReadUndeclared, msg: "(Describe)"},
		{name: "batch read", rpc: idempotencyRPC{Name: "BatchGetOrders"}, want: ruleIdempotencyReadUndeclared, msg: "(BatchGet)"},
		// Only BatchGet is a batch read, as in the hook generator and the
		// activity interceptor; no other Batch/Bulk prefix is stripped.
		{name: "BatchSearch is not judged", rpc: idempotencyRPC{Name: "BatchSearchOrders"}},
		{name: "BulkGet is not judged", rpc: idempotencyRPC{Name: "BulkGetOrders"}},
		{name: "BatchUpdate declaring NO_SIDE_EFFECTS is not judged", rpc: idempotencyRPC{Name: "BatchUpdateOrders", NoSideEffects: true}},
		{name: "server-streaming read", rpc: idempotencyRPC{Name: "WatchOrders"}, want: ruleIdempotencyReadUndeclared, msg: "(Watch)"},
		{name: "client-streaming read is not judged", rpc: idempotencyRPC{Name: "ListOrders", ClientStreaming: true}},
		{name: "whole-word match: Checkout is no read", rpc: idempotencyRPC{Name: "CheckoutCart"}},
		{name: "whole-word match: Issue is no read", rpc: idempotencyRPC{Name: "IssueRefund"}},
		{name: "noun name is not judged", rpc: idempotencyRPC{Name: "Health"}},
		{name: "noun name may declare it", rpc: idempotencyRPC{Name: "Version", NoSideEffects: true}},
		{name: "read-named write with a reason", rpc: idempotencyRPC{Name: "GetOrCreateSession", Mutation: true, MutationReason: "creates the session on first call"}},
		{name: "read-named write without a reason", rpc: idempotencyRPC{Name: "GetOrCreateSession", Mutation: true}, want: ruleIdempotencyReadUndeclared, msg: "with no reason"},
		{name: "bare forge:mutation on a write is not judged", rpc: idempotencyRPC{Name: "PlaceOrder", Mutation: true}},
		{name: "write declaring NO_SIDE_EFFECTS", rpc: idempotencyRPC{Name: "CreateOrder", NoSideEffects: true}, want: ruleIdempotencyWriteAsRead, msg: "named as a write (Create)"},
		{name: "revoke declaring NO_SIDE_EFFECTS", rpc: idempotencyRPC{Name: "RevokeKey", NoSideEffects: true}, want: ruleIdempotencyWriteAsRead, msg: "(Revoke)"},
		// Verbs a read-only rpc can open with are not judged.
		{name: "RunSavedQuery declaring NO_SIDE_EFFECTS", rpc: idempotencyRPC{Name: "RunSavedQuery", NoSideEffects: true}},
		{name: "ExecuteQuery declaring NO_SIDE_EFFECTS", rpc: idempotencyRPC{Name: "ExecuteQuery", NoSideEffects: true}},
		{name: "EnsureFresh declaring NO_SIDE_EFFECTS", rpc: idempotencyRPC{Name: "EnsureFresh", NoSideEffects: true}},
		{name: "StartPreview declaring NO_SIDE_EFFECTS", rpc: idempotencyRPC{Name: "StartPreview", NoSideEffects: true}},
		{name: "streaming write declaring NO_SIDE_EFFECTS", rpc: idempotencyRPC{Name: "SendMessages", ClientStreaming: true, NoSideEffects: true}, want: ruleIdempotencyWriteAsRead, msg: "(Send)"},
		{name: "write without the option", rpc: idempotencyRPC{Name: "CreateOrder"}},
		{name: "whole-word match: Settings is no write", rpc: idempotencyRPC{Name: "SettingsSummary", NoSideEffects: true}},
		{name: "forge:mutation and NO_SIDE_EFFECTS contradict", rpc: idempotencyRPC{Name: "FindAndHoldSeat", Mutation: true, MutationReason: "holds a seat", NoSideEffects: true}, want: ruleIdempotencyWriteAsRead, msg: "says both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.rpc.Service = "OrderService"
			got := idempotencyFindingsFor([]idempotencyRPC{tc.rpc})
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no finding, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one %s finding, got %+v", tc.want, got)
			}
			if got[0].Rule != tc.want || got[0].Severity != finding.SeverityError {
				t.Errorf("rule/severity = %s/%s, want %s/error", got[0].Rule, got[0].Severity, tc.want)
			}
			if !strings.Contains(got[0].Message, tc.msg) || !strings.Contains(got[0].Message, "OrderService."+tc.rpc.Name) {
				t.Errorf("message %q lacks %q or the rpc name", got[0].Message, tc.msg)
			}
			if got[0].Remediation == "" {
				t.Error("finding has no fix hint")
			}
		})
	}
}

const idempotencyFixtureProto = `syntax = "proto3";

package shop.v1;

service ShopService {
  rpc GetOrder(GetOrderRequest) returns (GetOrderResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc ListOrders(ListOrdersRequest) returns (ListOrdersResponse);
  // forge:mutation: creates the cart on first call
  rpc GetOrCreateCart(GetOrderRequest) returns (GetOrderResponse);
  // forge:mutation
  rpc FindAndHoldSeat(GetOrderRequest) returns (GetOrderResponse);
  rpc CountOrders(GetOrderRequest) returns (GetOrderResponse); // forge:mutation — bumps a counter
  rpc CreateOrder(GetOrderRequest) returns (GetOrderResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc RunSavedQuery(GetOrderRequest) returns (GetOrderResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  // forge:lint-disable-next-line forge-idempotency-write-marked-read: renders the mail it would send; sends nothing
  rpc SendTestPreview(GetOrderRequest) returns (GetOrderResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc WatchOrders(GetOrderRequest) returns (stream GetOrderResponse);
  rpc SearchOrders(stream GetOrderRequest) returns (GetOrderResponse);
}
`

// writeIdempotencyProject writes proto/services/shop/v1/shop.proto and a
// descriptor carrying methods (what forge generate compiles from it).
func writeIdempotencyProject(t *testing.T, methods []codegen.Method) string {
	t.Helper()
	root := t.TempDir()
	pdir := filepath.Join(root, "proto", "services", "shop", "v1")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "shop.proto"), []byte(idempotencyFixtureProto), 0o644); err != nil {
		t.Fatal(err)
	}
	desc := codegen.ForgeDescriptor{Services: []codegen.ServiceDef{{
		Name: "ShopService", Package: "shop.v1", ProtoFile: "services/shop/v1/shop.proto", Methods: methods,
	}}}
	b, err := json.Marshal(desc)
	if err != nil {
		t.Fatal(err)
	}
	ddir := filepath.Join(root, "gen", "forge_descriptor")
	if err := os.MkdirAll(ddir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ddir, "fixture.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCollectIdempotencyFindings(t *testing.T) {
	root := writeIdempotencyProject(t, []codegen.Method{
		{Name: "GetOrder", NoSideEffects: true},
		{Name: "ListOrders"},
		{Name: "GetOrCreateCart"},
		{Name: "FindAndHoldSeat"},
		{Name: "CountOrders"},
		{Name: "CreateOrder", NoSideEffects: true},
		{Name: "RunSavedQuery", NoSideEffects: true},
		{Name: "SendTestPreview", NoSideEffects: true},
		{Name: "WatchOrders", ServerStreaming: true},
		{Name: "SearchOrders", ClientStreaming: true},
	})
	fs, err := collectIdempotencyFindings(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range fs {
		if f.File != "proto/services/shop/v1/shop.proto" {
			t.Errorf("finding file = %q, want the proto relative to the project root", f.File)
		}
		rpc := ""
		for _, name := range []string{"ListOrders", "FindAndHoldSeat", "CreateOrder", "WatchOrders", "GetOrCreateCart", "CountOrders", "RunSavedQuery", "SendTestPreview", "SearchOrders"} {
			if strings.Contains(f.Message, "ShopService."+name+" ") {
				rpc = name
			}
		}
		got[rpc] = f.Rule + "@" + itoa(f.Line)
	}
	want := map[string]string{
		"ListOrders":      ruleIdempotencyReadUndeclared + "@9",
		"FindAndHoldSeat": ruleIdempotencyReadUndeclared + "@13", // forge:mutation with no reason
		"CreateOrder":     ruleIdempotencyWriteAsRead + "@15",
		"WatchOrders":     ruleIdempotencyReadUndeclared + "@25",
	}
	if len(got) != len(want) {
		t.Errorf("got findings %v, want %v", got, want)
	}
	for rpc, w := range want {
		if got[rpc] != w {
			t.Errorf("%s: got %q, want %q", rpc, got[rpc], w)
		}
	}
}

// A project nothing has generated yet has no descriptor: the lane has
// nothing to judge and says nothing (every other lane already asks for a
// generate).
func TestCollectIdempotencyFindingsNoDescriptor(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proto"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs, err := collectIdempotencyFindings(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Errorf("want no findings with no descriptor and no protos, got %+v", fs)
	}
}
