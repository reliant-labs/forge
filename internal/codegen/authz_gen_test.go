package codegen

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// TestAuthorizerGen_EmitsMethodErrors asserts the authorizer_gen.go.tmpl
// renders a methodErrors map entry when a method declares
// (forge.v1.method).errors. This is the LLM-visible surface — without
// this entry, an agent inspecting the handler package can't see the
// typed error contract.
func TestAuthorizerGen_EmitsMethodErrors(t *testing.T) {
	data := AuthzTemplateData{
		Package:     "svc",
		ServiceName: "Svc",
		Module:      "example.com/proj",
		Methods: []AuthzMethodData{
			{
				Procedure:    "/svc.v1.Svc/Foo",
				AuthRequired: true,
				Errors:       []string{"NotFound"},
			},
		},
	}

	out, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)

	want := `"/svc.v1.Svc/Foo": {"NotFound"}`
	if !strings.Contains(got, want) {
		t.Errorf("rendered output missing %q\n--- RENDERED ---\n%s", want, got)
	}
	if !strings.Contains(got, "var methodErrors = map[string][]string{") {
		t.Errorf("rendered output missing methodErrors declaration\n--- RENDERED ---\n%s", got)
	}
}

// TestAuthorizerGen_EmitsMultipleErrorCodes verifies the comma-separated
// emit shape — guards against a stray formatter regression in the
// {{range $i, $e := .Errors}} loop.
func TestAuthorizerGen_EmitsMultipleErrorCodes(t *testing.T) {
	data := AuthzTemplateData{
		Package:     "svc",
		ServiceName: "Svc",
		Module:      "example.com/proj",
		Methods: []AuthzMethodData{
			{
				Procedure:    "/svc.v1.Svc/Foo",
				AuthRequired: true,
				Errors:       []string{"NotFound", "PermissionDenied", "InvalidArgument"},
			},
		},
	}

	out, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)

	want := `"/svc.v1.Svc/Foo": {"NotFound", "PermissionDenied", "InvalidArgument"}`
	if !strings.Contains(got, want) {
		t.Errorf("rendered output missing %q\n--- RENDERED ---\n%s", want, got)
	}
}

// TestAuthorizerGen_OmitsEmptyErrors verifies that methods with no
// declared errors are omitted from the methodErrors map. An empty
// `"proc": {}` would be noise (and would compile as an empty slice
// distinct from "no entry") — keeping unannotated methods out means
// the contract surface is unambiguous.
func TestAuthorizerGen_OmitsEmptyErrors(t *testing.T) {
	data := AuthzTemplateData{
		Package:     "svc",
		ServiceName: "Svc",
		Module:      "example.com/proj",
		Methods: []AuthzMethodData{
			{
				Procedure:    "/svc.v1.Svc/Foo",
				AuthRequired: true,
				Errors:       nil,
			},
			{
				Procedure:    "/svc.v1.Svc/Bar",
				AuthRequired: true,
				Errors:       []string{}, // empty slice, same as unset
			},
		},
	}

	out, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)

	// Other maps (methodRoles, methodAuthRequired) legitimately
	// reference these procedures, so scope the omission check to the
	// methodErrors block only.
	errsBlock := mustExtractMapBlock(t, got, "methodErrors")
	if strings.Contains(errsBlock, `"/svc.v1.Svc/Foo":`) {
		t.Errorf("Foo (no errors) should be omitted from methodErrors\n--- BLOCK ---\n%s", errsBlock)
	}
	if strings.Contains(errsBlock, `"/svc.v1.Svc/Bar":`) {
		t.Errorf("Bar (empty-slice errors) should be omitted from methodErrors\n--- BLOCK ---\n%s", errsBlock)
	}
	// The map itself must still be declared so the package compiles
	// when other code references it.
	if !strings.Contains(got, "var methodErrors = map[string][]string{") {
		t.Errorf("methodErrors var declaration missing even when all methods are unannotated\n--- RENDERED ---\n%s", got)
	}
}

// mustExtractMapBlock returns the text between `var <name> = map[...]{`
// and the matching closing `}` — used to assert on a specific generated
// map without false positives from sibling maps that legitimately list
// the same procedure paths.
func mustExtractMapBlock(t *testing.T, src, name string) string {
	t.Helper()
	marker := "var " + name + " = map["
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("var %s declaration not found", name)
	}
	open := strings.Index(src[i:], "{")
	if open < 0 {
		t.Fatalf("opening brace for %s not found", name)
	}
	open += i
	close := strings.Index(src[open:], "\n}")
	if close < 0 {
		t.Fatalf("closing brace for %s not found", name)
	}
	return src[open : open+close+2]
}

