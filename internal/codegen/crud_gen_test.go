package codegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchCRUDMethods_BasicMatching(t *testing.T) {
	entities := []EntityDef{
		{
			Name:      "Patient",
			TableName: "patients",
			PkField:   "id",
			PkGoType:  "int64",
			Fields: []EntityField{
				{Name: "id", GoName: "ID", GoType: "int64"},
				{Name: "name", GoName: "Name", GoType: "string"},
			},
		},
	}

	svc := ServiceDef{
		Name: "PatientsService",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
			{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
			{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			{Name: "UpdatePatient", InputType: "UpdatePatientRequest", OutputType: "UpdatePatientResponse"},
			{Name: "DeletePatient", InputType: "DeletePatientRequest", OutputType: "DeletePatientResponse"},
		},
	}

	matches := MatchCRUDMethods(svc, entities)

	if len(matches) != 5 {
		t.Fatalf("expected 5 matches, got %d", len(matches))
	}

	expected := []struct {
		name string
		op   string
	}{
		{"CreatePatient", "create"},
		{"GetPatient", "get"},
		{"ListPatients", "list"},
		{"UpdatePatient", "update"},
		{"DeletePatient", "delete"},
	}

	for i, exp := range expected {
		if matches[i].Method.Name != exp.name {
			t.Errorf("match[%d].Method.Name = %q, want %q", i, matches[i].Method.Name, exp.name)
		}
		if matches[i].Operation != exp.op {
			t.Errorf("match[%d].Operation = %q, want %q", i, matches[i].Operation, exp.op)
		}
		if matches[i].Entity.Name != "Patient" {
			t.Errorf("match[%d].Entity.Name = %q, want Patient", i, matches[i].Entity.Name)
		}
	}
}

func TestMatchCRUDMethods_NoEntityMatch(t *testing.T) {
	entities := []EntityDef{
		{Name: "Invoice", TableName: "invoices", PkField: "id", PkGoType: "int64"},
	}

	svc := ServiceDef{
		Name: "PatientsService",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
		},
	}

	matches := MatchCRUDMethods(svc, entities)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for unrelated entity, got %d", len(matches))
	}
}

func TestMatchCRUDMethods_SkipsStreamingMethods(t *testing.T) {
	entities := []EntityDef{
		{Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64"},
	}

	svc := ServiceDef{
		Name: "PatientsService",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse", ServerStreaming: true},
		},
	}

	matches := MatchCRUDMethods(svc, entities)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for streaming methods, got %d", len(matches))
	}
}

func TestMatchCRUDMethods_NonCRUDMethodsIgnored(t *testing.T) {
	entities := []EntityDef{
		{Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64"},
	}

	svc := ServiceDef{
		Name: "PatientsService",
		Methods: []Method{
			{Name: "SearchPatients", InputType: "SearchPatientsRequest", OutputType: "SearchPatientsResponse"},
			{Name: "ArchivePatient", InputType: "ArchivePatientRequest", OutputType: "ArchivePatientResponse"},
		},
	}

	matches := MatchCRUDMethods(svc, entities)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for non-CRUD methods, got %d", len(matches))
	}
}

func TestParseCRUDOperation(t *testing.T) {
	tests := []struct {
		input    string
		wantOp   string
		wantName string
	}{
		{"CreatePatient", "create", "Patient"},
		{"GetPatient", "get", "Patient"},
		{"ListPatients", "list", "Patients"},
		{"UpdatePatient", "update", "Patient"},
		{"DeletePatient", "delete", "Patient"},
		{"SearchPatients", "", ""},  // not CRUD
		{"ArchivePatient", "", ""},  // not CRUD
		{"Create", "", ""},          // prefix only, no entity name
		{"Get", "", ""},
		{"ProcessPayment", "", ""},
	}

	for _, tt := range tests {
		op, name := parseCRUDOperation(tt.input)
		if op != tt.wantOp || name != tt.wantName {
			t.Errorf("parseCRUDOperation(%q) = (%q, %q), want (%q, %q)",
				tt.input, op, name, tt.wantOp, tt.wantName)
		}
	}
}

