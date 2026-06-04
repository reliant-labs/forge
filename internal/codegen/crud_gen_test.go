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

// TestEnsureDepsDBField_DoesNotMutateWhenHandlersGoExists pins the
// Tier-3 user-owned contract for handlers/<svc>/service.go.
//
// Before this regression test landed, ensureDepsDBField silently
// injected `DB orm.Context` and a `pkg/orm` import into service.go on
// the FIRST `forge generate` after a proto service grew a `List*` /
// `Get*` / `Create*` / etc. method — even when the user had written a
// hand-rolled handlers.go and never intended to consume forge's CRUD
// codegen. service.go is Tier-3 user-owned (banners.go classifies it
// so) and "user-owned, never mutated" is the documented convention;
// mutating it on regen was a silent stomp.
//
// The opt-out: presence of handlers.go in the service package signals
// "I'm managing handler wiring myself; keep your hands off service.go".
// The CRUD dedup pass still emits handlers_crud_gen.go for any CRUD
// method the user has NOT implemented in handlers.go — but if those
// stubs reference s.deps.DB and the user hasn't added DB, the resulting
// `go build` error is loud and visible, not a silent file mutation.
func TestEnsureDepsDBField_DoesNotMutateWhenHandlersGoExists(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// User-owned service.go: NO DB field, NO orm import. The user wants
	// to hand-write handlers without forge's CRUD codegen wiring.
	serviceGo := `package patients

import (
	"fmt"
	"log/slog"
)

type Deps struct {
	Logger *slog.Logger
}

func (d Deps) validateDeps() error {
	if d.Logger == nil {
		return fmt.Errorf("PatientsService: Deps.Logger is required")
	}
	return nil
}

type Service struct {
	deps Deps
}
`
	servicePath := filepath.Join(handlerDir, "service.go")
	if err := os.WriteFile(servicePath, []byte(serviceGo), 0o644); err != nil {
		t.Fatal(err)
	}

	// User-owned handlers.go: signals opt-out from forge CRUD codegen.
	// The user has implemented ListPatients themselves with whatever
	// shape they like — no DB dependency.
	handlersGo := `package patients

import (
	"context"
	"connectrpc.com/connect"
	pb "example.com/test/gen/proto/services/patients/v1"
)

func (s *Service) ListPatients(ctx context.Context, req *connect.Request[pb.ListPatientsRequest]) (*connect.Response[pb.ListPatientsResponse], error) {
	return nil, nil
}
`
	if err := os.WriteFile(filepath.Join(handlerDir, "handlers.go"), []byte(handlersGo), 0o644); err != nil {
		t.Fatal(err)
	}

	// Snapshot the exact bytes of service.go before generate.
	beforeBytes, err := os.ReadFile(servicePath)
	if err != nil {
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
	if len(crudMethods) != 1 {
		t.Fatalf("expected 1 CRUD match (ListPatients → Patient), got %d", len(crudMethods))
	}

	if err := GenerateCRUDHandlers(svc, crudMethods, "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDHandlers() error = %v", err)
	}

	// service.go must be byte-for-byte identical — NO injected DB field,
	// NO injected orm import. This is the Tier-3-never-mutated guarantee.
	afterBytes, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Errorf("service.go was mutated despite handlers.go existing.\n--- before ---\n%s\n--- after ---\n%s",
			string(beforeBytes), string(afterBytes))
	}

	// Sanity checks: defensive against the regression's exact symptoms.
	after := string(afterBytes)
	if strings.Contains(after, "orm.Context") {
		t.Error("service.go must not contain orm.Context after generate when handlers.go exists")
	}
	if strings.Contains(after, "github.com/reliant-labs/forge/pkg/orm") {
		t.Error("service.go must not contain pkg/orm import after generate when handlers.go exists")
	}
}

