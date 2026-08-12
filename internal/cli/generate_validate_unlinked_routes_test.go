// Regression test for the dogfooding finding: `forge generate` reported
// unlinked nav routes via reportUnlinkedRoutes' inline ℹ️ line, but that
// line sits mid-run among ~150 lines of ✅ output and was missed. The
// post-generation warnings block (validateGeneratedProject, printed by
// stepPostGenValidate) is read every time — this pins that the same
// finding also lands there.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

func unlinkedRoutesFixtureServices() []codegen.ServiceDef {
	return []codegen.ServiceDef{
		{
			Name:      "CustomerService",
			ProtoFile: "proto/services/customers/v1/customers.proto",
			Methods: []codegen.Method{
				{Name: "ListCustomers", InputType: "ListCustomersRequest", OutputType: "ListCustomersResponse"},
				{Name: "CreateCustomer", InputType: "CreateCustomerRequest", OutputType: "CreateCustomerResponse"},
			},
		},
	}
}

func unlinkedRoutesFixtureEntities() []codegen.EntityDef {
	return []codegen.EntityDef{{Name: "Customer", TableName: "customers"}}
}

// TestValidateGeneratedProject_ReportsUnlinkedNavRoutes is the positive
// case: a nav.tsx that the user has taken over (touched, so no longer
// pristine) and that never mentions a live entity's route must surface in
// validateGeneratedProject's warnings — the same list stepPostGenValidate
// prints under "⚠️  Post-generation warnings:".
func TestValidateGeneratedProject_ReportsUnlinkedNavRoutes(t *testing.T) {
	projectDir := t.TempDir()
	cfg := &config.ProjectConfig{
		Name: "demo",
		Frontends: []config.FrontendConfig{
			{Name: "web", Type: "nextjs"},
		},
	}
	services := unlinkedRoutesFixtureServices()
	entities := unlinkedRoutesFixtureEntities()

	// Scaffold the nav for real via the generator, then simulate the user
	// taking ownership of it (an edit) with no mention of "/customers" —
	// exactly the state a nav edited before the Customer entity existed
	// would be in.
	cs := &checksums.FileChecksums{}
	if err := generateFrontendNav(cfg, services, projectDir, entities, cs); err != nil {
		t.Fatalf("generateFrontendNav: %v", err)
	}
	navPath := filepath.Join(projectDir, "frontends", "web", "src", "components", "nav.tsx")
	if err := os.WriteFile(navPath, []byte("// hand-rolled nav, no /customers link\nexport const ALL_ROUTES = [];\n"), 0o644); err != nil {
		t.Fatalf("simulate user edit: %v", err)
	}

	warnings := validateGeneratedProject(projectDir, cfg, services, entities)

	var found string
	for _, w := range warnings {
		if strings.Contains(w, "/customers") {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("validateGeneratedProject warnings missing the unlinked /customers route; got: %v", warnings)
	}
	if !strings.Contains(found, "nav.tsx") {
		t.Errorf("warning should name the nav.tsx path; got: %q", found)
	}
}

// TestValidateGeneratedProject_PristineNavStaysSilent is the negative
// control the task requires: a freshly scaffolded, never-touched nav.tsx
// must NOT produce a warning, because forge is still keeping it current —
// it is about to add the route itself, so warning would be wrong on every
// greenfield project. This is the exact suppression reportUnlinkedRoutes
// already had; the promotion to the warnings block must not weaken it.
func TestValidateGeneratedProject_PristineNavStaysSilent(t *testing.T) {
	projectDir := t.TempDir()
	cfg := &config.ProjectConfig{
		Name: "demo",
		Frontends: []config.FrontendConfig{
			{Name: "web", Type: "nextjs"},
		},
	}
	services := unlinkedRoutesFixtureServices()
	entities := unlinkedRoutesFixtureEntities()

	cs := &checksums.FileChecksums{}
	if err := generateFrontendNav(cfg, services, projectDir, entities, cs); err != nil {
		t.Fatalf("generateFrontendNav: %v", err)
	}
	// nav.tsx is untouched since the scaffold wrote it.

	warnings := validateGeneratedProject(projectDir, cfg, services, entities)
	for _, w := range warnings {
		if strings.Contains(w, "nav.tsx") {
			t.Errorf("pristine nav must stay silent; got warning: %q (full: %v)", w, warnings)
		}
	}
}
