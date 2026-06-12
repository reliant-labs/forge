package templates

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// bootstrapGuardSvc is the minimal service shape the bootstrap template
// needs for the registration-guard tests below.
type bootstrapGuardSvc struct {
	Name, Package, ImportPath, FieldName, Alias string
	Fallible, HasWebhooks                       bool
	ConnectPkg, ProtoServiceName                string
}

func mkBootstrapGuardData(services []bootstrapGuardSvc, allNames []string) any {
	return struct {
		Module              string
		HasDatabase         bool
		DatabaseDriver      string
		OrmEnabled          bool
		LeaderElectionID    string
		Services            []bootstrapGuardSvc
		Packages            []struct{}
		Workers             []struct{}
		Operators           []struct{}
		HasFallible         bool
		ConfigFields        map[string]bool
		RESTEnabled         bool
		ConnectImports      []string
		DiagnosticsEnabled  bool
		StrictWiringEnabled bool
		AllServiceNames     []string
	}{
		Module:           "example.com/myproject",
		LeaderElectionID: "myproject-leader",
		Services:         services,
		ConfigFields:     map[string]bool{},
		AllServiceNames:  allNames,
	}
}

// TestBootstrapTemplate_RegistrationGuard pins BootstrapOnly's
// registration guard: the project's full service inventory renders into
// a name list, names in the inventory are checked against the LIVE rows
// RegisteredServices returned (def.Services), and an inventory name
// with no row fails with a pointed error naming pkg/app/services.go.
// Unknown names fall through to appkit's warning.
func TestBootstrapTemplate_RegistrationGuard(t *testing.T) {
	apiSvc := bootstrapGuardSvc{Name: "api", Package: "api", ImportPath: "api", FieldName: "API", Alias: "apihandler"}

	t.Run("guard-renders-with-inventory", func(t *testing.T) {
		content, err := ProjectTemplates().Render("bootstrap.go.tmpl",
			mkBootstrapGuardData([]bootstrapGuardSvc{apiSvc}, []string{"api", "project"}))
		if err != nil {
			t.Fatalf("Render bootstrap.go.tmpl: %v", err)
		}
		rendered := string(content)
		for _, want := range []string{
			`[]string{"api", "project"}`,
			"not registered in pkg/app/services.go",
			"RegisteredServices(app, cfg, logger, devMode, opts...)",
			"range def.Services",
		} {
			if !strings.Contains(rendered, want) {
				t.Errorf("rendered bootstrap missing %q", want)
			}
		}
		// The table must NOT inline per-service rows anymore — rows live
		// in services_gen.go and the user picks them in services.go.
		if strings.Contains(rendered, "Construct: func() (appkit.Mounter, error)") {
			t.Errorf("bootstrap must not inline service Construct closures (rows moved to services_gen.go)")
		}
		fset := token.NewFileSet()
		if _, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors); parseErr != nil {
			t.Fatalf("rendered bootstrap.go does not parse:\n%v\n\nSource:\n%s", parseErr, rendered)
		}
	})

	t.Run("no-guard-without-services", func(t *testing.T) {
		content, err := ProjectTemplates().Render("bootstrap.go.tmpl", mkBootstrapGuardData(nil, nil))
		if err != nil {
			t.Fatalf("Render bootstrap.go.tmpl: %v", err)
		}
		rendered := string(content)
		if strings.Contains(rendered, "not registered in pkg/app/services.go") {
			t.Errorf("zero-inventory bootstrap must not carry the registration guard")
		}
		if strings.Contains(rendered, "RegisteredServices(") {
			t.Errorf("zero-service bootstrap must not call RegisteredServices")
		}
		fset := token.NewFileSet()
		if _, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors); parseErr != nil {
			t.Fatalf("rendered zero-service bootstrap.go does not parse:\n%v\n\nSource:\n%s", parseErr, rendered)
		}
	})
}

