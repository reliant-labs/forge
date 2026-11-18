package contract

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyParam is the redaction table: what a failed or slow call's
// forensics event may record of each parameter, decided from its type
// expression and name alone. Deny by default — anything not listed as a
// value records its length or its type only.
func TestClassifyParam(t *testing.T) {
	imports := map[string]string{
		"context":   "context",
		"time":      "time",
		"pb":        "example.com/shop/gen/orders/v1",
		"bufpb":     "buf.build/gen/go/acme/apis/protocolbuffers/go/acme/v1",
		"tspb":      "google.golang.org/protobuf/types/known/timestamppb",
		"domain":    "example.com/shop/internal/domain",
		"generator": "example.com/shop/internal/generator", // "gen" is not a path segment here
	}
	aliases := map[string]string{"Status": "int", "OrderRef": "string", "Email": "string"}
	for _, tc := range []struct {
		name, typ string
		want      captureKind
	}{
		{"ctx", "context.Context", captureNone},
		{"n", "int", captureInt},
		{"n", "int64", captureInt},
		{"r", "rune", captureInt},
		{"n", "uint32", captureUint},
		{"b", "byte", captureUint},
		{"f", "float64", captureFloat},
		{"ok", "bool", captureBool},
		{"wait", "time.Duration", captureDuration},
		{"at", "time.Time", captureTime},
		{"orderID", "string", captureID},
		{"order_id", "string", captureID},
		{"userIDs", "string", captureID},
		{"id", "string", captureID},
		{"ids", "string", captureID},
		{"email", "string", captureLen},
		{"note", "string", captureLen},
		{"sessionID", "string", captureLen}, // a credential, ID-shaped or not
		{"apiToken", "string", captureLen},
		{"body", "[]byte", captureLen},
		{"ids", "[]string", captureCount},
		{"attrs", "map[string]string", captureCount},
		{"grid", "[4]int", captureCount},
		{"rest", "...string", captureCount},
		{"in", "*pb.Order", captureProto},
		{"in", "*bufpb.Order", captureProto},
		{"at", "*tspb.Timestamp", captureProto},
		{"status", "pb.Status", captureNamed},
		{"in", "*domain.Order", captureType},
		{"in", "domain.Order", captureType},
		{"in", "*generator.Spec", captureType},
		{"in", "*Order", captureType},
		{"in", "Order", captureType},
		{"err", "error", captureType},
		{"v", "any", captureType},
		{"fn", "func(int) error", captureType},
		{"ch", "chan int", captureType},
		{"p", "*int", captureType},
		{"s", "Status", captureInt},     // local named int
		{"ref", "OrderRef", captureLen}, // local named string, not ID-named
		{"orderRefID", "OrderRef", captureID},
		{"c", "complex128", captureType},
	} {
		if got := classifyParam(tc.name, tc.typ, imports, aliases); got != tc.want {
			t.Errorf("classifyParam(%q, %q) = %s, want %s", tc.name, tc.typ, got, tc.want)
		}
	}
}

func writeModule(t *testing.T, files map[string]string) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "internal", "orders")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files["../../go.mod"] = "module example.com/shop\n\ngo 1.27\n"
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, dir
}

