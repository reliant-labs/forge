package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/templates"
)

// renderMockTransport parses and executes the frontend mocks/mock-transport
// template with the given entity data. Helper for the regression tests below.
func renderMockTransport(t *testing.T, entities []codegen.MockTransportEntity) string {
	t.Helper()
	tmplContent, err := templates.FrontendTemplates().Get(filepath.Join("mocks", "mock-transport.ts.tmpl"))
	if err != nil {
		t.Fatalf("read mock-transport template: %v", err)
	}
	tmpl, err := template.New("mock-transport.ts.tmpl").Funcs(templates.FuncMap()).Parse(string(tmplContent))
	if err != nil {
		t.Fatalf("parse mock-transport template: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, codegen.MockTransportTemplateData{Entities: entities}); err != nil {
		t.Fatalf("execute mock-transport template: %v", err)
	}
	return buf.String()
}

// TestMockTransport_ListOnlyEntity_DoesNotEmitEmptySchemaImports is the
// regression test for the kalshi-trader friction round's blocker: when an
// entity exposes only a List RPC (no Get / Create / Update / Delete), the
// template used to emit
//
//	import { ListTradesResponseSchema, Schema, Schema } from "@/gen/...";
//
// because `.GetResponseType` and `.CreateResponseType` were empty strings
// and the template concatenated `{{ .X }}Schema` unconditionally. The
// duplicate `Schema` identifier blocked the dashboard's Next.js build with
// TS2300 + TS2305 errors before any user code ran. Fix: gate each schema
// import on the matching `Has*` flag.
func TestMockTransport_ListOnlyEntity_DoesNotEmitEmptySchemaImports(t *testing.T) {
	entities := []codegen.MockTransportEntity{
		{
			EntityName:       "Trade",
			EntityNamePlural: "Trades",
			EntitySlug:       "trades",
			ServiceName:      "TradingService",
			ServiceTypeName:  "kalshi.v1.TradingService",
			ListRPC:          "ListTrades",
			HasList:          true,
			ImportPath:       "services/api/v1/api_pb",
			ListResponseType: "ListTradesResponse",
		},
		{
			EntityName:       "Hypothesis",
			EntityNamePlural: "Hypotheses",
			EntitySlug:       "hypotheses",
			ServiceName:      "TradingService",
			ServiceTypeName:  "kalshi.v1.TradingService",
			ListRPC:          "ListHypotheses",
			HasList:          true,
			ImportPath:       "services/api/v1/api_pb",
			ListResponseType: "ListHypothesesResponse",
		},
	}

	got := renderMockTransport(t, entities)

	// The exact pre-fix substring that broke the kalshi-trader build —
	// `Schema, Schema` was a duplicate-identifier TS2300 error AND was not
	// exported from api_pb (TS2305).
	if strings.Contains(got, "Schema, Schema") {
		t.Errorf("rendered template still contains duplicate `Schema, Schema` import — TS2300/TS2305 regression. Output:\n%s", got)
	}

	// The named imports should appear — these ARE exported and ARE used.
	for _, want := range []string{
		"ListTradesResponseSchema",
		"ListHypothesesResponseSchema",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected named import %q in rendered output, got:\n%s", want, got)
		}
	}

	// Symbols for absent RPCs must NOT appear — an entity with HasGet=false
	// has no GetResponseType, and importing a non-existent symbol breaks
	// the next.js build.
	for _, unwanted := range []string{
		"GetTradeResponseSchema",
		"CreateTradeResponseSchema",
		"GetHypothesisResponseSchema",
		"CreateHypothesisResponseSchema",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("did not expect %q (entity has no Get/Create RPC) in rendered output, got:\n%s", unwanted, got)
		}
	}
}

// TestMockTransport_FullCRUD_EmitsAllSchemaImports asserts the canonical CRUD
// path still emits every needed Schema import. Guards against an overzealous
// fix to the import-gating block above breaking the standard scaffold path
// (Patient/Clinic-style projects where every entity has List/Get/Create/
// Update/Delete).
func TestMockTransport_FullCRUD_EmitsAllSchemaImports(t *testing.T) {
	entities := []codegen.MockTransportEntity{
		{
			EntityName:         "Patient",
			EntityNamePlural:   "Patients",
			EntitySlug:         "patients",
			ServiceName:        "ClinicService",
			ServiceTypeName:    "demo.v1.ClinicService",
			ListRPC:            "ListPatients",
			GetRPC:             "GetPatient",
			CreateRPC:          "CreatePatient",
			UpdateRPC:          "UpdatePatient",
			DeleteRPC:          "DeletePatient",
			HasList:            true,
			HasGet:             true,
			HasCreate:          true,
			HasUpdate:          true,
			HasDelete:          true,
			ImportPath:         "services/clinic/v1/clinic_pb",
			ListResponseType:   "ListPatientsResponse",
			GetResponseType:    "GetPatientResponse",
			CreateResponseType: "CreatePatientResponse",
		},
	}

	got := renderMockTransport(t, entities)

	for _, want := range []string{
		"ListPatientsResponseSchema",
		"GetPatientResponseSchema",
		"CreatePatientResponseSchema",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in full-CRUD output, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Schema, Schema") {
		t.Errorf("full-CRUD output unexpectedly contains `Schema, Schema`:\n%s", got)
	}
}

// TestMockTransport_BindsTransportVariableNotCastAtReturn is the regression
// test for the second kalshi-trader friction: the template used to return
// `{ async unary(...) {...}, async stream(...) {...} } as unknown as Transport`,
// and that trailing cast does NOT propagate Connect's `Transport` interface
// backwards into the callback parameter types. Under strict tsc every
// `unary` / `stream` parameter (method, signal, timeoutMs, header, input,
// contextValues) errored with TS7006 "Parameter X implicitly has an any
// type" — 12 errors per file. The fix: bind the object literal to a
// `const transport: Transport = { ... }` variable up front, then return it,
// so tsc has the interface available when checking the method bodies.
func TestMockTransport_BindsTransportVariableNotCastAtReturn(t *testing.T) {
	entities := []codegen.MockTransportEntity{
		{
			EntityName:       "Trade",
			EntityNamePlural: "Trades",
			EntitySlug:       "trades",
			ServiceName:      "TradingService",
			ServiceTypeName:  "kalshi.v1.TradingService",
			ListRPC:          "ListTrades",
			HasList:          true,
			ImportPath:       "services/api/v1/api_pb",
			ListResponseType: "ListTradesResponse",
		},
	}

	got := renderMockTransport(t, entities)

	// The variable-binding pattern: tsc gets Transport's parameter types
	// before it checks the method bodies.
	if !strings.Contains(got, "const transport: Transport = {") {
		t.Errorf("expected `const transport: Transport = {` binding in output (so callback param types are inferred). Got:\n%s", got)
	}
	if !strings.Contains(got, "return transport;") {
		t.Errorf("expected `return transport;` at the end of createMockTransport. Got:\n%s", got)
	}

	// The cast-at-the-end pattern would still type-check the OUTER return
	// shape but fail TS7006 on every method parameter. Guard against it
	// creeping back via a future template edit.
	if strings.Contains(got, "as unknown as Transport") {
		t.Errorf("template should not use `as unknown as Transport` cast (TS7006 on callback params). Got:\n%s", got)
	}
}