// TestServicesGenTemplate_RowConstructors pins the generated row
// constructor shape (one serviceRow<FieldName> per service, wire call +
// New + Register closure) and the header-only degradation with zero
// services.
func TestServicesGenTemplate_RowConstructors(t *testing.T) {
	type svc struct {
		Name, Package, ImportPath, FieldName, Alias string
		Fallible, HasWebhooks                       bool
		ConnectPkg, ProtoServiceName                string
	}
	mkData := func(services []svc) any {
		return struct {
			Module         string
			HasDatabase         bool
			DatabaseDriver      string
			OrmEnabled          bool
			Services       []svc
			RESTEnabled    bool
			ConnectImports []string
		}{
			Module:   "example.com/myproject",
			Services: services,
		}
	}

	t.Run("rows-render", func(t *testing.T) {
		content, err := ProjectTemplates().Render("services_gen.go.tmpl", mkData([]svc{
			{Name: "api", Package: "api", ImportPath: "api", FieldName: "API", Alias: "apihandler"},
			{Name: "billing", Package: "billing", ImportPath: "billing", FieldName: "Billing", Alias: "billing", HasWebhooks: true},
		}))
		if err != nil {
			t.Fatalf("Render services_gen.go.tmpl: %v", err)
		}
		rendered := string(content)
		for _, want := range []string{
			"func serviceRowAPI(app *App, cfg *config.Config, logger *slog.Logger, devMode bool, opts ...connect.HandlerOption) appkit.ServiceDef",
			"func serviceRowBilling(app *App",
			"wireAPIDeps(app, cfg, logger, devMode)",
			"middleware.AuthzInterceptor(apiDeps.Authorizer)",
			"app.Services.Billing.RegisterWebhookRoutes(mux, fmw.HTTPStack(logger, middleware.ClaimsFromContext))",
		} {
			if !strings.Contains(rendered, want) {
				t.Errorf("rendered services_gen missing %q", want)
			}
		}
		fset := token.NewFileSet()
		if _, parseErr := parser.ParseFile(fset, "services_gen.go", rendered, parser.AllErrors); parseErr != nil {
			t.Fatalf("rendered services_gen.go does not parse:\n%v\n\nSource:\n%s", parseErr, rendered)
		}
	})

	t.Run("zero-services-header-only", func(t *testing.T) {
		content, err := ProjectTemplates().Render("services_gen.go.tmpl", mkData(nil))
		if err != nil {
			t.Fatalf("Render services_gen.go.tmpl: %v", err)
		}
		rendered := string(content)
		if strings.Contains(rendered, "import") {
			t.Errorf("zero-service services_gen must not declare imports:\n%s", rendered)
		}
		fset := token.NewFileSet()
		if _, parseErr := parser.ParseFile(fset, "services_gen.go", rendered, parser.AllErrors); parseErr != nil {
			t.Fatalf("rendered zero-service services_gen.go does not parse:\n%v\n\nSource:\n%s", parseErr, rendered)
		}
	})
}

// TestServicesRegistryTemplate pins the scaffold-once user-owned
// pkg/app/services.go: one row per service, the empty-list zero-service
// shape, and the tombstone-comment instructions the parser depends on.
func TestServicesRegistryTemplate(t *testing.T) {
	type svc struct{ FieldName string }
	mkData := func(services []svc) any {
		return struct {
			Module   string
			HasDatabase         bool
			DatabaseDriver      string
			OrmEnabled          bool
			Services []svc
		}{Module: "example.com/myproject", Services: services}
	}

	t.Run("rows-listed", func(t *testing.T) {
		content, err := ProjectTemplates().Render("services.go.tmpl", mkData([]svc{{FieldName: "API"}, {FieldName: "Billing"}}))
		if err != nil {
			t.Fatalf("Render services.go.tmpl: %v", err)
		}
		rendered := string(content)
		for _, want := range []string{
			"func RegisteredServices(app *App, cfg *config.Config, logger *slog.Logger, devMode bool, opts ...connect.HandlerOption) []appkit.ServiceDef",
			"serviceRowAPI(app, cfg, logger, devMode, opts...),",
			"serviceRowBilling(app, cfg, logger, devMode, opts...),",
			// The comment contract the registry parser depends on must be
			// spelled out for the user/agent editing the file.
			"leave a comment",
		} {
			if !strings.Contains(rendered, want) {
				t.Errorf("rendered services.go missing %q", want)
			}
		}
		fset := token.NewFileSet()
		if _, parseErr := parser.ParseFile(fset, "services.go", rendered, parser.AllErrors); parseErr != nil {
			t.Fatalf("rendered services.go does not parse:\n%v\n\nSource:\n%s", parseErr, rendered)
		}
	})

	t.Run("zero-services-empty-list", func(t *testing.T) {
		content, err := ProjectTemplates().Render("services.go.tmpl", mkData(nil))
		if err != nil {
			t.Fatalf("Render services.go.tmpl: %v", err)
		}
		rendered := string(content)
		if !strings.Contains(rendered, "return []appkit.ServiceDef{}") {
			t.Errorf("zero-service registry must return the empty list:\n%s", rendered)
		}
		fset := token.NewFileSet()
		if _, parseErr := parser.ParseFile(fset, "services.go", rendered, parser.AllErrors); parseErr != nil {
			t.Fatalf("rendered zero-service services.go does not parse:\n%v\n\nSource:\n%s", parseErr, rendered)
		}
	})
}