func renderOrders(t *testing.T, dir string, opts Options) string {
	t.Helper()
	cf, err := ParseContract(filepath.Join(dir, "contract.go"))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := WriteObservedDecorator(cf, dir, "Service", "NewServiceWithForgeMiddleware", "forgeMiddlewareService", "orders", &DefaultChain{Scope: "example.com/shop/internal/orders", Name: "orders"}, opts)
	if err != nil || !ok {
		t.Fatalf("generated=%v err=%v", ok, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "middleware_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const ordersContract = `package orders

import (
	"context"
	"time"
)

type Service interface {
	// Place takes an order.
	// forge:no-capture coupon
	Place(ctx context.Context, orderID string, note string, qty int, coupon string, wait time.Duration, items []string, in *Input) (string, error)
	Get(ctx context.Context, orderID string) error
	// Ping is answered by an embedded helper.
	Ping(ctx context.Context) error
}

type Input struct{ SKU string }
`

func TestObservedDecoratorCodeLocationAndCapture(t *testing.T) {
	_, dir := writeModule(t, map[string]string{
		"contract.go": ordersContract,
		"service.go": `package orders

import (
	"context"
	"time"
)

type service struct{ pinger }

func New() Service { return &service{} }

func (s *service) Place(ctx context.Context, orderID, note string, qty int, coupon string, wait time.Duration, items []string, in *Input) (string, error) {
	return "", nil
}
`,
		"get.go": `package orders

import "context"

// Get is in a file of its own.
// forge:no-capture
func (s *service) Get(ctx context.Context, orderID string) error { return nil }
`,
		"ping.go": `package orders

import "context"

type pinger struct{}

func (pinger) Ping(context.Context) error { return nil }
`,
		// A mock implements every method too; it must never be taken for the
		// implementation.
		"mock_gen.go": `package orders

import (
	"context"
	"time"
)

type MockService struct{}

func (m *MockService) Place(ctx context.Context, orderID, note string, qty int, coupon string, wait time.Duration, items []string, in *Input) (string, error) {
	return "", nil
}
func (m *MockService) Get(ctx context.Context, orderID string) error { return nil }
func (m *MockService) Ping(ctx context.Context) error             { return nil }
`,
	})
	got := renderOrders(t, dir, Options{})
	for _, want := range []string{
		`Place: observe.NewOp("orders.Place", "example.com/shop/internal/orders.Service.Place", "internal/orders/service.go"),`,
		`Get:   observe.NewOp("orders.Get", "example.com/shop/internal/orders.Service.Get", "internal/orders/get.go"),`,
		// Ping is promoted from an embedded type: tied to the one type that
		// declares it.
		`Ping:  observe.NewOp("orders.Ping", "example.com/shop/internal/orders.Service.Ping", "internal/orders/ping.go"),`,
		`o.chain.AroundOp(ctx, forgeMiddlewareServiceOps.Place, forgeMiddlewareServicePlaceArgs{orderID, note, qty, items}, func(ctx context.Context) (string, error) {`,
		`r.ID("orderID", a.orderID)`,
		`r.Len("note", len(a.note))`,
		`r.Int("qty", int64(a.qty))`,
		`r.Type("wait", "Duration")`,
		`r.Count("items", len(a.items))`,
		`r.Type("in", "*Input")`,
		// Bare forge:no-capture on the implementation: nothing for Get.
		`o.chain.RunOp(ctx, forgeMiddlewareServiceOps.Get, observe.NoArgs{}, func(ctx context.Context) error {`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("decorator missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, `"coupon"`) {
		t.Errorf("forge:no-capture coupon still captured:\n%s", got)
	}
	if strings.Contains(got, "a.wait") || strings.Contains(got, "\twait ") {
		t.Errorf("a duration's value is stored or recorded:\n%s", got)
	}
	if strings.Contains(got, "\tin      *Input") || strings.Contains(got, "\tin *Input") {
		t.Errorf("a type-only parameter is stored in the capture:\n%s", got)
	}
	if strings.Contains(got, ".go:") {
		t.Errorf("a code location carries a line number:\n%s", got)
	}

	off := renderOrders(t, dir, Options{NoForensics: true})
	if strings.Contains(off, "RecordForensics") || strings.Contains(off, "PlaceArgs") {
		t.Errorf("NoForensics still generated capture code:\n%s", off)
	}
	if !strings.Contains(off, `o.chain.AroundOp(ctx, forgeMiddlewareServiceOps.Place, observe.NoArgs{},`) {
		t.Errorf("NoForensics must still carry the op:\n%s", off)
	}
}

// Without an implementation in the package the interface's own file is the
// code location.
func TestObservedDecoratorCodeLocationFallsBackToTheInterface(t *testing.T) {
	_, dir := writeModule(t, map[string]string{"contract.go": ordersContract})
	got := renderOrders(t, dir, Options{})
	if !strings.Contains(got, `"example.com/shop/internal/orders.Service.Get", "internal/orders/contract.go")`) {
		t.Errorf("fallback location missing:\n%s", got)
	}
}

// Two types implementing the whole interface: the one New returns wins.
func TestObservedDecoratorCodeLocationPrefersTheReturnedType(t *testing.T) {
	_, dir := writeModule(t, map[string]string{
		"contract.go": "package orders\nimport \"context\"\ntype Service interface{ Get(ctx context.Context, orderID string) error }\n",
		"real.go":     "package orders\nimport \"context\"\ntype real struct{}\nfunc New() Service { return &real{} }\nfunc (*real) Get(context.Context, string) error { return nil }\n",
		"fake.go":     "package orders\nimport \"context\"\ntype fake struct{}\nfunc (fake) Get(context.Context, string) error { return nil }\n",
	})
	got := renderOrders(t, dir, Options{})
	if !strings.Contains(got, `"internal/orders/real.go")`) {
		t.Errorf("returned type not preferred:\n%s", got)
	}
}

func TestParseNoCapture(t *testing.T) {
	for text, want := range map[string]noCapture{
		"// forge:no-capture":                {all: true},
		"// forge:no-capture coupon":         {params: map[string]bool{"coupon": true}},
		"// forge:no-capture: a, b":          {params: map[string]bool{"a": true, "b": true}},
		"//forge:no-capture a b":             {params: map[string]bool{"a": true, "b": true}},
		"// forge:no-captured x":             {},
		"// see forge:no-capture for detail": {},
	} {
		got := parseNoCapture(commentGroup(text))
		if got.all != want.all || len(got.params) != len(want.params) {
			t.Errorf("parseNoCapture(%q) = %+v, want %+v", text, got, want)
			continue
		}
		for p := range want.params {
			if !got.params[p] {
				t.Errorf("parseNoCapture(%q) missing %q", text, p)
			}
		}
	}
}

func commentGroup(text string) *ast.CommentGroup {
	return &ast.CommentGroup{List: []*ast.Comment{{Text: text}}}
}
