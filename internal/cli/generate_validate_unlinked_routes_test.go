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

// TestValidateGeneratedProject_DeletedPageIsNotUnlinked: a route whose
// page the user DELETED must not be reported as unlinked. There is
// nothing to link to, so "add it to ALL_ROUTES" would produce a 404.
//
// control-plane's internal-console hit this on every generate. It was
// told to add /daemons, /plans, /deployments and /llm-keys — four pages
// it had deliberately removed, because they are owner-scoped customer
// surfaces that could only ever render empty against the operator
// listener. The same generate output listed all thirteen of those
// pages' files under "scaffold-once file(s) … left absent on purpose",
// so forge contradicted itself within one run.
//
// Deleting a scaffold-once file is an act of ownership the ledger
// records as recorded-and-absent, which is the signal used here.
func TestValidateGeneratedProject_DeletedPageIsNotUnlinked(t *testing.T) {
	projectDir := t.TempDir()
	cfg := &config.ProjectConfig{
		Name:      "demo",
		Frontends: []config.FrontendConfig{{Name: "web", Type: "nextjs"}},
	}
	services := unlinkedRoutesFixtureServices()
	entities := unlinkedRoutesFixtureEntities()

	cs := &checksums.FileChecksums{}
	if err := generateFrontendNav(cfg, services, projectDir, entities, cs); err != nil {
		t.Fatalf("generateFrontendNav: %v", err)
	}

	// The user takes over the nav and drops the /customers link...
	navPath := filepath.Join(projectDir, "frontends", "web", "src", "components", "nav.tsx")
	if err := os.WriteFile(navPath, []byte("// hand-rolled nav, no /customers link\nexport const ALL_ROUTES = [];\n"), 0o644); err != nil {
		t.Fatalf("simulate user edit: %v", err)
	}

	// ...because they deleted the page it pointed at. Record the birth
	// and remove the file: recorded && absent is how the ledger spells
	// "the user deleted this".
	pageRel := filepath.Join("frontends", "web", "src", "app", "customers", "page.tsx")
	pageAbs := filepath.Join(projectDir, pageRel)
	if err := os.MkdirAll(filepath.Dir(pageAbs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(pageAbs, []byte("export default function P() { return null }\n"), 0o644); err != nil {
		t.Fatalf("write page: %v", err)
	}
	checksums.RecordScaffold(projectDir, pageRel)
	checksums.ResetScaffoldLedgerCache()
	if err := os.Remove(pageAbs); err != nil {
		t.Fatalf("delete page: %v", err)
	}

	for _, w := range validateGeneratedProject(projectDir, cfg, services, entities) {
		if strings.Contains(w, "/customers") {
			t.Errorf("a deleted page must not be reported as an unlinked route — "+
				"linking to it would be a 404; got: %q", w)
		}
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
