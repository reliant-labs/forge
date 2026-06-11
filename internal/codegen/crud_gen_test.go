package codegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
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

	// Tier-1 projection: per-RPC op constructors land in the ops file.
	opsPath := filepath.Join(handlerDir, "handlers_crud_ops_gen.go")
	opsData, err := os.ReadFile(opsPath)
	if err != nil {
		t.Fatalf("generated ops file not found: %v", err)
	}
	ops := string(opsData)

	// Should NOT carry an op for CreatePatient (already in user handlers.go)
	if contains(ops, "crudCreatePatientOp") {
		t.Error("ops file should not contain crudCreatePatientOp (already implemented)")
	}
	// Should carry the GetPatient op (not in user handlers)
	if !contains(ops, "func (s *Service) crudGetPatientOp()") {
		t.Error("ops file should contain crudGetPatientOp constructor")
	}

	// User-owned shim: only the un-implemented RPC gets a delegating method.
	shimPath := filepath.Join(handlerDir, "handlers_crud.go")
	shimData, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("scaffolded handlers_crud.go not found: %v", err)
	}
	shim := string(shimData)

	if contains(shim, "func (s *Service) CreatePatient(") {
		t.Error("handlers_crud.go should not contain CreatePatient (already implemented)")
	}
	if !contains(shim, "func (s *Service) GetPatient(") {
		t.Error("handlers_crud.go should contain the GetPatient shim")
	}
	if !contains(shim, "crud.HandleGet(s.crudGetPatientOp())(ctx, req)") {
		t.Error("GetPatient shim should delegate to crud.HandleGet(s.crudGetPatientOp())")
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

	// Create stale legacy/current gen files self-identifying as forge
	// output (untracked files without the banner are never swept).
	genPath := filepath.Join(handlerDir, "handlers_crud_gen.go")
	if err := os.WriteFile(genPath, []byte("// Code generated by forge. DO NOT EDIT.\npackage patients\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opsPath := filepath.Join(handlerDir, "handlers_crud_ops_gen.go")
	if err := os.WriteFile(opsPath, []byte("// Code generated by forge. DO NOT EDIT.\npackage patients\n"), 0o644); err != nil {
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

	// Stale files should be removed since CreatePatient is already implemented
	if _, err := os.Stat(genPath); !os.IsNotExist(err) {
		t.Error("stale legacy handlers_crud_gen.go should have been removed")
	}
	if _, err := os.Stat(opsPath); !os.IsNotExist(err) {
		t.Error("stale handlers_crud_ops_gen.go should have been removed")
	}
	// And nothing new is scaffolded: the user owns every CRUD method.
	if _, err := os.Stat(filepath.Join(handlerDir, "handlers_crud.go")); !os.IsNotExist(err) {
		t.Error("handlers_crud.go should not be scaffolded when no CRUD methods remain")
	}
}

// TestGenerateCRUDHandlers_KeepsUserOwnedLegacyGen pins the ownership guard
// on the legacy-file sweep: a handlers_crud_gen.go whose manifest entry is
// Tier-2 (disowned / user-owned) is the USER's file now — the retirement
// sweep must leave both the file and its manifest entry alone.
func TestGenerateCRUDHandlers_KeepsUserOwnedLegacyGen(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte("package patients\n\ntype Deps struct{}\ntype Service struct{ deps Deps }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	legacyRel := filepath.Join("handlers", "patients", "handlers_crud_gen.go")
	legacyPath := filepath.Join(projectDir, legacyRel)
	legacyBody := "// Code generated by forge. DO NOT EDIT.\npackage patients\n\n// user took ownership of this file\n"
	if err := os.WriteFile(legacyPath, []byte(legacyBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		legacyRel: {Tier: 2, Disowned: true},
	}}

	svc := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
		},
	}
	entities := []EntityDef{{
		Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64",
		Fields: []EntityField{{Name: "id", GoName: "ID", GoType: "int64"}},
	}}

	if err := GenerateCRUDHandlers(svc, MatchCRUDMethods(svc, entities), "example.com/test", projectDir, cs); err != nil {
		t.Fatalf("GenerateCRUDHandlers() error = %v", err)
	}

	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("user-owned legacy handlers_crud_gen.go was deleted: %v", err)
	}
	if string(after) != legacyBody {
		t.Error("user-owned legacy handlers_crud_gen.go was modified")
	}
	if _, ok := cs.Files[legacyRel]; !ok {
		t.Error("manifest entry for user-owned legacy file was dropped")
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
	// Real projects always have a service.go in the handler dir before CRUD
	// gen runs; the disk-first resolver needs at least one parseable .go
	// file to read the package clause from.
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte("package patients\n"), 0o644); err != nil {
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
			Name:       "Patient",
			TableName:  "patients",
			PkField:    "id",
			PkGoType:   "string",
			Timestamps: true,
			Fields: []EntityField{
				{Name: "id", GoName: "Id", ProtoType: "string", GoType: "string"},
				{Name: "name", GoName: "Name", ProtoType: "string", GoType: "string"},
				{Name: "active", GoName: "Active", ProtoType: "bool", GoType: "bool"},
			},
		},
	}

	crudMethods := MatchCRUDMethods(svc, entities)

	err := GenerateCRUDTests(svc, crudMethods, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	// The legacy marker-scaffold pair is no longer emitted.
	for _, retired := range []string{"handlers_crud_gen_test.go", "handlers_crud_integration_test.go"} {
		if _, err := os.Stat(filepath.Join(handlerDir, retired)); !os.IsNotExist(err) {
			t.Errorf("retired %s should not be generated", retired)
		}
	}

	// The replacement: ONE user-owned lifecycle test with real semantics.
	lifecyclePath := filepath.Join(handlerDir, "handlers_crud_test.go")
	lifecycleData, err := os.ReadFile(lifecyclePath)
	if err != nil {
		t.Fatalf("scaffolded handlers_crud_test.go not found: %v", err)
	}
	content := string(lifecycleData)

	if !contains(content, "package patients_test") {
		t.Error("expected package patients_test in lifecycle output")
	}
	if !contains(content, "TestCRUD_Patient_Lifecycle") {
		t.Error("expected TestCRUD_Patient_Lifecycle in lifecycle output")
	}

	// User-owned from line one: no Tier-1 banner, no scaffold markers.
	if contains(content, "DO NOT EDIT") {
		t.Error("lifecycle test is user-owned; must not carry a DO NOT EDIT banner")
	}
	if contains(content, "FORGE_SCAFFOLD:") {
		t.Error("lifecycle test is user-owned; must not carry FORGE_SCAFFOLD markers")
	}

	// Real-semantics assertions, not AnyOutcome frames.
	if !contains(content, "create must be an insert, never an upsert") {
		t.Error("expected distinct-id (insert-not-upsert) assertion in lifecycle output")
	}
	if !contains(content, "connect.CodeNotFound") {
		t.Error("expected clean NotFound assertion in lifecycle output")
	}
	// All 5 ops matched: list + update + delete sections render too.
	if !contains(content, "svc.ListPatients") {
		t.Error("expected list section in lifecycle output")
	}
	if !contains(content, "lifecycle-updated") {
		t.Error("expected update-mutates assertion (MutableStringField) in lifecycle output")
	}
	if !contains(content, "svc.DeletePatient") {
		t.Error("expected delete section in lifecycle output")
	}
	// timestamps: true on the entity → created_at asserted set.
	if !contains(content, "GetCreatedAt()") {
		t.Error("expected created_at assertion for timestamps:true entity")
	}

	// Must parse as valid Go.
	if _, err := parser.ParseFile(token.NewFileSet(), lifecyclePath, content, parser.SkipObjectResolution); err != nil {
		t.Errorf("lifecycle output is not valid Go: %v\n----\n%s", err, content)
	}

	// Scaffold-once: a later run must not touch the (now user-owned) file.
	custom := "package patients_test\n\n// customized by the user\n"
	if err := os.WriteFile(lifecyclePath, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GenerateCRUDTests(svc, crudMethods, "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDTests() second run error = %v", err)
	}
	after, err := os.ReadFile(lifecyclePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != custom {
		t.Error("handlers_crud_test.go was rewritten on a later run; it must be scaffold-once")
	}
}

func TestGenerateCRUDTests_PartialCRUD(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real projects always have a service.go in the handler dir before CRUD
	// gen runs; the disk-first resolver needs at least one parseable .go
	// file to read the package clause from.
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte("package patients\n"), 0o644); err != nil {
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
			PkGoType:  "string",
			Fields: []EntityField{
				{Name: "id", GoName: "Id", ProtoType: "string", GoType: "string"},
				{Name: "name", GoName: "Name", ProtoType: "string", GoType: "string"},
			},
		},
	}

	crudMethods := MatchCRUDMethods(svc, entities)

	err := GenerateCRUDTests(svc, crudMethods, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	lifecyclePath := filepath.Join(handlerDir, "handlers_crud_test.go")
	lifecycleData, err := os.ReadFile(lifecyclePath)
	if err != nil {
		t.Fatalf("scaffolded handlers_crud_test.go not found: %v", err)
	}
	content := string(lifecycleData)

	// create+get with a string PK qualifies — the lifecycle test renders
	// covering exactly the ops that exist.
	if !contains(content, "TestCRUD_Patient_Lifecycle") {
		t.Error("expected TestCRUD_Patient_Lifecycle for create+get string-PK entity")
	}

	// Missing ops must not be referenced.
	for _, absent := range []string{"svc.ListPatients", "svc.UpdatePatient", "svc.DeletePatient"} {
		if contains(content, absent) {
			t.Errorf("lifecycle output should not reference %s (op not in proto)", absent)
		}
	}

	if _, err := parser.ParseFile(token.NewFileSet(), lifecyclePath, content, parser.SkipObjectResolution); err != nil {
		t.Errorf("lifecycle output is not valid Go: %v\n----\n%s", err, content)
	}
}

// TestGenerateCRUDTests_SkipsNonQualifyingEntities pins the qualification
// gate: the lifecycle test exercises forge's CRUD convention (string PK,
// create+get); an entity outside it gets no scaffold at all.
func TestGenerateCRUDTests_SkipsNonQualifyingEntities(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte("package patients\n"), 0o644); err != nil {
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

	// int64 PK — outside the string-PK CRUD convention.
	entities := []EntityDef{{
		Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "int64",
		Fields: []EntityField{
			{Name: "id", GoName: "Id", ProtoType: "int64", GoType: "int64"},
			{Name: "name", GoName: "Name", ProtoType: "string", GoType: "string"},
		},
	}}

	if err := GenerateCRUDTests(svc, MatchCRUDMethods(svc, entities), "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(handlerDir, "handlers_crud_test.go")); !os.IsNotExist(err) {
		t.Error("expected no handlers_crud_test.go for a non-string-PK entity")
	}
}

func TestGenerateCRUDTests_CleanupWhenNoMethods(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real projects always have a service.go in the handler dir before CRUD
	// gen runs; the disk-first resolver needs at least one parseable .go
	// file to read the package clause from.
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte("package patients\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create stale retired test gen files (both unit and integration
	// variants) still carrying FORGE_SCAFFOLD markers — i.e. forge-owned.
	unitPath := filepath.Join(handlerDir, "handlers_crud_gen_test.go")
	if err := os.WriteFile(unitPath, []byte("// FORGE_SCAFFOLD: stale\npackage patients_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	integrationPath := filepath.Join(handlerDir, "handlers_crud_integration_test.go")
	if err := os.WriteFile(integrationPath, []byte("//go:build integration\n// FORGE_SCAFFOLD: stale\npackage patients_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{Name: "PatientsService"}

	err := GenerateCRUDTests(svc, nil, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	// Stale forge-owned files should be removed
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Error("stale handlers_crud_gen_test.go should have been removed")
	}
	if _, err := os.Stat(integrationPath); !os.IsNotExist(err) {
		t.Error("stale handlers_crud_integration_test.go should have been removed")
	}

	// A legacy file whose markers the user cleared is user-owned: kept.
	customized := "package patients_test\n\n// user customized: markers cleared\n"
	if err := os.WriteFile(unitPath, []byte(customized), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GenerateCRUDTests(svc, nil, "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDTests() second run error = %v", err)
	}
	after, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("user-owned legacy test file was removed: %v", err)
	}
	if string(after) != customized {
		t.Error("user-owned legacy test file was modified")
	}
}

// TestGenerateCRUDTests_SkipsExistingMethods mirrors
// TestGenerateCRUDHandlers_SkipsExistingMethods: once the user has taken
// ownership of a CRUD method by writing a real handler, the test
// generator must stop treating it as forge-wired. With CreatePatient
// hand-implemented, the Patient entity no longer has a forge-owned
// create+get pair, so the lifecycle scaffold (which drives the GENERATED
// CRUD stack) must not be emitted at all — it would exercise the user's
// bespoke handler with conventions it never promised to follow.
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
			PkGoType:  "string",
			Fields: []EntityField{
				{Name: "id", GoName: "Id", ProtoType: "string", GoType: "string"},
				{Name: "name", GoName: "Name", ProtoType: "string", GoType: "string"},
			},
		},
	}

	crudMethods := MatchCRUDMethods(svc, entities)

	if err := GenerateCRUDTests(svc, crudMethods, "example.com/test", projectDir, nil); err != nil {
		t.Fatalf("GenerateCRUDTests() error = %v", err)
	}

	// CreatePatient is user-owned, so Patient has no forge-wired create:
	// the entity no longer qualifies and no lifecycle test is scaffolded.
	if _, err := os.Stat(filepath.Join(handlerDir, "handlers_crud_test.go")); !os.IsNotExist(err) {
		t.Error("expected no handlers_crud_test.go when the user owns the create method")
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
			Entity:    EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64", Timestamps: true, Fields: []EntityField{{Name: "id", GoName: "ID", GoType: "int64"}, {Name: "name", GoName: "Name", GoType: "string"}}},
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
	if !ent.HasTimestamps {
		t.Error("expected HasTimestamps=true (entity annotation timestamps:true)")
	}
	if ent.MutableStringField != "Name" {
		t.Errorf("MutableStringField = %q, want Name (first non-PK string field)", ent.MutableStringField)
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
			Method: MethodTemplateData{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			Entity: EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64", Fields: []EntityField{
				{Name: "id", GoName: "ID", ProtoType: "int64", GoType: "int64"},
				{Name: "name", GoName: "Name", ProtoType: "string", GoType: "string"},
				{Name: "status", GoName: "Status", ProtoType: "string", GoType: "string"},
				{Name: "active", GoName: "Active", ProtoType: "bool", GoType: "bool"},
			}},
			Operation: "list",
		},
	}

	data, err := buildCRUDTemplateData(svc, crudMethods, "example.com/test")
	if err != nil {
		t.Fatalf("buildCRUDTemplateData() error = %v", err)
	}

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

	// The search filter spans the entity's non-PK string columns; it never
	// maps to a column of its own.
	search := m.FilterFields[1]
	if len(search.SearchColumns) != 2 || search.SearchColumns[0] != "name" || search.SearchColumns[1] != "status" {
		t.Errorf("search.SearchColumns = %v, want [name status]", search.SearchColumns)
	}
}

// TestBuildCRUDTemplateData_FilterMappingErrors pins the loud-failure
// contract for filter mapping: an exact filter must name a DECLARED entity
// column, and a search filter needs at least one non-PK string column to
// span. Shipping either as a phantom-column query was the
// review-confirmed silence bug.
func TestBuildCRUDTemplateData_FilterMappingErrors(t *testing.T) {
	listMethod := func(entity EntityDef) []CRUDMethod {
		return []CRUDMethod{{
			Method:    MethodTemplateData{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			Entity:    entity,
			Operation: "list",
		}}
	}

	t.Run("exact_filter_with_no_matching_column_errors", func(t *testing.T) {
		svc := ServiceDef{
			Name: "PatientsService",
			Messages: map[string][]MessageFieldDef{
				"ListPatientsRequest": {
					{Name: "page_size", ProtoType: "int32"},
					{Name: "page_token", ProtoType: "string"},
					{Name: "favorite_color", ProtoType: "string"},
				},
			},
		}
		entity := EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64", Fields: []EntityField{
			{Name: "id", GoName: "ID", ProtoType: "int64", GoType: "int64"},
			{Name: "name", GoName: "Name", ProtoType: "string", GoType: "string"},
		}}
		_, err := buildCRUDTemplateData(svc, listMethod(entity), "example.com/test")
		if err == nil {
			t.Fatal("expected error for exact filter naming no declared column")
		}
		if !strings.Contains(err.Error(), "favorite_color") {
			t.Errorf("error should name the unmappable filter field; got: %v", err)
		}
	})

	t.Run("search_filter_with_no_string_columns_errors", func(t *testing.T) {
		svc := ServiceDef{
			Name: "PatientsService",
			Messages: map[string][]MessageFieldDef{
				"ListPatientsRequest": {
					{Name: "page_size", ProtoType: "int32"},
					{Name: "page_token", ProtoType: "string"},
					{Name: "search", ProtoType: "string"},
				},
			},
		}
		entity := EntityDef{Name: "Patient", PkField: "id", PkGoType: "int64", Fields: []EntityField{
			{Name: "id", GoName: "ID", ProtoType: "int64", GoType: "int64"},
			{Name: "age", GoName: "Age", ProtoType: "int32", GoType: "int32"},
		}}
		_, err := buildCRUDTemplateData(svc, listMethod(entity), "example.com/test")
		if err == nil {
			t.Fatal("expected error for search filter on an entity with no string columns")
		}
		if !strings.Contains(err.Error(), "search") {
			t.Errorf("error should name the search filter; got: %v", err)
		}
	})
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
			{Name: "id", GoName: "ID", ProtoType: "int64", GoType: "int64"},
			{Name: "name", GoName: "Name", ProtoType: "string", GoType: "string"},
			{Name: "active", GoName: "Active", ProtoType: "bool", GoType: "bool"},
		},
	}}

	crudMethods := MatchCRUDMethods(svc, entities)

	err := GenerateCRUDHandlers(svc, crudMethods, "example.com/test", projectDir, nil)
	if err != nil {
		t.Fatalf("GenerateCRUDHandlers() error = %v", err)
	}

	opsPath := filepath.Join(handlerDir, "handlers_crud_ops_gen.go")
	data, err := os.ReadFile(opsPath)
	if err != nil {
		t.Fatalf("generated ops file not found: %v", err)
	}

	content := string(data)

	// Tier-1 file: must carry the generated banner.
	if !contains(content, "Code generated by forge. DO NOT EDIT.") {
		t.Error("expected DO NOT EDIT banner in ops file")
	}

	// Should contain the unexported list-op constructor with the entity's
	// declared-column allowlist.
	if !contains(content, "func (s *Service) crudListPatientsOp() crud.ListOp[") {
		t.Error("expected crudListPatientsOp constructor in ops file")
	}
	if !contains(content, "Columns:       db.PatientColumns") {
		t.Error("expected Columns: db.PatientColumns allowlist in ListOp literal")
	}

	// Should contain orm import
	if !contains(content, "pkg/orm") {
		t.Error("expected orm import in ops file")
	}

	// Search spans the entity's string columns via WhereILikeAny — never
	// a phantom "search" column.
	if !contains(content, `orm.WhereILikeAny([]string{"name"`) || !contains(content, `"%"+*req.Search+"%"`) {
		t.Error("expected WhereILikeAny over entity string columns for search filter")
	}
	if contains(content, `WhereILike("search"`) {
		t.Error("search filter must not hit a phantom search column via WhereILike")
	}
	if !contains(content, "WhereEq") {
		t.Error("expected WhereEq for exact filter")
	}

	// Should hand off ordering to the crud library (lifecycle lives in
	// pkg/crud; the op only wires request accessors).
	if !contains(content, "HasOrderBy:    true") {
		t.Error("expected HasOrderBy: true in generated ListOp literal")
	}
	if !contains(content, "req.OrderBy") {
		t.Error("expected req.OrderBy accessor closure in ops file")
	}
	if !contains(content, "req.Descending") {
		t.Error("expected req.Descending accessor closure in ops file")
	}

	// Should contain filter nil check for optional fields (inside the
	// per-RPC Filters closure).
	if !contains(content, "req.Active != nil") {
		t.Error("expected nil check for optional Active filter")
	}
	if !contains(content, "req.Search != nil") {
		t.Error("expected nil check for optional Search filter")
	}

	// Ops file must parse as valid Go.
	if _, err := parser.ParseFile(token.NewFileSet(), opsPath, content, parser.SkipObjectResolution); err != nil {
		t.Errorf("ops file is not valid Go: %v\n----\n%s", err, content)
	}

	// The RPC method itself lives in the user-owned shim and delegates.
	shimData, err := os.ReadFile(filepath.Join(handlerDir, "handlers_crud.go"))
	if err != nil {
		t.Fatalf("scaffolded handlers_crud.go not found: %v", err)
	}
	shim := string(shimData)
	if !contains(shim, "func (s *Service) ListPatients(") {
		t.Error("expected ListPatients method in handlers_crud.go")
	}
	if !contains(shim, "crud.HandleList(s.crudListPatientsOp())(ctx, req)") {
		t.Error("expected ListPatients to delegate to crud.HandleList(s.crudListPatientsOp())")
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "handlers_crud.go", shim, parser.SkipObjectResolution); err != nil {
		t.Errorf("handlers_crud.go is not valid Go: %v\n----\n%s", err, shim)
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

	data, err := buildCRUDTemplateData(svc, crudMethods, "example.com/test")
	if err != nil {
		t.Fatalf("buildCRUDTemplateData() error = %v", err)
	}

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
			// A message-typed update field carrying the entity's name in
			// MessageType matches: ProtoType is the unmatchable literal
			// "message" for every message field, and relying on it alone
			// produced a false mismatch on forge's own scaffold (F2 bug).
			name: "update_request_messagetype_matches",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"UpdateMarketRequest": {
						{Name: "market", ProtoType: "message", MessageType: "Market"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "UpdateMarket", InputType: "UpdateMarketRequest", OutputType: "UpdateMarketResponse"},
				Entity:    market,
				Operation: "update",
			},
			wantOK: true,
		},
		{
			name: "update_request_qualified_messagetype_matches",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"UpdateMarketRequest": {
						{Name: "market", ProtoType: "message", MessageType: "db.v1.Market"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "UpdateMarket", InputType: "UpdateMarketRequest", OutputType: "UpdateMarketResponse"},
				Entity:    market,
				Operation: "update",
			},
			wantOK: true,
		},
		{
			// The observed-fields diagnostic must show the real message
			// type (MessageType), not the useless literal "message" — a
			// wrong matcher self-incriminates.
			name: "mismatch_reason_shows_observed_message_types",
			svc: ServiceDef{
				Messages: map[string][]MessageFieldDef{
					"UpdateMarketRequest": {
						{Name: "update_mask", ProtoType: "message", MessageType: "google.protobuf.FieldMask"},
					},
				},
			},
			cm: CRUDMethod{
				Method:    MethodTemplateData{Name: "UpdateMarket", InputType: "UpdateMarketRequest", OutputType: "UpdateMarketResponse"},
				Entity:    market,
				Operation: "update",
			},
			wantOK:    false,
			reasonHas: "observed fields: update_mask google.protobuf.FieldMask",
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
// messages don't fit the CRUD conventions, the package must still compile
// (and satisfy the proto service interface). In the projection/implementation
// split that means: no op constructor is rendered into the Tier-1 ops file,
// and the user-owned handlers_crud.go shim carries a tagged
// CodeUnimplemented stub for each mismatched RPC rather than a delegation
// that would dereference fields the request type doesn't have.
//
// Each method pins one of the kalshi-trader shapes:
//   - ListMarkets: `limit/cursor` instead of `page_size/page_token`
//   - GetMarket:   `ticker` instead of `id`
//   - CreateMarket: response has `repeated Market markets` not `Market market`
//
// Together they prove buildCRUDTemplateData + the shim template fall back
// to a stub for every operation where validateCRUDShape returns false.
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

	// 1. Every method failed shape validation, so the Tier-1 ops file has
	//    nothing to wire and must not exist.
	if _, err := os.Stat(filepath.Join(handlerDir, "handlers_crud_ops_gen.go")); !os.IsNotExist(err) {
		t.Error("handlers_crud_ops_gen.go should not be written when every method is shape-mismatched")
	}

	// 2. The explanatory stubs land in the user-owned shim instead.
	data, err := os.ReadFile(filepath.Join(handlerDir, "handlers_crud.go"))
	if err != nil {
		t.Fatalf("read handlers_crud.go: %v", err)
	}
	content := string(data)

	// 3. Every method should be emitted (so the proto interface is satisfied).
	for _, want := range []string{"func (s *Service) ListMarkets(", "func (s *Service) GetMarket(", "func (s *Service) CreateMarket("} {
		if !strings.Contains(content, want) {
			t.Errorf("missing handler declaration %q in:\n%s", want, content)
		}
	}

	// 4. Each method must carry the FORGE_CRUD_SHAPE_MISMATCH marker (so
	//    the user can grep for it), and the reason must self-incriminate
	//    by listing the fields the matcher actually observed.
	if mismatches := strings.Count(content, "FORGE_CRUD_SHAPE_MISMATCH"); mismatches < 3 {
		t.Errorf("expected >=3 FORGE_CRUD_SHAPE_MISMATCH markers, got %d in:\n%s", mismatches, content)
	}
	if !strings.Contains(content, "observed fields:") {
		t.Errorf("expected mismatch reasons to carry the observed-fields diagnostic:\n%s", content)
	}

	// 5. None of the AIP-158 dereferences (or crud delegations) that would
	//    fail to compile against this proto should appear in the output.
	for _, bad := range []string{"req.PageSize", "req.PageToken", "crud.HandleList(", "crud.HandleGet(", "crud.HandleCreate("} {
		if strings.Contains(content, bad) {
			t.Errorf("unexpected %q in mismatch-only output:\n%s", bad, content)
		}
	}

	// 6. Each stub must return CodeUnimplemented so callers get a clear
	//    error instead of a silent nil response.
	if c := strings.Count(content, "connect.CodeUnimplemented"); c != 3 {
		t.Errorf("expected 3 CodeUnimplemented returns, got %d", c)
	}

	// 7. The scaffolded file must parse as valid Go — the whole point of
	//    the stub fallback is that the package keeps compiling against
	//    the real proto shape.
	if _, err := parser.ParseFile(token.NewFileSet(), "handlers_crud.go", content, parser.SkipObjectResolution); err != nil {
		t.Errorf("generated file is not valid Go: %v\n----\n%s", err, content)
	}

	// 8. Because every method is a stub, the import block must not pull
	//    in pkg/crud or internal/db (they would be unused and trip the
	//    compiler).
	for _, bad := range []string{`"github.com/reliant-labs/forge/pkg/crud"`, `"example.com/test/internal/db"`, `"example.com/test/pkg/middleware"`} {
		if strings.Contains(content, bad) {
			t.Errorf("expected no import of %s when every method is a stub; got:\n%s", bad, content)
		}
	}
}

