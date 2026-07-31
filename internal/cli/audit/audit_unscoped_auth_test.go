package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/audittype"
	"github.com/reliant-labs/forge/internal/codegen"
)

// These tests build a project on disk with the SAME two inputs the real
// category reads — a gen/forge_descriptor.json carrying per-method
// AuthRequired, and a user-owned handler package — then assert on the
// derived sets. Nothing here greps rendered prose or quotes a sentence
// from a template.
//
// The auth seam the fixtures call is resolved from codegen rather than
// spelled: if the seam is renamed, these fixtures follow it instead of
// silently testing a function nothing calls any more.

// writeProject lays down a descriptor + handler package and returns the
// project dir. methods maps RPC name → auth_required.
func writeProject(t *testing.T, service string, methods map[string]bool, handlerSrc string) string {
	t.Helper()
	dir := t.TempDir()

	type method struct {
		Name         string
		InputType    string
		OutputType   string
		AuthRequired bool
	}
	type svc struct {
		Name      string
		Package   string
		GoPackage string
		ProtoFile string
		Methods   []method
	}
	var ms []method
	for name, auth := range methods {
		ms = append(ms, method{
			Name: name, InputType: name + "Request", OutputType: name + "Response",
			AuthRequired: auth,
		})
	}
	desc := struct {
		Services []svc
	}{Services: []svc{{
		Name:      service,
		Package:   "services.shop.v1",
		GoPackage: "example.com/app/gen/services/shop/v1",
		ProtoFile: "proto/services/shop/v1/shop.proto",
		Methods:   ms,
	}}}

	genDir := filepath.Join(dir, "gen")
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "forge_descriptor.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hDir := filepath.Join(dir, "internal", "handlers", "shop")
	if err := os.MkdirAll(hDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hDir, "handlers_crud.go"), []byte(handlerSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// unscopedMethods pulls the derived finding set out of the category's
// details, failing loudly if the shape is not what the emitter produces.
func unscopedMethods(t *testing.T, cat audittype.Category) []string {
	t.Helper()
	raw, ok := cat.Details["unscoped_rpcs"]
	if !ok {
		t.Fatalf("category emitted no unscoped_rpcs key at all — details: %v", cat.Details)
	}
	found, ok := raw.([]unscopedRPC)
	if !ok {
		t.Fatalf("unscoped_rpcs is %T, not []unscopedRPC — the derived set changed shape", raw)
	}
	var names []string
	for _, f := range found {
		names = append(names, f.Method)
	}
	return names
}

// delegationBody renders the exact shape the CRUD shim template emits:
// a bare delegation through the crud package, reading no caller.
func delegationBody(method string) string {
	return "func (s *Service) " + method + "(ctx context.Context, req *connect.Request[pb." + method + "Request]) (*connect.Response[pb." + method + "Response], error) {\n" +
		"\treturn " + codegen.CRUDDelegatePkgName() + ".HandleGet(s.crud" + method + "Op())(ctx, req)\n}\n"
}

// scopedBody renders a handler that resolves the caller through the real
// seam, whatever it is currently named.
func scopedBody(method string) string {
	seam := codegen.CRUDAuthSeam()
	return "func (s *Service) " + method + "(ctx context.Context, req *connect.Request[pb." + method + "Request]) (*connect.Response[pb." + method + "Response], error) {\n" +
		"\tclaims, err := " + seam + "(ctx)\n" +
		"\tif err != nil {\n\t\treturn nil, err\n\t}\n" +
		"\t_ = claims\n\treturn nil, nil\n}\n"
}

const handlerHeader = "package shop\n\nimport (\n\t\"context\"\n\t\"connectrpc.com/connect\"\n)\n\n"

// TestUnscopedAuth_FlagsAuthenticatedRPCThatNeverReadsTheCaller is the
// core assertion, and the one that goes red without the category: an RPC
// the proto declares authenticated, whose handler is a bare delegation,
// is reported.
func TestUnscopedAuth_FlagsAuthenticatedRPCThatNeverReadsTheCaller(t *testing.T) {
	src := handlerHeader + delegationBody("GetOrder") + scopedBody("ListMyOrders")
	dir := writeProject(t, "ShopService", map[string]bool{
		"GetOrder":     true,
		"ListMyOrders": true,
	}, src)

	cat := auditUnscopedAuth(dir)

	if cat.Status != audittype.StatusWarn {
		t.Fatalf("status = %q, want warn — an unscoped authenticated RPC must not report clean\nsummary: %s", cat.Status, cat.Summary)
	}
	names := unscopedMethods(t, cat)
	if len(names) != 1 || names[0] != "GetOrder" {
		t.Fatalf("unscoped = %v, want exactly [GetOrder]; ListMyOrders resolves the caller and must not be flagged", names)
	}
	if got := cat.Details["scoped_rpcs"]; got != 1 {
		t.Errorf("scoped_rpcs = %v, want 1", got)
	}
}

// TestUnscopedAuth_IgnoresPublicRPCs pins the false-positive boundary
// that matters most: auth_required: false means no principal exists, so
// reading claims is not expected and absence is not a finding.
func TestUnscopedAuth_IgnoresPublicRPCs(t *testing.T) {
	src := handlerHeader + delegationBody("GetProduct") + delegationBody("ListProducts")
	dir := writeProject(t, "ShopService", map[string]bool{
		"GetProduct":   false,
		"ListProducts": false,
	}, src)

	cat := auditUnscopedAuth(dir)

	if cat.Status != audittype.StatusOK {
		t.Fatalf("status = %q, want ok — public RPCs read no caller BY DECLARATION\nsummary: %s", cat.Status, cat.Summary)
	}
	if names := unscopedMethods(t, cat); len(names) != 0 {
		t.Fatalf("unscoped = %v, want empty — every RPC here declares auth_required: false", names)
	}
}

// TestUnscopedAuth_AcknowledgementSuppressesWithReason covers the escape
// hatch: a legitimately global authenticated RPC, acknowledged in code.
func TestUnscopedAuth_AcknowledgementSuppressesWithReason(t *testing.T) {
	src := handlerHeader +
		"// " + AuthUnscopedOKDirective + " global admin list; every caller is an operator.\n" +
		delegationBody("ListAllOrders")
	dir := writeProject(t, "ShopService", map[string]bool{"ListAllOrders": true}, src)

	cat := auditUnscopedAuth(dir)

	if cat.Status != audittype.StatusOK {
		t.Fatalf("status = %q, want ok — an acknowledged global RPC is not a finding\nsummary: %s", cat.Status, cat.Summary)
	}
	if names := unscopedMethods(t, cat); len(names) != 0 {
		t.Fatalf("unscoped = %v, want empty — the RPC carries %s", names, AuthUnscopedOKDirective)
	}
	acks, ok := cat.Details["acknowledged_rpcs"].([]acknowledgedRPC)
	if !ok || len(acks) != 1 {
		t.Fatalf("acknowledged_rpcs = %v, want exactly one entry", cat.Details["acknowledged_rpcs"])
	}
	if !strings.Contains(acks[0].Reason, "operator") {
		t.Errorf("reason = %q, want the author's text preserved for review", acks[0].Reason)
	}
}

// TestUnscopedAuth_BareAcknowledgementDoesNotCount is the whole point of
// requiring a reason. A directive with nothing after it is the
// unfalsifiable comment this category replaces, so it must not silence
// the finding.
func TestUnscopedAuth_BareAcknowledgementDoesNotCount(t *testing.T) {
	src := handlerHeader +
		"// " + AuthUnscopedOKDirective + "\n" +
		delegationBody("GetOrder")
	dir := writeProject(t, "ShopService", map[string]bool{"GetOrder": true}, src)

	cat := auditUnscopedAuth(dir)

	if cat.Status != audittype.StatusWarn {
		t.Fatalf("status = %q, want warn — a reasonless directive must not suppress\nsummary: %s", cat.Status, cat.Summary)
	}
	if names := unscopedMethods(t, cat); len(names) != 1 || names[0] != "GetOrder" {
		t.Fatalf("unscoped = %v, want [GetOrder] — the bare directive said nothing", names)
	}
}

// TestUnscopedAuth_AcceptsHelperIndirection keeps the common factoring
// out of the false-positive set: the handler does not call the seam
// itself, but calls a package helper that does.
func TestUnscopedAuth_AcceptsHelperIndirection(t *testing.T) {
	seam := codegen.CRUDAuthSeam()
	src := handlerHeader +
		"func (s *Service) callerID(ctx context.Context) (string, error) {\n" +
		"\tclaims, err := " + seam + "(ctx)\n" +
		"\tif err != nil {\n\t\treturn \"\", err\n\t}\n" +
		"\treturn claims.UserID, nil\n}\n\n" +
		"func (s *Service) GetOrder(ctx context.Context, req *connect.Request[pb.GetOrderRequest]) (*connect.Response[pb.GetOrderResponse], error) {\n" +
		"\tid, err := s.callerID(ctx)\n" +
		"\tif err != nil {\n\t\treturn nil, err\n\t}\n" +
		"\t_ = id\n\treturn nil, nil\n}\n"
	dir := writeProject(t, "ShopService", map[string]bool{"GetOrder": true}, src)

	cat := auditUnscopedAuth(dir)

	if cat.Status != audittype.StatusOK {
		t.Fatalf("status = %q, want ok — the handler resolves the caller one hop away\nsummary: %s", cat.Status, cat.Summary)
	}
}

// TestUnscopedAuth_EmptyDerivationFailsLoudly pins the guard against this
// category silently inspecting nothing. The descriptor declares
// authenticated RPCs under a service whose handler package does not
// exist, so no handler matches — and the category must SAY that rather
// than report a clean auth surface.
//
// The first cut of this file had exactly this bug (it resolved
// "StorefrontService" against internal/handlers/ instead of "storefront")
// and reported ok on a project with a live IDOR.
func TestUnscopedAuth_EmptyDerivationFailsLoudly(t *testing.T) {
	dir := writeProject(t, "ShopService", map[string]bool{"GetOrder": true}, handlerHeader+delegationBody("GetOrder"))
	// Remove the handler tree the descriptor points at.
	if err := os.RemoveAll(filepath.Join(dir, "internal", "handlers", "shop")); err != nil {
		t.Fatal(err)
	}

	cat := auditUnscopedAuth(dir)

	if cat.Status != audittype.StatusWarn {
		t.Fatalf("status = %q, want warn — 1 authenticated RPC declared and 0 inspected is a broken check, not a clean project\nsummary: %s", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "inspected nothing") {
		t.Errorf("summary = %q, want it to say the check inspected nothing", cat.Summary)
	}
	if got := cat.Details["declared_authenticated_rpcs"]; got != 1 {
		t.Errorf("declared_authenticated_rpcs = %v, want 1", got)
	}
}

// TestUnscopedAuth_NonServiceProjectsAreNotSubject keeps the category
// honest about generality: a project with no Connect services (a CLI, a
// worker-only app) is n/a, not clean-by-luck.
func TestUnscopedAuth_NonServiceProjectsAreNotSubject(t *testing.T) {
	dir := t.TempDir()
	cat := auditUnscopedAuth(dir)
	if cat.Status != audittype.StatusOK {
		t.Fatalf("status = %q, want ok for a project with no descriptor", cat.Status)
	}
	if !strings.Contains(cat.Summary, "n/a") {
		t.Errorf("summary = %q, want it marked n/a rather than implying a clean audit", cat.Summary)
	}
}