// TestEnsureDepsDBField_InjectsWhenHandlersGoAbsent pins the happy
// path: a fresh service with no handlers.go (user hasn't started
// writing handler code yet) DOES get the DB field auto-injected so the
// generated handlers_crud_gen.go compiles out of the box. This is the
// behavior that motivated ensureDepsDBField in the first place and we
// don't want to regress it while fixing the silent-mutation case above.
func TestEnsureDepsDBField_InjectsWhenHandlersGoAbsent(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Fresh service.go from the scaffold: no DB field, no orm import,
	// has the "// Add your dependencies here." marker.
	serviceGo := `package patients

import (
	"fmt"
	"log/slog"
)

type Deps struct {
	Logger *slog.Logger
	// Add your dependencies here.
}

func (d Deps) validateDeps() error {
	if d.Logger == nil {
		return fmt.Errorf("PatientsService: Deps.Logger is required")
	}
	return nil
}

type Service struct {
	deps Deps
}
`
	servicePath := filepath.Join(handlerDir, "service.go")
	if err := os.WriteFile(servicePath, []byte(serviceGo), 0o644); err != nil {
		t.Fatal(err)
	}
	// Intentionally NO handlers.go — fresh service, user hasn't started.

	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
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

	if err := GenerateCRUDHandlers(svc, crudMethods, "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDHandlers() error = %v", err)
	}

	after, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(after)

	if !strings.Contains(content, "DB") || !strings.Contains(content, "orm.Context") {
		t.Errorf("expected DB orm.Context to be injected into Deps when no handlers.go exists; got:\n%s", content)
	}
	if !strings.Contains(content, "github.com/reliant-labs/forge/pkg/orm") {
		t.Errorf("expected pkg/orm import to be injected when no handlers.go exists; got:\n%s", content)
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

// TestGenerateCRUDTests_SkipsExistingMethods mirrors
// TestGenerateCRUDHandlers_SkipsExistingMethods: once the user has taken
// ownership of a CRUD method by writing a real handler, the test scaffold
// MUST stop re-emitting an AIP-158-shaped harness for that method. Before
// this regression test landed, GenerateCRUDTests kept clobbering the
// scaffold with `req.PageSize: 10` / `req.Id: 1` rows that no longer
// type-checked against the user's hand-written request shape, and the
// test package went red on the next `go test ./...`.
func TestGenerateCRUDTests_SkipsExistingMethods(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a service.go (required for buildCRUDTestTemplateData to
	// resolve the test-helper name) and a handlers.go that already
	// implements CreatePatient. The remaining methods should drive the
	// test scaffold.
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

	if err := GenerateCRUDTests(svc, crudMethods, "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	unitPath := filepath.Join(handlerDir, "handlers_crud_gen_test.go")
	unitData, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("expected handlers_crud_gen_test.go to be written: %v", err)
	}
	unit := string(unitData)

	// Must NOT carry a TestUnit_CreatePatient row — user owns CreatePatient.
	if contains(unit, "TestUnit_CreatePatient") || contains(unit, "svc.CreatePatient") {
		t.Errorf("unit test scaffold should not reference CreatePatient (user-owned); got:\n%s", unit)
	}
	// Should still carry GetPatient (user has not implemented it).
	if !contains(unit, "TestUnit_GetPatient") {
		t.Errorf("unit test scaffold should still cover GetPatient; got:\n%s", unit)
	}
}

// TestGenerateCRUDTests_RemovesScaffoldWhenAllUserOwned guarantees the
// stale-cleanup path runs after every CRUD method has been hand-implemented.
// Combined with the filter above, a user who hand-writes every CRUD method
// ends up with no `handlers_crud_gen_test.go` on disk — exactly the same
// shape `GenerateCRUDHandlers` produces for `handlers_crud_gen.go`.
func TestGenerateCRUDTests_RemovesScaffoldWhenAllUserOwned(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed a stale scaffold so we can verify the cleanup branch fires.
	unitPath := filepath.Join(handlerDir, "handlers_crud_gen_test.go")
	if err := os.WriteFile(unitPath, []byte("// FORGE_SCAFFOLD: stale\npackage patients_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	integrationPath := filepath.Join(handlerDir, "handlers_crud_integration_test.go")
	if err := os.WriteFile(integrationPath, []byte("//go:build integration\n// FORGE_SCAFFOLD: stale\npackage patients_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	serviceGo := `package patients

type Deps struct{}
type Service struct{ deps Deps }
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(serviceGo), 0o644); err != nil {
		t.Fatal(err)
	}

	handlersGo := `package patients

import (
	"context"
	"connectrpc.com/connect"
	pb "example.com/test/gen/proto/services/patients/v1"
)

func (s *Service) CreatePatient(ctx context.Context, req *connect.Request[pb.CreatePatientRequest]) (*connect.Response[pb.CreatePatientResponse], error) {
	return nil, nil
}
func (s *Service) GetPatient(ctx context.Context, req *connect.Request[pb.GetPatientRequest]) (*connect.Response[pb.GetPatientResponse], error) {
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
	entities := []EntityDef{{
		Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64",
		Fields: []EntityField{{Name: "id", GoName: "ID", GoType: "int64"}},
	}}

	if err := GenerateCRUDTests(svc, MatchCRUDMethods(svc, entities), "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Error("expected handlers_crud_gen_test.go to be removed when all CRUD methods are user-owned")
	}
	if _, err := os.Stat(integrationPath); !os.IsNotExist(err) {
		t.Error("expected handlers_crud_integration_test.go to be removed when all CRUD methods are user-owned")
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


// TestValidateCRUDShape_BespokeShape covers the gating that landed for
// crud-body-generator-shape-mismatch: when proto messages don't match the
// AIP-158 conventions the CRUD body template assumes, validateCRUDShape
// reports the mismatch with a human-readable reason so buildCRUDTemplateData
// can emit a CodeUnimplemented stub instead of non-compiling body code.
//
// The kalshi-trader port hit each of these shapes:
//   - ListMarketsRequest carries `Limit` (int32) and `Cursor` (string)
//     instead of AIP-158 `page_size` / `page_token`
//   - GetMarketRequest's key is `ticker` (string), not the entity's
//     default `id` PK
//   - CreateMarketResponse has `repeated Market markets = 1` (a fan-out)
//     instead of the AIP-shaped `Market market = 1`
//
// Each row asserts the rule that catches the specific divergence; the
// final "matches" row pins the happy path so a future refactor that
// accidentally tightens the rules trips a test rather than starving
// real CRUD generation.
func TestValidateCRUDShape_BespokeShape(t *testing.T) {
	type tc struct {
		name      string
		svc       ServiceDef
		cm        CRUDMethod
		wantOK    bool
		reasonHas string // substring expected in reason when !wantOK
	}

	patient := EntityDef{Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64"}
	market := EntityDef{Name: "Market", TableName: "markets", PkField: "id", PkGoType: "int64"}

	cases := []tc{
		{
			name: "list_missing_page_size_in_request",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"ListMarketsRequest": {
						{Name: "limit", ProtoType: "int32"},
						{Name: "cursor", ProtoType: "string"},
					},
					"ListMarketsResponse": {
						{Name: "markets", ProtoType: "message"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "ListMarkets", InputType: "ListMarketsRequest", OutputType: "ListMarketsResponse"},
				Entity:    market,
				Operation: "list",
			},
			wantOK:    false,
			reasonHas: "page_size",
		},
		{
			name: "list_missing_response_plural_field",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"ListPatientsRequest": {
						{Name: "page_size", ProtoType: "int32"},
						{Name: "page_token", ProtoType: "string"},
					},
					"ListPatientsResponse": {
						{Name: "items", ProtoType: "message"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
				Entity:    patient,
				Operation: "list",
			},
			wantOK:    false,
			reasonHas: "patients",
		},
		{
			name: "get_request_missing_pk",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"GetMarketRequest": {
						{Name: "ticker", ProtoType: "string"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "GetMarket", InputType: "GetMarketRequest", OutputType: "GetMarketResponse"},
				Entity:    market,
				Operation: "get",
			},
			wantOK:    false,
			reasonHas: "id",
		},
		{
			name: "create_response_missing_entity_field",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"CreateMarketRequest": {
						{Name: "ticker", ProtoType: "string"},
					},
					"CreateMarketResponse": {
						{Name: "markets", ProtoType: "message"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "CreateMarket", InputType: "CreateMarketRequest", OutputType: "CreateMarketResponse"},
				Entity:    market,
				Operation: "create",
			},
			wantOK:    false,
			reasonHas: "market",
		},
		{
			name: "update_request_missing_entity_message",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"UpdateMarketRequest": {
						{Name: "ticker", ProtoType: "string"},
						{Name: "title", ProtoType: "string"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "UpdateMarket", InputType: "UpdateMarketRequest", OutputType: "UpdateMarketResponse"},
				Entity:    market,
				Operation: "update",
			},
			wantOK:    false,
			reasonHas: "Market message",
		},
		{
			name: "delete_request_missing_pk",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"DeleteMarketRequest": {
						{Name: "ticker", ProtoType: "string"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "DeleteMarket", InputType: "DeleteMarketRequest", OutputType: "DeleteMarketResponse"},
				Entity:    market,
				Operation: "delete",
			},
			wantOK:    false,
			reasonHas: "id",
		},
		{
			name: "no_messages_map_returns_legacy_ok",
			svc:  ServiceDef{}, // Messages is nil
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
				Entity:    patient,
				Operation: "list",
			},
			wantOK: true,
		},
		{
			name: "matches_aip158_shape",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"ListPatientsRequest": {
						{Name: "page_size", ProtoType: "int32"},
						{Name: "page_token", ProtoType: "string"},
					},
					"ListPatientsResponse": {
						{Name: "patients", ProtoType: "message"},
						{Name: "next_page_token", ProtoType: "string"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
				Entity:    patient,
				Operation: "list",
			},
			wantOK: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := validateCRUDShape(c.svc, c.cm)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (reason=%q)", ok, c.wantOK, reason)
			}
			if !c.wantOK && c.reasonHas != "" && !strings.Contains(reason, c.reasonHas) {
				t.Errorf("reason = %q, want substring %q", reason, c.reasonHas)
			}
		})
	}
}

// TestGenerateCRUDHandlers_StubsOnShapeMismatch is the full integration
// regression for crud-body-generator-shape-mismatch: when the input/output
// messages don't fit the CRUD-body template's AIP-158 assumptions, the
// emitted handlers_crud_gen.go must still compile (and satisfy the proto
// service interface) by returning a tagged CodeUnimplemented stub rather
// than dereferencing fields the request type doesn't have.
//
// Each row pins one of the kalshi-trader shapes:
//   - ListMarkets: `limit/cursor` instead of `page_size/page_token`
//   - GetMarket:   `ticker` instead of `id`
//   - CreateMarket: response has `repeated Market markets` not `Market market`
//
// Together they prove buildCRUDTemplateData + the template branch fall
// back to a stub for every operation where validateCRUDShape returns false.
func TestGenerateCRUDHandlers_StubsOnShapeMismatch(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "markets")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	serviceGo := `package markets

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
		Name:       "MarketsService",
		GoPackage:  "example.com/test/gen/proto/services/markets/v1",
		PkgName:    "marketsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "ListMarkets", InputType: "ListMarketsRequest", OutputType: "ListMarketsResponse"},
			{Name: "GetMarket", InputType: "GetMarketRequest", OutputType: "GetMarketResponse"},
			{Name: "CreateMarket", InputType: "CreateMarketRequest", OutputType: "CreateMarketResponse"},
		},
		Messages: map[string][]MessageFieldDef{
			// bespoke kalshi-style pagination
			"ListMarketsRequest": {
				{Name: "limit", ProtoType: "int32"},
				{Name: "cursor", ProtoType: "string"},
				{Name: "kalshi_status", ProtoType: "enum"},
			},
			"ListMarketsResponse": {
				{Name: "markets", ProtoType: "message"},
			},
			// string Ticker key, not int64 Id
			"GetMarketRequest": {
				{Name: "ticker", ProtoType: "string"},
			},
			"GetMarketResponse": {
				{Name: "market", ProtoType: "message"},
			},
			// response holds repeated entity (fan-out create), not single
			"CreateMarketRequest": {
				{Name: "ticker", ProtoType: "string"},
			},
			"CreateMarketResponse": {
				{Name: "markets", ProtoType: "message"},
			},
		},
	}

	entities := []EntityDef{{
		Name: "Market", TableName: "markets", PkField: "id", PkGoType: "int64",
		Fields: []EntityField{
			{Name: "id", GoName: "ID", GoType: "int64"},
			{Name: "ticker", GoName: "Ticker", GoType: "string"},
		},
	}}

	crudMethods := MatchCRUDMethods(svc, entities)
	if err := GenerateCRUDHandlers(svc, crudMethods, "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDHandlers() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(handlerDir, "handlers_crud_gen.go"))
	if err != nil {
		t.Fatalf("read gen file: %v", err)
	}
	content := string(data)

	// 1. Every method should be emitted (so the proto interface is satisfied).
	for _, want := range []string{"func (s *Service) ListMarkets(", "func (s *Service) GetMarket(", "func (s *Service) CreateMarket("} {
		if !strings.Contains(content, want) {
			t.Errorf("missing handler declaration %q in:\n%s", want, content)
		}
	}

	// 2. Each method must carry the FORGE_CRUD_SHAPE_MISMATCH marker (so
	//    the user can grep for it). The package header comment also
	//    references the marker once, so the total count is per-stub-count + 1.
	if mismatches := strings.Count(content, "FORGE_CRUD_SHAPE_MISMATCH"); mismatches < 3 {
		t.Errorf("expected >=3 FORGE_CRUD_SHAPE_MISMATCH markers, got %d in:\n%s", mismatches, content)
	}

	// 3. None of the AIP-158 dereferences that would fail to compile
	//    against this proto should appear in the output.
	for _, bad := range []string{"req.PageSize", "req.PageToken", "crud.HandleList(", "crud.HandleGet(", "crud.HandleCreate("} {
		if strings.Contains(content, bad) {
			t.Errorf("unexpected %q in mismatch-only output:\n%s", bad, content)
		}
	}

	// 4. Each stub must return CodeUnimplemented so callers get a clear
	//    error instead of a silent nil response.
	if c := strings.Count(content, "connect.CodeUnimplemented"); c != 3 {
		t.Errorf("expected 3 CodeUnimplemented returns, got %d", c)
	}

	// 5. The generated file must parse as valid Go — the whole point of
	//    the stub fallback is that the package keeps compiling against
	//    the real proto shape.
	if _, err := parser.ParseFile(token.NewFileSet(), "handlers_crud_gen.go", content, parser.SkipObjectResolution); err != nil {
		t.Errorf("generated file is not valid Go: %v\n----\n%s", err, content)
	}

	// 6. Because every method is a stub, the import block must not pull
	//    in pkg/crud or internal/db (they would be unused and trip the
	//    compiler).
	for _, bad := range []string{`"github.com/reliant-labs/forge/pkg/crud"`, `"example.com/test/internal/db"`, `"example.com/test/pkg/middleware"`} {
		if strings.Contains(content, bad) {
			t.Errorf("expected no import of %s when every method is a stub; got:\n%s", bad, content)
		}
	}
}

// TestGenerateCRUDTests_StubsOnShapeMismatch pins the test-scaffold
// counterpart of TestGenerateCRUDHandlers_StubsOnShapeMismatch: when
// validateCRUDShape returns false for an RPC, the per-RPC test row in
// handlers_crud_gen_test.go must NOT emit an AIP-158 happy-path literal
// that dereferences fields the request type doesn't have (e.g.
// `Id: 1` on a GetMarketRequest whose key is `string ticker`). Instead
// the row is replaced with a CodeUnimplemented scaffold matching the
// CRUD body generator's stub, so the test file stays compileable
// against the real proto shape.
//
// Surfaced-by: kalshi-trader migration round (3 friction reports —
// add-model-performance-entity, add-get-model-performance-rpc — all
// hit the same template bug from different proto angles).
func TestGenerateCRUDTests_StubsOnShapeMismatch(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "markets")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// service.go must be present for buildCRUDTestTemplateData to
	// resolve its TestHelperName lookup.
	serviceGo := `package markets

type Deps struct{}
type Service struct{ deps Deps }
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(serviceGo), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:       "MarketsService",
		GoPackage:  "example.com/test/gen/proto/services/markets/v1",
		PkgName:    "marketsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "ListMarkets", InputType: "ListMarketsRequest", OutputType: "ListMarketsResponse"},
			{Name: "GetMarket", InputType: "GetMarketRequest", OutputType: "GetMarketResponse"},
			{Name: "CreateMarket", InputType: "CreateMarketRequest", OutputType: "CreateMarketResponse"},
		},
		Messages: map[string][]MessageFieldDef{
			"ListMarketsRequest": {
				{Name: "limit", ProtoType: "int32"},
				{Name: "cursor", ProtoType: "string"},
			},
			"ListMarketsResponse": {
				{Name: "markets", ProtoType: "message"},
			},
			"GetMarketRequest": {
				{Name: "ticker", ProtoType: "string"},
			},
			"GetMarketResponse": {
				{Name: "market", ProtoType: "message"},
			},
			"CreateMarketRequest": {
				{Name: "ticker", ProtoType: "string"},
			},
			"CreateMarketResponse": {
				{Name: "markets", ProtoType: "message"},
			},
		},
	}

	entities := []EntityDef{{
		Name: "Market", TableName: "markets", PkField: "id", PkGoType: "int64",
		Fields: []EntityField{
			{Name: "id", GoName: "ID", GoType: "int64"},
			{Name: "ticker", GoName: "Ticker", GoType: "string"},
		},
	}}

	crudMethods := MatchCRUDMethods(svc, entities)
	if err := GenerateCRUDTests(svc, crudMethods, "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	unit, err := os.ReadFile(filepath.Join(handlerDir, "handlers_crud_gen_test.go"))
	if err != nil {
		t.Fatalf("read unit test file: %v", err)
	}
	content := string(unit)

	// 1. The non-compiling AIP-158 happy-path literals must NOT appear.
	//    These are the exact strings that broke kalshi-trader's build
	//    on the freshly-regenerated test scaffold.
	for _, bad := range []string{
		"&pb.GetMarketRequest{\n\t\t\t\tId:",
		"&pb.ListMarketsRequest{PageSize:",
		"PageSize: 10",
	} {
		if strings.Contains(content, bad) {
			t.Errorf("unexpected AIP-158 literal %q in shape-mismatch test scaffold:\n%s", bad, content)
		}
	}

	// 2. Each mismatched method must carry the FORGE_CRUD_SHAPE_MISMATCH
	//    marker so the user can grep for it from the test file too.
	if c := strings.Count(content, "FORGE_CRUD_SHAPE_MISMATCH"); c < 3 {
		t.Errorf("expected >=3 FORGE_CRUD_SHAPE_MISMATCH markers in test scaffold, got %d in:\n%s", c, content)
	}

	// 3. Each stub row must assert WantErr: connect.CodeUnimplemented so
	//    the test exercises (and pins) the matching handler stub.
	if c := strings.Count(content, "connect.CodeUnimplemented"); c < 3 {
		t.Errorf("expected >=3 connect.CodeUnimplemented assertions in test scaffold, got %d", c)
	}

	// 4. Each method's TestUnit_ function must still be present so the
	//    proto interface stays covered by at least one row.
	for _, want := range []string{"TestUnit_ListMarkets", "TestUnit_GetMarket", "TestUnit_CreateMarket"} {
		if !strings.Contains(content, want) {
			t.Errorf("expected %s in shape-mismatch test scaffold:\n%s", want, content)
		}
	}

	// 5. The generated file must parse as valid Go — the whole point of
	//    the stub fallback is that the test file keeps compiling
	//    against the real proto shape.
	if _, err := parser.ParseFile(token.NewFileSet(), "handlers_crud_gen_test.go", content, parser.SkipObjectResolution); err != nil {
		t.Errorf("generated test file is not valid Go: %v\n----\n%s", err, content)
	}
}
