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