// TestAuthorizerGen_EmitsFailClosed pins the generated default: the
// authorizer shim must construct a fail-closed RolesDecider. Any
// reference to a permissive fail mode in the generated file is a
// security regression — dev permissiveness is the DevAuthorizer swap's
// job, not the policy table's.
func TestAuthorizerGen_EmitsFailClosed(t *testing.T) {
	data := AuthzTemplateData{
		Package:     "svc",
		ServiceName: "Svc",
		Module:      "example.com/proj",
		Methods: []AuthzMethodData{
			{Procedure: "/svc.v1.Svc/Foo", AuthRequired: true},
		},
	}

	out, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)

	if strings.Contains(got, "FailOpen") {
		t.Errorf("generated authorizer must not reference FailOpen (it no longer exists; zero value is FailClosed)\n--- RENDERED ---\n%s", got)
	}
	if strings.Contains(got, "AllowUnknownMethods") {
		t.Errorf("generated authorizer must not opt into AllowUnknownMethods\n--- RENDERED ---\n%s", got)
	}
}

// TestAuthorizerGen_AuthzCustomFailsClosed pins the fail-close fix for
// authz_custom methods: a method that delegates its decision to a
// hand-written authorizer carries NO declared roles, so a naive emit would
// write it into methodRoles with EMPTY roles — which on this table reads as
// "any authenticated user allowed", a latent grant. The template must instead
// emit it with the fail-closed sentinel role (no caller ever holds it), so the
// generated table can never be misread as an any-authenticated grant. The LIVE
// decision is the descriptor-driven RoleInterceptor + authorizer.go, never
// this table.
func TestAuthorizerGen_AuthzCustomFailsClosed(t *testing.T) {
	data := AuthzTemplateData{
		Package:     "svc",
		ServiceName: "Svc",
		Module:      "example.com/proj",
		Methods: []AuthzMethodData{
			{Procedure: "/svc.v1.Svc/Custom", AuthRequired: true, AuthzCustom: true},
			{Procedure: "/svc.v1.Svc/Open", AuthRequired: true}, // ordinary any-authenticated
		},
	}

	out, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)
	rolesBlock := mustExtractMapBlock(t, got, "methodRoles")

	// The custom method must NOT be emitted with empty roles — that is the
	// any-authenticated-grant footgun this fix exists to kill.
	if strings.Contains(rolesBlock, `"/svc.v1.Svc/Custom": { }`) ||
		strings.Contains(rolesBlock, `"/svc.v1.Svc/Custom": {}`) {
		t.Errorf("authz_custom method emitted with EMPTY roles (reads as any-authenticated grant)\n--- BLOCK ---\n%s", rolesBlock)
	}
	// It must be emitted fail-closed via the sentinel role.
	if !strings.Contains(rolesBlock, `"/svc.v1.Svc/Custom": {authzCustomSentinel}`) {
		t.Errorf("authz_custom method must be emitted with the fail-closed authzCustomSentinel role\n--- BLOCK ---\n%s", rolesBlock)
	}
	// The sentinel constant must be declared so the package compiles.
	if !strings.Contains(got, "const authzCustomSentinel =") {
		t.Errorf("authzCustomSentinel constant declaration missing\n--- RENDERED ---\n%s", got)
	}
	// An ORDINARY any-authenticated method (no roles, not custom) keeps its
	// empty-roles emit — the fix must not change non-custom behaviour.
	if !strings.Contains(rolesBlock, `"/svc.v1.Svc/Open": { }`) {
		t.Errorf("ordinary any-authenticated method should keep empty-roles emit\n--- BLOCK ---\n%s", rolesBlock)
	}
}

