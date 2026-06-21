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

	// Data-only descriptor rows: Name (display) + ConnectPath + version-aware
	// BaseService/Version metadata + Kind. NO Mount closure on the row.
	for _, want := range []string{
		// Descriptor TYPE lives in forge/pkg/mountkit/inventory now.
		`"github.com/reliant-labs/forge/pkg/mountkit/inventory"`,
		`var Inventory = []inventory.ComponentInfo{`,
		`Name:        "billing",`,
		`Name:        "user",`,
		`BaseService: "billing",`,
		`BaseService: "user",`,
		`Version:     "v1",`,
		`Kind:        "service",`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inventory missing %q:\n%s", want, out)
		}
	}

	// TYPED MOUNTS — the run path. One typed method per service + MountAll +
	// the typed MountByName map of method expressions (no string lookup).
	for _, want := range []string{
		`func (s *Services) MountBilling(mux *http.ServeMux`,
		`func (s *Services) MountUser(mux *http.ServeMux`,
		`func (s *Services) MountAll(mux *http.ServeMux`,
		`s.Billing.Register(`,
		`s.User.Register(`,
		`s.Billing.RegisterHTTP(`,
		`var MountByName = map[string]MountFunc{`,
		`"billing": (*Services).MountBilling,`,
		`"user": (*Services).MountUser,`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("typed mounts missing %q:\n%s", want, out)
		}
	}

	// The descriptor row must NOT carry a Mount closure anymore.
	if strings.Contains(out, `Mount: func(`) {
		t.Fatalf("ComponentInfo row must be data-only (no Mount closure):\n%s", out)
	}

	// The authorizer-bearing service threads its authz interceptor in its
	// typed mount; the authorizer-free one does not construct an authz var.
	billingMount := out[strings.Index(out, `func (s *Services) MountBilling`):]
	if next := strings.Index(billingMount, "func (s *Services) MountUser"); next > 0 {
		billingMount = billingMount[:next]
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
// inventory file is STILL emitted as a valid empty []inventory.ComponentInfo. The
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
	if !strings.Contains(out, `var Inventory = []inventory.ComponentInfo{`) {
		t.Fatalf("empty inventory should still declare Inventory:\n%s", out)
	}
	// No service rows in the empty case.
	if strings.Contains(out, `Kind:        "service"`) {
		t.Fatalf("no-services inventory should have no rows:\n%s", out)
	}
}