// TestGenerateCRUDHandlers_ShimScaffoldOnceAndAppend pins the ownership
// semantics of the implementation half of the split: handlers_crud.go is
// scaffolded exactly once (tracked Tier-2), the user's edits are never
// rewritten, and CRUD RPCs added later get their shim APPENDED without
// touching existing content.
func TestGenerateCRUDHandlers_ShimScaffoldOnceAndAppend(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

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

	entities := []EntityDef{{
		Name: "Patient", TableName: "patients", PkField: "id", PkGoType: "string",
		Fields: []EntityField{
			{Name: "id", GoName: "Id", ProtoType: "string", GoType: "string"},
			{Name: "name", GoName: "Name", ProtoType: "string", GoType: "string"},
		},
	}}

	svcV1 := ServiceDef{
		Name:       "PatientsService",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
		},
	}

	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{}}
	if err := GenerateCRUDHandlers(svcV1, MatchCRUDMethods(svcV1, entities), "example.com/test", projectDir, cs); err != nil {
		t.Fatalf("GenerateCRUDHandlers() first run error = %v", err)
	}

	shimRel := filepath.Join("handlers", "patients", "handlers_crud.go")
	shimPath := filepath.Join(projectDir, shimRel)
	first, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("handlers_crud.go not scaffolded: %v", err)
	}
	if !contains(string(first), "func (s *Service) CreatePatient(") {
		t.Fatalf("expected CreatePatient shim in scaffold; got:\n%s", first)
	}
	// Tracked Tier-2 (user-owned) in the manifest, so cleanup sweeps and
	// the Tier-1 stomp guard leave it alone.
	if entry, ok := cs.Files[shimRel]; !ok {
		t.Error("handlers_crud.go missing from checksum manifest")
	} else if entry.Tier != 2 {
		t.Errorf("handlers_crud.go manifest Tier = %d, want 2 (user-owned)", entry.Tier)
	}

	// The user customizes the file: replaces the delegation body.
	customized := strings.Replace(string(first),
		"return crud.HandleCreate(s.crudCreatePatientOp())(ctx, req)",
		"// user customization marker\n\treturn crud.HandleCreate(s.crudCreatePatientOp())(ctx, req)", 1)
	if customized == string(first) {
		t.Fatalf("expected CreatePatient shim to delegate via crud.HandleCreate; got:\n%s", first)
	}
	if err := os.WriteFile(shimPath, []byte(customized), 0o644); err != nil {
		t.Fatal(err)
	}

	// A new CRUD RPC appears in the proto: GetPatient.
	svcV2 := svcV1
	svcV2.Methods = []Method{
		{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
		{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
	}
	if err := GenerateCRUDHandlers(svcV2, MatchCRUDMethods(svcV2, entities), "example.com/test", projectDir, cs); err != nil {
		t.Fatalf("GenerateCRUDHandlers() second run error = %v", err)
	}

	second, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(second)

	// Existing content (including the user's edit) is preserved verbatim
	// as a prefix-or-better: never modified, never duplicated.
	if !contains(got, "// user customization marker") {
		t.Errorf("user customization was lost on regen:\n%s", got)
	}
	if c := strings.Count(got, "func (s *Service) CreatePatient("); c != 1 {
		t.Errorf("CreatePatient declared %d times after append, want exactly 1", c)
	}
	// The new RPC's shim was appended.
	if !contains(got, "func (s *Service) GetPatient(") {
		t.Errorf("expected GetPatient shim appended on second run:\n%s", got)
	}
	if !contains(got, "crud.HandleGet(s.crudGetPatientOp())(ctx, req)") {
		t.Errorf("appended GetPatient shim must delegate to crud.HandleGet:\n%s", got)
	}
	// And the result still parses as Go (import insertion + append must
	// not corrupt the file).
	if _, err := parser.ParseFile(token.NewFileSet(), "handlers_crud.go", got, parser.SkipObjectResolution); err != nil {
		t.Errorf("handlers_crud.go is not valid Go after append: %v\n----\n%s", err, got)
	}

	// A third run with no new methods is a no-op.
	if err := GenerateCRUDHandlers(svcV2, MatchCRUDMethods(svcV2, entities), "example.com/test", projectDir, cs); err != nil {
		t.Fatalf("GenerateCRUDHandlers() third run error = %v", err)
	}
	third, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(third) != got {
		t.Error("handlers_crud.go changed on a run with no new CRUD methods")
	}

	// The Tier-1 ops file regenerates every run and carries both ops.
	ops, err := os.ReadFile(filepath.Join(handlerDir, "handlers_crud_ops_gen.go"))
	if err != nil {
		t.Fatalf("handlers_crud_ops_gen.go not written: %v", err)
	}
	for _, want := range []string{"crudCreatePatientOp", "crudGetPatientOp"} {
		if !contains(string(ops), want) {
			t.Errorf("expected %s in ops file", want)
		}
	}
}