// TestBuildAuthzMethods_CarriesAuthzCustom verifies the flag is plumbed from
// the parsed Method onto every emitted table entry — including the CRUD
// "<action>:<resource>" alias, so a custom-authz CRUD RPC's alias is fail-
// closed too (not just its procedure-path entry).
func TestBuildAuthzMethods_CarriesAuthzCustom(t *testing.T) {
	svc := ServiceDef{
		Name:    "PatientService",
		Package: "patients.v1",
		Methods: []Method{
			{Name: "CreatePatient", AuthRequired: true, AuthzCustom: true},
		},
	}
	entities := []EntityDef{{Name: "Patient"}}

	methods := BuildAuthzMethods(svc, entities)
	custom := make(map[string]bool, len(methods))
	for _, m := range methods {
		custom[m.Procedure] = m.AuthzCustom
	}
	if !custom["/patients.v1.PatientService/CreatePatient"] {
		t.Error("procedure-path entry lost AuthzCustom flag")
	}
	if !custom["create:patient"] {
		t.Error("CRUD alias entry lost AuthzCustom flag — its table entry would be an empty-roles grant")
	}
}

// TestBuildAuthzMethods_EmitsCRUDActionAliases pins the one-key-universe
// fix: the table the authorizer generator emits must contain the
// "<action>:<resource>" keys the generated CRUD handler bodies pass to
// Can(). Before this change the table held ONLY procedure paths, so
// every Can() check hit the unknown-method branch forever.
func TestBuildAuthzMethods_EmitsCRUDActionAliases(t *testing.T) {
	svc := ServiceDef{
		Name:    "PatientService",
		Package: "patients.v1",
		Methods: []Method{
			{Name: "CreatePatient", AuthRequired: true, RequiredRoles: []string{"admin"}},
			{Name: "GetPatient", AuthRequired: true},
			{Name: "ListPatients", AuthRequired: true},
			{Name: "Echo", AuthRequired: true}, // non-CRUD: no alias
		},
	}
	entities := []EntityDef{{Name: "Patient"}}

	methods := BuildAuthzMethods(svc, entities)
	keys := make(map[string][]string, len(methods))
	authReq := make(map[string]bool, len(methods))
	for _, m := range methods {
		keys[m.Procedure] = m.RequiredRoles
		authReq[m.Procedure] = m.AuthRequired
	}

	// Procedure paths still present.
	for _, p := range []string{
		"/patients.v1.PatientService/CreatePatient",
		"/patients.v1.PatientService/Echo",
	} {
		if _, ok := keys[p]; !ok {
			t.Errorf("procedure key %q missing from table", p)
		}
	}
	// CRUD aliases present, carrying the source RPC's roles/auth flags.
	if roles, ok := keys["create:patient"]; !ok {
		t.Error(`alias key "create:patient" missing — Can() checks will hit the unknown branch`)
	} else if len(roles) != 1 || roles[0] != "admin" {
		t.Errorf(`alias "create:patient" roles = %v, want [admin]`, roles)
	}
	if !authReq["create:patient"] {
		t.Error(`alias "create:patient" must carry AuthRequired=true from CreatePatient`)
	}
	// Get → "read" (middleware.ActionRead), List keeps "list".
	if _, ok := keys["read:patient"]; !ok {
		t.Error(`alias key "read:patient" missing (Get maps to the read action)`)
	}
	if _, ok := keys["list:patient"]; !ok {
		t.Error(`alias key "list:patient" missing`)
	}
	// Non-CRUD methods get no alias.
	if _, ok := keys["echo:patient"]; ok {
		t.Error("non-CRUD method must not produce an alias key")
	}
}

// TestVerifyCanKeyUniverse_CatchesDrift proves the generate-time check
// is independent of the emission: a table missing a Can() key (here, a
// doctored procedure-only table) must fail verification.
func TestVerifyCanKeyUniverse_CatchesDrift(t *testing.T) {
	svc := ServiceDef{
		Name:    "PatientService",
		Package: "patients.v1",
		Methods: []Method{{Name: "CreatePatient", AuthRequired: true}},
	}
	entities := []EntityDef{{Name: "Patient"}}

	// Complete table passes.
	full := BuildAuthzMethods(svc, entities)
	if err := VerifyCanKeyUniverse(svc, entities, full); err != nil {
		t.Fatalf("complete table should verify clean: %v", err)
	}

	// Procedure-only table (the pre-fix shape) fails loudly.
	drifted := []AuthzMethodData{
		{Procedure: "/patients.v1.PatientService/CreatePatient", AuthRequired: true},
	}
	err := VerifyCanKeyUniverse(svc, entities, drifted)
	if err == nil {
		t.Fatal("procedure-only table must fail Can-key verification")
	}
	if !strings.Contains(err.Error(), "create:patient") {
		t.Errorf("error should name the missing key, got: %v", err)
	}
}
