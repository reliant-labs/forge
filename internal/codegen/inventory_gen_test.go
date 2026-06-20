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

func TestGenerateInventory_NoServicesNoFile(t *testing.T) {
	dir := newInjectProject(t)
	if err := GenerateInventory(InventoryGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
	}); err != nil {
		t.Fatalf("GenerateInventory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal", "app", "inventory_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("no services should emit no inventory file")
	}
}
