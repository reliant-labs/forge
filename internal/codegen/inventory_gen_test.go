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
			{Name: "BillingService", Package: "billing.v1", ModulePath: "example.com/proj"},
			{Name: "UserService", Package: "user.v1", ModulePath: "example.com/proj"},
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
	// version-aware BaseService/Version metadata + Kind, and a typed Mount
	// closure over *Services.
	for _, want := range []string{
		`var Inventory = []ComponentInfo{`,
		`Name:        "billing",`,
		`Name:        "user",`,
		`BaseService: "billing",`,
		`BaseService: "user",`,
		`Version:     "v1",`,
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

// TestGenerateInventory_VersionMetadata: the version-aware seam records the
// proto API version as EXPLICIT metadata derived from the descriptor's proto
// package, while leaving today's v1 mount path/keying untouched. A
// higher-version package (v2beta1) flows through as Version metadata without
// any change to identity — the precondition for additive multi-version
// support. An unversioned package records an empty Version.
func TestGenerateInventory_VersionMetadata(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "shop", "shop", "\tLogger *slog.Logger")
	writeComponentDeps(t, dir, "internal/handlers", "legacy", "legacy", "\tLogger *slog.Logger")

	err := GenerateInventory(InventoryGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services: []ServiceDef{
			{Name: "ShopService", Package: "acme.shop.v2beta1", ModulePath: "example.com/proj"},
			{Name: "LegacyService", Package: "legacy", ModulePath: "example.com/proj"}, // unversioned
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
	for _, want := range []string{
		`BaseService: "shop",`,
		`Version:     "v2beta1",`,
		`BaseService: "legacy",`,
		`Version:     "",`, // unversioned proto package
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inventory missing version metadata %q:\n%s", want, out)
		}
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