func TestGenerateCRUDHandlers_SkipsExistingMethods(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a service.go with Deps struct containing DB field
	serviceGo := `package patients

import "github.com/reliant-labs/forge/pkg/orm"

type Deps struct {
	DB orm.Context
}

type Service struct {
	deps Deps
}
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(serviceGo), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write a handlers.go that already implements CreatePatient
	handlersGo := `package patients

import (
	"context"
	"connectrpc.com/connect"
	pb "example.com/test/gen/proto/services/patients/v1"
)

func (s *Service) CreatePatient(ctx context.Context, req *connect.Request[pb.CreatePatientRequest]) (*connect.Response[pb.CreatePatientResponse], error) {
	return nil, nil
}
`
	if err := os.WriteFile(filepath.Join(handlerDir, "handlers.go"), []byte(handlersGo), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
			{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
		},
	}

	entities := []EntityDef{
		{
			Name:      "Patient",
			TableName: "patients",
			PkField:   "id",
			PkGoType:  "int64",
			Fields: []EntityField{
				{Name: "id", GoName: "ID", GoType: "int64"},
				{Name: "name", GoName: "Name", GoType: "string"},
			},
		},
	}

	crudMethods := MatchCRUDMethods(svc, entities)

	err := GenerateCRUDHandlers(svc, crudMethods, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDHandlers() error = %v", err)
	}

	// Check that the generated file exists
	genPath := filepath.Join(handlerDir, "handlers_crud_gen.go")
	data, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatalf("generated file not found: %v", err)
	}

	content := string(data)

	// Should NOT contain CreatePatient (already in user handlers.go)
	if contains(content, "func (s *Service) CreatePatient(") {
		t.Error("generated file should not contain CreatePatient (already implemented)")
	}

	// Should contain GetPatient (not in user handlers)
	if !contains(content, "func (s *Service) GetPatient(") {
		t.Error("generated file should contain GetPatient stub")
	}
}

func TestGenerateCRUDHandlers_CleanupWhenNoMethods(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a stale crud gen file
	genPath := filepath.Join(handlerDir, "handlers_crud_gen.go")
	if err := os.WriteFile(genPath, []byte("package patients\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write handlers.go that already implements everything
	handlersGo := `package patients

import (
	"context"
	"connectrpc.com/connect"
	pb "example.com/test/gen/proto/services/patients/v1"
)

func (s *Service) CreatePatient(ctx context.Context, req *connect.Request[pb.CreatePatientRequest]) (*connect.Response[pb.CreatePatientResponse], error) {
	return nil, nil
}
`
	if err := os.WriteFile(filepath.Join(handlerDir, "handlers.go"), []byte(handlersGo), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
		},
	}

	crudMethods := []CRUDMethod{
		{
			Method:    MethodTemplateData{Name: "CreatePatient"},
			Entity:    EntityDef{Name: "Patient"},
			Operation: "create",
		},
	}

	err := GenerateCRUDHandlers(svc, crudMethods, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDHandlers() error = %v", err)
	}

	// Stale file should be removed since CreatePatient is already implemented
	if _, err := os.Stat(genPath); !os.IsNotExist(err) {
		t.Error("stale handlers_crud_gen.go should have been removed")
	}
}

func TestOperationToAuthAction(t *testing.T) {
	tests := []struct {
		op   string
		want string
	}{
		{"create", "create"},
		{"get", "read"},
		{"list", "list"},
		{"update", "update"},
		{"delete", "delete"},
		{"unknown", "read"},
	}

	for _, tt := range tests {
		got := operationToAuthAction(tt.op)
		if got != tt.want {
			t.Errorf("operationToAuthAction(%q) = %q, want %q", tt.op, got, tt.want)
		}
	}
}

func TestMatchCRUDMethods_CaseInsensitiveEntityMatch(t *testing.T) {
	entities := []EntityDef{
		{Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64"},
	}

	svc := ServiceDef{
		Name: "PatientsService",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
		},
	}

	matches := MatchCRUDMethods(svc, entities)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
}

func TestMatchCRUDMethods_AuthRequired(t *testing.T) {
	entities := []EntityDef{
		{Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64"},
	}

	svc := ServiceDef{
		Name: "PatientsService",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse", AuthRequired: true},
			{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse", AuthRequired: false},
		},
	}

	matches := MatchCRUDMethods(svc, entities)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if !matches[0].Method.AuthRequired {
		t.Error("CreatePatient should have AuthRequired=true")
	}
	if matches[1].Method.AuthRequired {
		t.Error("GetPatient should have AuthRequired=false")
	}
}

func TestGenerateCRUDTests_BasicGeneration(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
			{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
			{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			{Name: "UpdatePatient", InputType: "UpdatePatientRequest", OutputType: "UpdatePatientResponse"},
			{Name: "DeletePatient", InputType: "DeletePatientRequest", OutputType: "DeletePatientResponse"},
		},
	}

	entities := []EntityDef{
		{
			Name:      "Patient",
			TableName: "patients",
			PkField:   "id",
			PkGoType:  "int64",
			Fields: []EntityField{
				{Name: "id", GoName: "ID", GoType: "int64"},
				{Name: "name", GoName: "Name", GoType: "string"},
				{Name: "active", GoName: "Active", GoType: "bool"},
			},
		},
	}

	crudMethods := MatchCRUDMethods(svc, entities)

	err := GenerateCRUDTests(svc, crudMethods, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	unitPath := filepath.Join(handlerDir, "handlers_crud_gen_test.go")
	unitData, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("generated unit test file not found: %v", err)
	}
	unitContent := string(unitData)

	integrationPath := filepath.Join(handlerDir, "handlers_crud_integration_test.go")
	integrationData, err := os.ReadFile(integrationPath)
	if err != nil {
		t.Fatalf("generated integration test file not found: %v", err)
	}
	integrationContent := string(integrationData)

	// Integration file: should contain lifecycle test (all 5 CRUD ops matched)
	if !contains(integrationContent, "TestCRUDLifecycle_Patient") {
		t.Error("expected TestCRUDLifecycle_Patient in integration output")
	}

	// Integration file: should contain error case tests
	if !contains(integrationContent, "TestGet_Patient_NotFound") {
		t.Error("expected TestGet_Patient_NotFound in integration output")
	}
	if !contains(integrationContent, "TestDelete_Patient_NotFound") {
		t.Error("expected TestDelete_Patient_NotFound in integration output")
	}
	if !contains(integrationContent, "TestCreate_Patient_EmptyRequest") {
		t.Error("expected TestCreate_Patient_EmptyRequest in integration output")
	}
	if !contains(integrationContent, "TestUpdate_Patient_NotFound") {
		t.Error("expected TestUpdate_Patient_NotFound in integration output")
	}

	// Integration file: must START with the build tag so it's excluded from
	// default `go test ./...`.
	if !strings.HasPrefix(integrationContent, "//go:build integration\n") {
		t.Error("expected //go:build integration as first line of integration output")
	}

	// Unit file: should contain unit test frames
	if !contains(unitContent, "TestUnit_CreatePatient") {
		t.Error("expected TestUnit_CreatePatient in unit output")
	}
	if !contains(unitContent, "TestUnit_GetPatient") {
		t.Error("expected TestUnit_GetPatient in unit output")
	}

	// Unit file: must NOT carry the integration build tag as a directive
	// (a comment-mention in a doc block is fine).
	if strings.HasPrefix(unitContent, "//go:build") {
		t.Error("unit output must not start with a //go:build directive")
	}

	// Both files: should have test package suffix
	if !contains(unitContent, "package patients_test") {
		t.Error("expected package patients_test in unit output")
	}
	if !contains(integrationContent, "package patients_test") {
		t.Error("expected package patients_test in integration output")
	}

	// Both files: should have DO NOT EDIT marker
	if !contains(unitContent, "DO NOT EDIT") {
		t.Error("expected DO NOT EDIT marker in unit output")
	}
	if !contains(integrationContent, "DO NOT EDIT") {
		t.Error("expected DO NOT EDIT marker in integration output")
	}

	// Integration file: should include testify require import
	if !contains(integrationContent, "github.com/stretchr/testify/require") {
		t.Error("expected testify/require import in integration output")
	}

	// Integration file: should contain test field values
	if !contains(integrationContent, `"test-value"`) {
		t.Error("expected test-value string literal in integration output")
	}

	// Unit file: must delegate to tdd.RunRPCCases instead of inlining
	// per-RPC harness construction. This is the migration fingerprint
	// for v0.x-to-tdd-rpccases — locked here so future template churn
	// trips a test rather than silently regressing the shape.
	if !contains(unitContent, `"github.com/reliant-labs/forge/pkg/tdd"`) {
		t.Error("expected unit output to import forge/pkg/tdd")
	}
	if !contains(unitContent, "tdd.RunRPCCases(t, []tdd.RPCCase[") {
		t.Error("expected unit output to call tdd.RunRPCCases with []tdd.RPCCase[…] rows")
	}
	if !contains(unitContent, "svc.CreatePatient") {
		t.Error("expected unit output to pass svc.CreatePatient as the handler arg")
	}
	// Unit file: scaffold marker still present so writeScaffoldFile keeps
	// the file forge-owned until the user clears every marker.
	if !contains(unitContent, "FORGE_SCAFFOLD:") {
		t.Error("expected FORGE_SCAFFOLD marker in unit output")
	}

	// Unit file: must parse as valid Go. Catches template
	// regressions that produce syntactically broken output —
	// the template-level fingerprints above check shape, this
	// checks structure.
	if _, err := parser.ParseFile(token.NewFileSet(), unitPath, unitContent, parser.SkipObjectResolution); err != nil {
		t.Errorf("unit output is not valid Go: %v\n----\n%s", err, unitContent)
	}
}

func TestGenerateCRUDTests_PartialCRUD(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
			{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
		},
	}

	entities := []EntityDef{
		{
			Name:      "Patient",
			TableName: "patients",
			PkField:   "id",
			PkGoType:  "int64",
			Fields: []EntityField{
				{Name: "id", GoName: "ID", GoType: "int64"},
				{Name: "name", GoName: "Name", GoType: "string"},
			},
		},
	}

	crudMethods := MatchCRUDMethods(svc, entities)

	err := GenerateCRUDTests(svc, crudMethods, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	integrationPath := filepath.Join(handlerDir, "handlers_crud_integration_test.go")
	integrationData, err := os.ReadFile(integrationPath)
	if err != nil {
		t.Fatalf("generated integration test file not found: %v", err)
	}
	integrationContent := string(integrationData)

	// Should NOT contain lifecycle test (missing list, update, delete)
	if contains(integrationContent, "TestCRUDLifecycle_Patient") {
		t.Error("expected no lifecycle test when not all 5 CRUD ops exist")
	}

	// Should contain individual tests for existing ops (live in integration file)
	if !contains(integrationContent, "TestCreate_Patient_EmptyRequest") {
		t.Error("expected TestCreate_Patient_EmptyRequest")
	}
	if !contains(integrationContent, "TestGet_Patient_NotFound") {
		t.Error("expected TestGet_Patient_NotFound")
	}

	// Should NOT contain tests for missing ops
	if contains(integrationContent, "TestDelete_Patient_NotFound") {
		t.Error("should not have delete test when delete op is missing")
	}
}

func TestGenerateCRUDTests_CleanupWhenNoMethods(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create stale test gen files (both unit and integration variants)
	unitPath := filepath.Join(handlerDir, "handlers_crud_gen_test.go")
	if err := os.WriteFile(unitPath, []byte("package patients_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	integrationPath := filepath.Join(handlerDir, "handlers_crud_integration_test.go")
	if err := os.WriteFile(integrationPath, []byte("//go:build integration\npackage patients_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{Name: "PatientsService"}

	err := GenerateCRUDTests(svc, nil, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	// Stale files should be removed
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Error("stale handlers_crud_gen_test.go should have been removed")
	}
	if _, err := os.Stat(integrationPath); !os.IsNotExist(err) {
		t.Error("stale handlers_crud_integration_test.go should have been removed")
	}
}

func TestTestValueForType(t *testing.T) {
	tests := []struct {
		goType string
		want   string
	}{
		{"string", `"test-value"`},
		{"int32", "1"},
		{"int64", "1"},
		{"uint32", "1"},
		{"uint64", "1"},
		{"float32", "1.0"},
		{"float64", "1.0"},
		{"bool", "true"},
		{"[]byte", `[]byte("test")`},
		{"*timestamppb.Timestamp", "timestamppb.Now()"},
		// Wrapper types
		{"*string", `wrapperspb.String("test-value")`},
		{"*int32", "wrapperspb.Int32(42)"},
		{"*int64", "wrapperspb.Int64(42)"},
		{"*bool", "wrapperspb.Bool(true)"},
		{"*float64", "wrapperspb.Double(1.0)"},
		// Message/repeated/map types → nil
		{"*SomeMessage", "nil"},
		{"[]string", "nil"},
		{"[]*SomeMessage", "nil"},
		{"map[string]string", "nil"},
		// Enum-like types (bare identifier) → 0
		{"SomeCustomType", "0"},
	}

	for _, tt := range tests {
		got := testValueForType(tt.goType)
		if got != tt.want {
			t.Errorf("testValueForType(%q) = %q, want %q", tt.goType, got, tt.want)
		}
	}
}

func TestBuildCRUDTestTemplateData(t *testing.T) {
	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
	}

	crudMethods := []CRUDMethod{
		{
			Method:    MethodTemplateData{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
			Entity:    EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64", Fields: []EntityField{{Name: "id", GoName: "ID", GoType: "int64"}, {Name: "name", GoName: "Name", GoType: "string"}}},
			Operation: "create",
		},
		{
			Method:    MethodTemplateData{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
			Entity:    EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64"},
			Operation: "get",
		},
		{
			Method:    MethodTemplateData{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			Entity:    EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64"},
			Operation: "list",
		},
		{
			Method:    MethodTemplateData{Name: "UpdatePatient", InputType: "UpdatePatientRequest", OutputType: "UpdatePatientResponse"},
			Entity:    EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64"},
			Operation: "update",
		},
		{
			Method:    MethodTemplateData{Name: "DeletePatient", InputType: "DeletePatientRequest", OutputType: "DeletePatientResponse"},
			Entity:    EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64"},
			Operation: "delete",
		},
	}

	data := buildCRUDTestTemplateData(svc, crudMethods, "example.com/test", "")

	if data.Package != "patients" {
		t.Errorf("Package = %q, want patients", data.Package)
	}
	if data.ProtoPackage != "proto/services/patients" {
		t.Errorf("ProtoPackage = %q, want proto/services/patients", data.ProtoPackage)
	}
	if len(data.Entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(data.Entities))
	}

	ent := data.Entities[0]
	if !ent.HasAllCRUD {
		t.Error("expected HasAllCRUD=true")
	}
	if !ent.HasCreate || !ent.HasGet || !ent.HasList || !ent.HasUpdate || !ent.HasDelete {
		t.Error("expected all individual CRUD flags to be true")
	}
	if ent.CreateMethod.MethodName != "CreatePatient" {
		t.Errorf("CreateMethod.MethodName = %q, want CreatePatient", ent.CreateMethod.MethodName)
	}
	if len(ent.Fields) != 1 {
		t.Errorf("expected 1 field (id excluded), got %d", len(ent.Fields))
	}
	if len(data.CRUDMethods) != 5 {
		t.Errorf("expected 5 CRUDMethods, got %d", len(data.CRUDMethods))
	}
}

func TestClassifyFilterField(t *testing.T) {
	tests := []struct {
		name       string
		field      MessageFieldDef
		wantType   string
		wantGoName string
	}{
		{"search field", MessageFieldDef{Name: "search", ProtoType: "string"}, "search", "Search"},
		{"query field", MessageFieldDef{Name: "query", ProtoType: "string"}, "search", "Query"},
		{"q field", MessageFieldDef{Name: "q", ProtoType: "string"}, "search", "Q"},
		{"bool field", MessageFieldDef{Name: "active", ProtoType: "bool"}, "exact", "Active"},
		{"string field", MessageFieldDef{Name: "status", ProtoType: "string"}, "exact", "Status"},
		{"int32 field", MessageFieldDef{Name: "age", ProtoType: "int32"}, "exact", "Age"},
		{"optional field", MessageFieldDef{Name: "active", ProtoType: "bool", IsOptional: true}, "exact", "Active"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ff := classifyFilterField(tt.field)
			if ff.FilterType != tt.wantType {
				t.Errorf("FilterType = %q, want %q", ff.FilterType, tt.wantType)
			}
			if ff.GoName != tt.wantGoName {
				t.Errorf("GoName = %q, want %q", ff.GoName, tt.wantGoName)
			}
			if ff.IsOptional != tt.field.IsOptional {
				t.Errorf("IsOptional = %v, want %v", ff.IsOptional, tt.field.IsOptional)
			}
		})
	}
}

func TestClassifySkipField(t *testing.T) {
	skipFields := []string{"page_size", "page_token", "descending", "desc", "sort_order"}
	for _, f := range skipFields {
		if !classifySkipField(f) {
			t.Errorf("expected %q to be skipped", f)
		}
	}

	noSkipFields := []string{"search", "active", "status", "order_by"}
	for _, f := range noSkipFields {
		if classifySkipField(f) {
			t.Errorf("expected %q to NOT be skipped", f)
		}
	}
}

func TestBuildCRUDTemplateData_WithFilters(t *testing.T) {
	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Messages: map[string][]MessageFieldDef{
			"ListPatientsRequest": {
				{Name: "page_size", ProtoType: "int32"},
				{Name: "page_token", ProtoType: "string"},
				{Name: "active", ProtoType: "bool", IsOptional: true},
				{Name: "search", ProtoType: "string", IsOptional: true},
				{Name: "status", ProtoType: "string"},
				{Name: "order_by", ProtoType: "string"},
				{Name: "descending", ProtoType: "bool"},
			},
		},
	}

	crudMethods := []CRUDMethod{
		{
			Method:    MethodTemplateData{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			Entity:    EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64", Fields: []EntityField{{Name: "id", GoName: "ID", GoType: "int64"}}},
			Operation: "list",
		},
	}

	data := buildCRUDTemplateData(svc, crudMethods, "example.com/test")

	if !data.NeedsORM {
		t.Error("expected NeedsORM=true")
	}
	if !data.HasFilters {
		t.Error("expected HasFilters=true")
	}
	if !data.HasOrderBy {
		t.Error("expected HasOrderBy=true")
	}
	if len(data.CRUDMethods) != 1 {
		t.Fatalf("expected 1 method, got %d", len(data.CRUDMethods))
	}

	m := data.CRUDMethods[0]
	if !m.HasFilters {
		t.Error("expected method HasFilters=true")
	}
	if !m.HasOrderBy {
		t.Error("expected method HasOrderBy=true")
	}
	if len(m.FilterFields) != 3 {
		t.Fatalf("expected 3 filter fields, got %d", len(m.FilterFields))
	}

	// Check filter field classification
	expected := []struct {
		goName     string
		filterType string
		isOptional bool
	}{
		{"Active", "exact", true},
		{"Search", "search", true},
		{"Status", "exact", false},
	}
	for i, exp := range expected {
		ff := m.FilterFields[i]
		if ff.GoName != exp.goName {
			t.Errorf("filter[%d].GoName = %q, want %q", i, ff.GoName, exp.goName)
		}
		if ff.FilterType != exp.filterType {
			t.Errorf("filter[%d].FilterType = %q, want %q", i, ff.FilterType, exp.filterType)
		}
		if ff.IsOptional != exp.isOptional {
			t.Errorf("filter[%d].IsOptional = %v, want %v", i, ff.IsOptional, exp.isOptional)
		}
	}
}

func TestGenerateCRUDHandlers_WithFilters(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a service.go with Deps struct containing DB field
	serviceGo := `package patients

import "github.com/reliant-labs/forge/pkg/orm"

type Deps struct {
	DB orm.Context
}

type Service struct {
	deps Deps
}
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(serviceGo), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
		},
		Messages: map[string][]MessageFieldDef{
			"ListPatientsRequest": {
				{Name: "page_size", ProtoType: "int32"},
				{Name: "page_token", ProtoType: "string"},
				{Name: "active", ProtoType: "bool", IsOptional: true},
				{Name: "search", ProtoType: "string", IsOptional: true},
				{Name: "order_by", ProtoType: "string"},
				{Name: "descending", ProtoType: "bool"},
			},
		},
	}

	entities := []EntityDef{{
		Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64",
		Fields: []EntityField{
			{Name: "id", GoName: "ID", GoType: "int64"},
			{Name: "name", GoName: "Name", GoType: "string"},
		},
	}}

	crudMethods := MatchCRUDMethods(svc, entities)

	err := GenerateCRUDHandlers(svc, crudMethods, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDHandlers() error = %v", err)
	}

	genPath := filepath.Join(handlerDir, "handlers_crud_gen.go")
	data, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatalf("generated file not found: %v", err)
	}

	content := string(data)

	// Should contain orm import
	if !contains(content, "pkg/orm") {
		t.Error("expected orm import in generated output")
	}

	// Should contain filter logic
	if !contains(content, "WhereILike") {
		t.Error("expected WhereILike for search filter")
	}
	if !contains(content, "WhereEq") {
		t.Error("expected WhereEq for exact filter")
	}

	// Should hand off ordering to the crud library (lifecycle moved
	// into pkg/crud; the shim only wires request accessors).
	if !contains(content, "HasOrderBy:    true") {
		t.Error("expected HasOrderBy: true in generated ListOp literal")
	}
	if !contains(content, "req.OrderBy") {
		t.Error("expected req.OrderBy accessor closure in generated output")
	}
	if !contains(content, "req.Descending") {
		t.Error("expected req.Descending accessor closure in generated output")
	}

	// Should contain filter nil check for optional fields (now inside
	// the per-RPC Filters closure).
	if !contains(content, "req.Active != nil") {
		t.Error("expected nil check for optional Active filter")
	}
	if !contains(content, "req.Search != nil") {
		t.Error("expected nil check for optional Search filter")
	}
}

func TestBuildCRUDTemplateData_NoFilters(t *testing.T) {
	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		// No Messages — simulates no proto message field parsing
	}

	crudMethods := []CRUDMethod{
		{
			Method:    MethodTemplateData{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			Entity:    EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64", Fields: []EntityField{{Name: "id", GoName: "ID", GoType: "int64"}}},
			Operation: "list",
		},
	}

	data := buildCRUDTemplateData(svc, crudMethods, "example.com/test")

	m := data.CRUDMethods[0]
	if m.HasFilters {
		t.Error("expected HasFilters=false when no Messages")
	}
	if m.HasOrderBy {
		t.Error("expected HasOrderBy=false when no Messages")
	}
	if !m.HasPagination {
		t.Error("expected HasPagination=true (AIP-158 naming)")
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 &&
		len(s) >= len(substr) &&
		// Simple containment check
		findSubstring(s, substr)
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}