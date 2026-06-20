package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateInventory_DataOnlyRowsAndMount(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "billing", "billing", "\tAuthorizer middleware.Authorizer")
	writeComponentDeps(t, dir, "internal/handlers", "user", "user", "\tLogger *slog.Logger")

	err := GenerateInventory(InventoryGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services: []ServiceDef{
			{Name: "BillingService", ModulePath: "example.com/proj"},
			{Name: "UserService", ModulePath: "example.com/proj"},
		},
	})
	if err != nil {
		t.Fatalf("GenerateInventory: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "internal", "app", "inventory_gen.go"))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	out := string(data)

	// Data-only descriptor rows: Name (display/selection) + ConnectPath +
	// Kind, and a typed Mount closure over *Services.
	for _, want := range []string{
		`var Inventory = []ComponentInfo{`,
		`Name:        "billing",`,
		`Name:        "user",`,
		`Kind:        "service",`,
		`Mount: func(s *Services, mux *http.ServeMux`,
		`s.Billing.Register(`,
		`s.User.Register(`,
		`s.Billing.RegisterHTTP(`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inventory missing %q:\n%s", want, out)
		}
	}

	// The authorizer-bearing service threads its authz interceptor; the
	// authorizer-free one does not construct an authz var.
	billingMount := out[strings.Index(out, `Name:        "billing"`):]
	if i := strings.Index(billingMount, "Name:"); i > 0 {
		// limit to billing's row by cutting at the next row start
		if next := strings.Index(billingMount[5:], "Name:"); next > 0 {
			billingMount = billingMount[:next+5]
		}
	}
	if !strings.Contains(billingMount, "AuthzInterceptor(authz)") {
		t.Fatalf("billing mount should thread authz interceptor:\n%s", billingMount)
	}
}

// TestGenerateInventory_NoServicesEmptyInventory: with no services, the
// inventory file is STILL emitted as a valid empty []ComponentInfo. The
// generated cmd/server.go imports internal/app and references app.Inventory
// unconditionally, so the symbol must exist even on a service-less tree —
// otherwise the package would be empty and `go mod tidy` would 404 trying to
// resolve the local import remotely (the §2 regression this guards against).
func TestGenerateInventory_NoServicesEmptyInventory(t *testing.T) {
	dir := newInjectProject(t)
	if err := GenerateInventory(InventoryGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
	}); err != nil {
		t.Fatalf("GenerateInventory: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "internal", "app", "inventory_gen.go"))
	if err != nil {
		t.Fatalf("no-services run should still emit inventory_gen.go: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, `var Inventory = []ComponentInfo{`) {
		t.Fatalf("empty inventory should still declare Inventory:\n%s", out)
	}
	// No service rows in the empty case.
	if strings.Contains(out, `Kind:        "service"`) {
		t.Fatalf("no-services inventory should have no rows:\n%s", out)
	}
}
