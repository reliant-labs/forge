package codegen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinzhu/inflection"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// CRUDMethod holds the correlation between an RPC method and a database entity.
type CRUDMethod struct {
	Method    MethodTemplateData // The RPC
	Entity    EntityDef          // The matched entity
	Operation string             // "create", "get", "list", "update", "delete"
}

// CRUDTemplateData holds all data needed to render the CRUD handlers template.
type CRUDTemplateData struct {
	Package       string // Go package name, e.g. "patients"
	Module        string // Go module path, e.g. "github.com/demo-project"
	ProtoPackage  string // e.g. "proto/services/patients"
	DBPackagePath string // e.g. "github.com/demo-project/gen/db/v1"
	HasPagination bool   // true if any list method uses pagination
	HasFilters    bool   // true if any list method has filter fields
	HasOrderBy    bool   // true if any list method has order_by
	NeedsORM      bool   // true if pagination, filters, or ordering requires orm import
	HasTenant     bool   // true if any CRUD method operates on a tenant-scoped entity
	// NeedsCRUDLib is true when at least one method emits a real CRUD
	// body (i.e. uses pkg/crud, internal/db, middleware). When every
	// method's request/response shape failed validation and we emit
	// only TODO stubs, the template skips those imports to keep the
	// file compiling.
	NeedsCRUDLib bool
	CRUDMethods  []CRUDMethodTemplateData
}

// CRUDMethodTemplateData holds per-method template data.
type CRUDMethodTemplateData struct {
	MethodName        string // "CreatePatient"
	InputType         string // "CreatePatientRequest"
	OutputType        string // "CreatePatientResponse"
	EntityName        string // "Patient"
	EntityLower       string // "patient"
	Operation         string // "create", "get", "list", "update", "delete"
	AuthRequired      bool
	AuthAction        string // "create", "read", "list", "update", "delete" (middleware constant)
	PkField           string // "Id" (proto PascalCase Go field name)
	PkColumnName      string // "id" (raw DB column name)
	PkGoType          string // "int64"
	HasPkInInput      bool   // true if the request message likely has an ID field
	ResponseField     string // "Patient" — the proto field name in the response that holds the entity
	HasPagination     bool   // true when List method's InputType follows AIP-158 convention
	PaginationStyle   string // "cursor" (default for now)
	HasFilters        bool   // true if list method has filter fields
	FilterFields      []FilterFieldData
	HasOrderBy        bool              // true if list method has order_by field
	HasTenant         bool              // true when the entity has a tenant key field
	TenantGoName      string            // e.g., "OrgId", "TenantId" (PascalCase Go field name on entity)
	TenantColumnName  string            // e.g., "org_id", "tenant_id"
	UpdateEntityField string            // e.g., "Project" — Go field name in the update request that holds the entity
	CreateFields      []CreateFieldData // fields from the create request message
	// ShapeMismatch is true when the request/response message shapes
	// observed in svc.Messages don't line up with what the CRUD body
	// templates assume (AIP-158 page_size/page_token for list, an `id`
	// scalar key for get/update/delete, an entity-typed response field,
	// etc.). When true the template emits a tagged TODO stub returning
	// CodeUnimplemented rather than CRUD-body code that wouldn't compile
	// against the real proto. See validateCRUDShape for the rules. The
	// stub carries a FORGE_CRUD_SHAPE_MISMATCH marker plus MismatchReason
	// so the user (and any future `forge audit`) can spot it.
	ShapeMismatch  bool
	MismatchReason string
}

// CreateFieldData holds a field mapping from a create request to the ORM entity.
type CreateFieldData struct {
	ProtoGoName  string    // Go field name on the proto request message, e.g. "Name"
	EntityGoName string    // Go field name on the ORM entity, e.g. "Name"
	Kind         FieldKind // scalar, enum, message, wrapper, timestamp, etc.
	GoType       string    // Go type: "string", "int32", "*timestamppb.Timestamp", etc.
	EnumGoType   string    // For enum fields: the pb.EnumType name
}

// FilterFieldData describes a filter field extracted from a List request message.
type FilterFieldData struct {
	ProtoName  string // e.g., "active", "search", "status"
	GoName     string // PascalCase: "Active", "Search", "Status"
	ColumnName string // DB column: "active", "status"
	FieldType  string // "bool", "string", "int32", "int64"
	FilterType string // "exact", "search"
	IsOptional bool   // proto optional keyword
}

// MatchCRUDMethods correlates a service's RPC methods with entity definitions
// and returns the matched CRUD methods. Only unary RPCs are matched.
func MatchCRUDMethods(svc ServiceDef, entities []EntityDef) []CRUDMethod {
	entityMap := make(map[string]EntityDef)
	for _, e := range entities {
		entityMap[strings.ToLower(e.Name)] = e
	}

	var matches []CRUDMethod
	for _, m := range svc.Methods {
		// Skip streaming methods — CRUD is unary only
		if m.ClientStreaming || m.ServerStreaming {
			continue
		}

		op, entityName := parseCRUDOperation(m.Name)
		if op == "" {
			continue
		}

		// Try to find the entity (case-insensitive)
		entity, ok := entityMap[strings.ToLower(entityName)]
		if !ok {
			// For "list", the method name uses plural — try singular
			if op == "list" {
				singular := inflection.Singular(entityName)
				entity, ok = entityMap[strings.ToLower(singular)]
			}
			if !ok {
				continue
			}
		}

		mtd := MethodTemplateData{
			Name:         m.Name,
			InputType:    m.InputType,
			OutputType:   m.OutputType,
			AuthRequired: m.AuthRequired,
		}

		matches = append(matches, CRUDMethod{
			Method:    mtd,
			Entity:    entity,
			Operation: op,
		})
	}
	return matches
}

// ParseCRUDOperation extracts the CRUD operation and entity name from a
// method name. Returns ("", "") if the method doesn't match a CRUD
// pattern. Exported so the CLI's webhook-only detection can ask the
// same question MatchCRUDMethods does without re-implementing the
// prefix list. Internal callers should keep using parseCRUDOperation;
// they're identical.
func ParseCRUDOperation(methodName string) (operation, entityName string) {
	return parseCRUDOperation(methodName)
}

// parseCRUDOperation extracts the CRUD operation and entity name from a method name.
// Returns ("", "") if the method doesn't match a CRUD pattern.
func parseCRUDOperation(methodName string) (operation, entityName string) {
	prefixes := []struct {
		prefix string
		op     string
	}{
		{"Create", "create"},
		{"Get", "get"},
		{"List", "list"},
		{"Update", "update"},
		{"Delete", "delete"},
	}

	for _, p := range prefixes {
		if strings.HasPrefix(methodName, p.prefix) {
			name := strings.TrimPrefix(methodName, p.prefix)
			if name != "" {
				return p.op, name
			}
		}
	}
	return "", ""
}

// GenerateCRUDHandlers generates handlers_crud_gen.go for a service with CRUD methods.
// It skips methods that already exist in user-owned handler files.
//
// cs is the project's checksum tracker. Passing it ensures the rendered
// handlers_crud_gen.go is recorded so it doesn't show up as an orphan in
// `forge audit`. A nil cs is tolerated.
func GenerateCRUDHandlers(svc ServiceDef, crudMethods []CRUDMethod, modulePath string, projectDir string, cs *checksums.FileChecksums) error {
	// Disk-first: handlers_crud_gen.go lands inside the EXISTING handler
	// dir and must declare that dir's real package clause. Re-synthesizing
	// the path from the proto name is how the historical
	// handlers/adminserver-vs-admin_server duplicate-dir bug was created.
	res, resErr := ResolveServiceComponent(projectDir, svc.Name)
	if resErr != nil {
		return resErr
	}
	pkg := res.PackageName
	targetDir := res.Dir

	// Scan existing user-owned methods to avoid generating duplicates
	existingMethods, err := scanExistingMethods(targetDir, false)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("scan existing methods for %s: %w", pkg, err)
	}

	// Filter out methods that already exist
	var filteredMethods []CRUDMethod
	for _, cm := range crudMethods {
		if existingMethods[cm.Method.Name] {
			continue
		}
		filteredMethods = append(filteredMethods, cm)
	}

	crudGenPath := filepath.Join(targetDir, "handlers_crud_gen.go")
	if len(filteredMethods) == 0 {
		// Clean up stale file if no CRUD methods needed
		_ = os.Remove(crudGenPath)
		return nil
	}

	// Ensure the Deps struct in service.go has a DB field for CRUD operations.
	if err := ensureDepsDBField(targetDir); err != nil {
		return fmt.Errorf("ensure Deps DB field for %s: %w", pkg, err)
	}

	// Build template data. Package is overridden with the disk-resolved
	// clause so the emitted file always matches the directory it lands in
	// (buildCRUDTemplateData's synthesis only holds for fresh scaffolds).
	data := buildCRUDTemplateData(svc, filteredMethods, modulePath)
	data.Package = pkg

	content, err := templates.ServiceTemplates().Render("handlers_crud_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_gen.go.tmpl: %w", err)
	}

	relPath := filepath.Join("handlers", filepath.FromSlash(res.ImportLeaf), "handlers_crud_gen.go")
	if _, err := checksums.WriteGeneratedFile(projectDir, relPath, content, cs, true); err != nil {
		return fmt.Errorf("write handlers_crud_gen.go: %w", err)
	}
	return nil
}

// validateCRUDShape decides whether the request/response messages observed
// in svc.Messages match the AIP-158-style shape the CRUD body template
// emits. It returns ok=true when:
//
//   - svc.Messages is empty (legacy/no-descriptor path — preserve old
//     behaviour and let the existing template fire), OR
//   - the input/output messages for this RPC are absent from
//     svc.Messages (we can't validate, so be lenient), OR
//   - every shape rule for the operation holds.
//
// When ok=false the returned reason describes the first failing rule.
// Callers should mark the method ShapeMismatch and skip emitting CRUD-body
// fields that would dereference unavailable proto fields (PageSize,
// PageToken, the entity-typed PK accessor, the repeated-entity response
// field). The handlers_crud_gen.go.tmpl template renders a tagged
// CodeUnimplemented stub in that branch so the generated file still
// compiles against bespoke proto shapes (Limit/enum filters, string Ticker
// keys, repeated-message responses).
//
// The rules deliberately stay conservative — they only fail when we can
// see message fields and prove they don't fit. Anything ambiguous (no
// Messages map at all, or this particular RPC's messages missing from it)
// is treated as ok so existing projects whose protos do match AIP-158
// keep generating the same code they did before this check landed.
func validateCRUDShape(svc ServiceDef, cm CRUDMethod) (ok bool, reason string) {
	if len(svc.Messages) == 0 {
		return true, ""
	}

	inputFields, inputKnown := svc.Messages[cm.Method.InputType]
	outputFields, outputKnown := svc.Messages[cm.Method.OutputType]

	inputByName := make(map[string]MessageFieldDef, len(inputFields))
	for _, f := range inputFields {
		inputByName[f.Name] = f
	}
	outputByName := make(map[string]MessageFieldDef, len(outputFields))
	for _, f := range outputFields {
		outputByName[f.Name] = f
	}

	switch cm.Operation {
	case "get", "delete":
		if !inputKnown {
			return true, ""
		}
		// PkField on the entity is the snake_case proto name (e.g.
		// "id", "ticker"). The CRUD template emits `req.<PascalPk>`
		// and downstream code calls Get/DeleteByID with that value.
		// If the request has no field with that name at all, the
		// generated body won't compile.
		if _, has := inputByName[cm.Entity.PkField]; !has {
			return false, fmt.Sprintf("request %s has no %s field matching entity PK", cm.Method.InputType, cm.Entity.PkField)
		}
	case "update":
		if !inputKnown {
			return true, ""
		}
		// Update body dereferences `req.<EntityField>` and expects
		// it to be a *db.<Entity>. Validate the request actually
		// carries an entity-typed field.
		found := false
		for _, f := range inputFields {
			if protoTypeMatchesEntity(f.ProtoType, cm.Entity.Name) {
				found = true
				break
			}
		}
		if !found {
			return false, fmt.Sprintf("request %s carries no %s message field", cm.Method.InputType, cm.Entity.Name)
		}
	case "list":
		// AIP-158-style template emits req.PageSize / req.PageToken
		// when the input type follows List*Request naming. If we
		// have field data and either is missing, the generated body
		// fails to compile against the real proto (kalshi-trader's
		// ListMarketsRequest carries `Limit` instead, for instance).
		if inputKnown && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			if _, hasSize := inputByName["page_size"]; !hasSize {
				return false, fmt.Sprintf("request %s lacks page_size (AIP-158 pagination assumed by template)", cm.Method.InputType)
			}
			if _, hasTok := inputByName["page_token"]; !hasTok {
				return false, fmt.Sprintf("request %s lacks page_token (AIP-158 pagination assumed by template)", cm.Method.InputType)
			}
		}
		// List response template emits `<EntityPlural>: items` and
		// optionally `NextPageToken: nextPageToken`. Validate the
		// response carries a repeated entity field by that name.
		if outputKnown {
			pluralLower := strings.ToLower(inflection.Plural(cm.Entity.Name))
			if _, has := outputByName[pluralLower]; !has {
				return false, fmt.Sprintf("response %s lacks repeated %s field %s", cm.Method.OutputType, cm.Entity.Name, pluralLower)
			}
		}
	case "create":
		// Create response template emits `<EntityName>: entity`.
		// Validate the response carries a single field of that type
		// (named after the entity in snake_case).
		if outputKnown {
			lower := strings.ToLower(cm.Entity.Name)
			if _, has := outputByName[lower]; !has {
				return false, fmt.Sprintf("response %s lacks %s field %s", cm.Method.OutputType, cm.Entity.Name, lower)
			}
		}
	}
	return true, ""
}

func buildCRUDTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath string) CRUDTemplateData {
	// Synthesized Package is a placeholder only: GenerateCRUDHandlers
	// overrides it with the disk-resolved package clause before rendering
	// (the file lands inside an EXISTING handler dir).
	pkg := naming.ServicePackage(svc.Name)

	// Build ProtoPackage path (same logic as mapServiceDefToTemplateData)
	protoPackage := ""
	if svc.ModulePath != "" && svc.GoPackage != "" {
		prefix := svc.ModulePath + "/gen/"
		if strings.HasPrefix(svc.GoPackage, prefix) {
			protoPackage = strings.TrimPrefix(svc.GoPackage, prefix)
			if idx := strings.LastIndex(protoPackage, "/v"); idx >= 0 {
				protoPackage = protoPackage[:idx]
			}
		}
	}

	var methods []CRUDMethodTemplateData
	for _, cm := range crudMethods {
		authAction := operationToAuthAction(cm.Operation)

		// Validate the request/response shape up front. When the
		// observed proto messages don't match the AIP-158-style body
		// the template emits, we still emit a method (so the proto's
		// RPC interface is satisfied) but route it to a tagged
		// CodeUnimplemented stub instead of the body. This keeps
		// handlers_crud_gen.go compiling against bespoke shapes
		// (Limit/enum filters, string Ticker keys, repeated-message
		// responses).
		shapeOK, shapeReason := validateCRUDShape(svc, cm)

		// Detect pagination for list operations: check if the input type
		// follows AIP-158 naming (List<Entity>Request implies page_size).
		// Skip when the shape didn't match — the stub branch doesn't
		// dereference PageSize/PageToken so suppressing keeps the
		// generated file from importing the crud lib unnecessarily.
		hasPagination := false
		paginationStyle := ""
		if shapeOK && cm.Operation == "list" && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			hasPagination = true
			paginationStyle = "cursor"
		}

		// Detect filters and ordering from request message fields.
		// Skip when the shape didn't match: classifyFilterField would
		// otherwise happily turn a bespoke field like `ticker` (a
		// string PK) or a `kalshi_status` enum into a synthetic
		// `WhereEq("ticker", req.Ticker)` clause that fails to compile
		// against the real request type.
		var filterFields []FilterFieldData
		hasOrderBy := false
		if shapeOK && cm.Operation == "list" && svc.Messages != nil {
			if msgFields, ok := svc.Messages[cm.Method.InputType]; ok {
				for _, mf := range msgFields {
					if classifySkipField(mf.Name) {
						continue
					}
					if mf.Name == "order_by" {
						hasOrderBy = true
						continue
					}
					ff := classifyFilterField(mf)
					filterFields = append(filterFields, ff)
				}
			}
		}

		// Determine the entity field name in the update request message.
		// Proto generates a field named after the entity (e.g., "Project project = 1;"
		// becomes Go field "Project"). We look it up in the parsed message fields;
		// if not found, we fall back to the entity name.
		updateEntityField := cm.Entity.Name
		if cm.Operation == "update" && svc.Messages != nil {
			if fields, ok := svc.Messages[cm.Method.InputType]; ok {
				for _, f := range fields {
					if protoTypeMatchesEntity(f.ProtoType, cm.Entity.Name) {
						updateEntityField = naming.ToProtoPascalCase(f.Name)
						break
					}
				}
			}
		}

		// Collect fields from the create request message for entity construction.
		// Skip on shape mismatch — the stub doesn't reference these.
		var createFields []CreateFieldData
		if shapeOK && cm.Operation == "create" && svc.Messages != nil {
			if fields, ok := svc.Messages[cm.Method.InputType]; ok {
				for _, f := range fields {
					goType := ProtoTypeToGoType(f.ProtoType)
					// Try to get richer GoType from the entity definition
					for _, ef := range cm.Entity.Fields {
						if ef.Name == f.Name {
							goType = ef.GoType
							break
						}
					}
					kind := DetermineFieldKind(f.ProtoType, goType)
					var enumGoType string
					if kind == FieldKindEnum {
						enumGoType = goType
					}
					createFields = append(createFields, CreateFieldData{
						ProtoGoName:  naming.ToProtoPascalCase(f.Name),
						EntityGoName: naming.ToProtoPascalCase(f.Name),
						Kind:         kind,
						GoType:       goType,
						EnumGoType:   enumGoType,
					})
				}
			}
		}

		methods = append(methods, CRUDMethodTemplateData{
			MethodName:        cm.Method.Name,
			InputType:         cm.Method.InputType,
			OutputType:        cm.Method.OutputType,
			EntityName:        cm.Entity.Name,
			EntityLower:       strings.ToLower(cm.Entity.Name),
			Operation:         cm.Operation,
			AuthRequired:      cm.Method.AuthRequired,
			AuthAction:        authAction,
			PkField:           naming.ToProtoPascalCase(cm.Entity.PkField),
			PkColumnName:      cm.Entity.PkField,
			PkGoType:          cm.Entity.PkGoType,
			HasPkInInput:      cm.Operation == "get" || cm.Operation == "update" || cm.Operation == "delete",
			ResponseField:     cm.Entity.Name,
			HasPagination:     hasPagination,
			PaginationStyle:   paginationStyle,
			HasFilters:        len(filterFields) > 0,
			FilterFields:      filterFields,
			HasOrderBy:        hasOrderBy,
			HasTenant:         cm.Entity.HasTenant,
			TenantGoName:      cm.Entity.TenantGoName,
			TenantColumnName:  cm.Entity.TenantColumnName,
			UpdateEntityField: updateEntityField,
			CreateFields:      createFields,
			ShapeMismatch:     !shapeOK,
			MismatchReason:    shapeReason,
		})
	}

	// Check if any method has pagination, filters, ordering, or a real
	// (non-stub) body. Mismatched stubs contribute nothing to the file's
	// import needs so we don't pull in pkg/crud or pkg/orm for a file
	// that only emits TODO stubs.
	hasPagination := false
	hasFilters := false
	hasOrderBy := false
	needsCRUDLib := false
	for _, m := range methods {
		if m.HasPagination {
			hasPagination = true
		}
		if m.HasFilters {
			hasFilters = true
		}
		if m.HasOrderBy {
			hasOrderBy = true
		}
		if !m.ShapeMismatch {
			needsCRUDLib = true
		}
	}
	hasTenant := false
	for _, m := range methods {
		if m.HasTenant && !m.ShapeMismatch {
			hasTenant = true
			break
		}
	}
	needsORM := hasPagination || hasFilters || hasOrderBy || hasTenant

	return CRUDTemplateData{
		Package:       pkg,
		Module:        modulePath,
		ProtoPackage:  protoPackage,
		DBPackagePath: modulePath + "/internal/db",
		HasPagination: hasPagination,
		HasFilters:    hasFilters,
		HasOrderBy:    hasOrderBy,
		NeedsORM:      needsORM,
		HasTenant:     hasTenant,
		NeedsCRUDLib:  needsCRUDLib,
		CRUDMethods:   methods,
	}
}

// CRUDTestTemplateData holds all data needed to render the CRUD test template.
type CRUDTestTemplateData struct {
	Package      string                   // Go package name, e.g. "patients"
	Module       string                   // Go module path, e.g. "github.com/demo-project"
	ProtoPackage string                   // e.g. "proto/services/patients"
	HasTenant    bool                     // true if any entity has tenant isolation
	Entities     []CRUDTestEntityData     // Grouped per-entity test data
	CRUDMethods  []CRUDMethodTemplateData // All CRUD methods (for individual error tests)
	// TestHelperName mirrors ServiceTemplateData.TestHelperName: the suffix
	// the bootstrap testing generator emits on `app.NewTest<X>` /
	// `app.NewTest<X>Server`. CRUD test scaffolds use this rather than
	// pascal-casing Package so the call site matches the actual factory
	// when an internal package shares the service's leaf name.
	TestHelperName string
}

// CRUDTestEntityData groups CRUD operations by entity for lifecycle tests.
type CRUDTestEntityData struct {
	EntityName        string // "Patient"
	EntityLower       string // "patient"
	PkField           string // "Id"
	PkGoType          string // "int64"
	HasCreate         bool
	HasGet            bool
	HasList           bool
	HasUpdate         bool
	HasDelete         bool
	HasAllCRUD        bool   // true if all 5 operations exist
	HasTenant         bool   // true when the entity has a tenant key field
	TenantGoName      string // e.g., "OrgId"
	TenantColumnName  string // e.g., "org_id"
	CreateMethod      CRUDMethodTemplateData
	GetMethod         CRUDMethodTemplateData
	ListMethod        CRUDMethodTemplateData
	UpdateMethod      CRUDMethodTemplateData
	DeleteMethod      CRUDMethodTemplateData
	Fields            []CRUDTestFieldData // entity proto message fields (minus PK, minus deleted_at)
	CreateFields      []CRUDTestFieldData // fields from the CreateRequest message
	UpdateEntityField string              // Go field name holding entity in UpdateRequest, e.g. "Project"
}

// CRUDTestFieldData holds per-field data for generating test values.
type CRUDTestFieldData struct {
	ProtoName string    // "Name"
	GoType    string    // "string"
	Kind      FieldKind // scalar, enum, message, wrapper, timestamp, etc.
	TestValue string    // `"test-value"` or `1` or `true`
}

// GenerateCRUDTests generates handlers_crud_gen_test.go (unit-test frames,
// no build tag — runs in the default `go test ./...`) and
// handlers_crud_integration_test.go (lifecycle / tenant / pagination /
// filter / NotFound suites guarded by `//go:build integration`) for a
// service with CRUD methods.
//
// cs is the project's checksum tracker. Both scaffold files are recorded
// when actually written; once the user clears every FORGE_SCAFFOLD marker
// the file becomes user-owned and forge stops re-rendering it (and stops
// updating the checksum). A nil cs is tolerated.
func GenerateCRUDTests(svc ServiceDef, crudMethods []CRUDMethod, modulePath string, projectDir string, cs *checksums.FileChecksums) error {
	// Disk-first: same handler-dir + package-clause resolution as
	// GenerateCRUDHandlers (the two MUST land in the same directory and
	// declare the same package).
	res, resErr := ResolveServiceComponent(projectDir, svc.Name)
	if resErr != nil {
		return resErr
	}
	pkg := res.PackageName
	targetDir := res.Dir
	relDir := filepath.Join("handlers", filepath.FromSlash(res.ImportLeaf))

	unitPath := filepath.Join(targetDir, "handlers_crud_gen_test.go")
	integrationPath := filepath.Join(targetDir, "handlers_crud_integration_test.go")

	// Filter out CRUD methods the user has already taken ownership of by
	// writing a real handler — mirrors the dedup GenerateCRUDHandlers
	// applies. Without this filter the test scaffold re-emits a stale
	// handlers_crud_gen_test.go that references AIP-158-shaped request
	// fields (PageSize, Id-int64, …) the user-owned handler no longer
	// accepts, and the test package goes red on the next `go test ./...`.
	existingMethods, err := scanExistingMethods(targetDir, false)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("scan existing methods for %s tests: %w", pkg, err)
	}
	var filteredMethods []CRUDMethod
	for _, cm := range crudMethods {
		if existingMethods[cm.Method.Name] {
			continue
		}
		filteredMethods = append(filteredMethods, cm)
	}

	if len(filteredMethods) == 0 {
		_ = os.Remove(unitPath)
		_ = os.Remove(integrationPath)
		return nil
	}

	// Package + TestHelperName are overridden with the disk-resolved
	// package clause so the emitted test files always match the directory
	// they land in AND call the `app.NewTest<X>` factory the bootstrap
	// testing generator (which uses the same resolver) actually emitted.
	data := buildCRUDTestTemplateData(svc, filteredMethods, modulePath, projectDir)
	data.Package = pkg
	data.TestHelperName = ComputeTestHelperName(pkg, projectDir)

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	unitContent, err := templates.ServiceTemplates().Render("handlers_crud_test_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_test_gen.go.tmpl: %w", err)
	}
	if err := writeScaffoldFile(projectDir, filepath.Join(relDir, "handlers_crud_gen_test.go"), unitContent, cs); err != nil {
		return err
	}

	integrationContent, err := templates.ServiceTemplates().Render("handlers_crud_integration_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_integration_test.go.tmpl: %w", err)
	}
	return writeScaffoldFile(projectDir, filepath.Join(relDir, "handlers_crud_integration_test.go"), integrationContent, cs)
}

// writeScaffoldFile writes a scaffold-with-placeholders file at
// projectDir/relPath. If the file already exists on disk and contains no
// remaining "FORGE_SCAFFOLD:" markers, the user has finished customising
// it and forge will not overwrite it. Otherwise the file is (re)written
// from the template and its checksum recorded so `forge audit` doesn't
// flag it as an orphan.
//
// This is the mechanism that lets `_gen` filenames carry "until-customized"
// semantics: as long as any marker is present the file is forge-owned and
// regenerated; the moment every marker is removed the file becomes user-owned.
//
// The file is tagged Tier-2 in the checksum manifest so the pre-pipeline
// Tier-1 file-stomp guard skips it — Tier-2 means "scaffold once, user
// edits expected", which is exactly the steady state after a user clears
// the FORGE_SCAFFOLD markers. When the user has taken ownership we also
// refresh the recorded checksum to the on-disk content so a future run of
// `forge audit` doesn't flag a tracked-but-modified mismatch (and so any
// legacy Tier-0/Tier-1 entry from before this fix gets re-stamped to
// Tier-2 + forked, matching the user's intent without requiring
// `forge generate --accept`).
func writeScaffoldFile(projectDir, relPath string, content []byte, cs *checksums.FileChecksums) error {
	fullPath := filepath.Join(projectDir, relPath)
	if existing, err := os.ReadFile(fullPath); err == nil {
		if !bytes.Contains(existing, []byte("FORGE_SCAFFOLD:")) {
			// User has cleared every marker — they own the file now.
			// Re-stamp the manifest entry to Tier-2 (forked) so the
			// stomp guard stops flagging the legitimate hand-edit on
			// the next run. Without this re-stamp a pre-existing Tier-1
			// recorded checksum would cause `forge generate` to error
			// out with a "Tier-1 file-stomp" report even though
			// writeScaffoldFile silently transferred ownership.
			if cs != nil {
				if entry, ok := cs.Files[relPath]; ok {
					entry.Hash = checksums.Hash(existing)
					entry.Tier = 2
					entry.Forked = true
					cs.Files[relPath] = entry
				}
			}
			return nil
		}
	}
	if _, err := checksums.WriteGeneratedFileTier2(projectDir, relPath, content, cs, true); err != nil {
		return err
	}
	return nil
}

func buildCRUDTestTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath, projectDir string) CRUDTestTemplateData {
	// Synthesized Package/TestHelperName are placeholders only:
	// GenerateCRUDTests overrides both with disk-resolved values before
	// rendering — see the call site for the rationale.
	pkg := naming.ServicePackage(svc.Name)

	// Build ProtoPackage path (same logic as buildCRUDTemplateData)
	protoPackage := ""
	if svc.ModulePath != "" && svc.GoPackage != "" {
		prefix := svc.ModulePath + "/gen/"
		if strings.HasPrefix(svc.GoPackage, prefix) {
			protoPackage = strings.TrimPrefix(svc.GoPackage, prefix)
			if idx := strings.LastIndex(protoPackage, "/v"); idx >= 0 {
				protoPackage = protoPackage[:idx]
			}
		}
	}

	// Group by entity
	entityMap := make(map[string]*CRUDTestEntityData)
	var entityOrder []string

	// Build all CRUDMethodTemplateData
	var allMethods []CRUDMethodTemplateData

	for _, cm := range crudMethods {
		authAction := operationToAuthAction(cm.Operation)

		// Validate the request/response shape up front. The CRUD-body
		// generator uses the same gate to decide whether to emit a real
		// handler or a tagged CodeUnimplemented stub; the test scaffold
		// has to mirror that decision or it emits per-RPC test rows that
		// dereference fields the request type doesn't have (e.g.
		// `Id: 1` on a GetMarketRequest keyed on `string ticker`).
		shapeOK, shapeReason := validateCRUDShape(svc, cm)

		hasPagination := false
		paginationStyle := ""
		if shapeOK && cm.Operation == "list" && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			hasPagination = true
			paginationStyle = "cursor"
		}

		// Detect filters and ordering from request message fields.
		// Skip when the shape didn't match — the stub branch doesn't
		// dereference filter fields and classifyFilterField on a
		// bespoke request shape would otherwise leak filter rows into
		// per-RPC test setup that fails to compile.
		var filterFields []FilterFieldData
		hasOrderBy := false
		if shapeOK && cm.Operation == "list" && svc.Messages != nil {
			if msgFields, ok := svc.Messages[cm.Method.InputType]; ok {
				for _, mf := range msgFields {
					if classifySkipField(mf.Name) {
						continue
					}
					if mf.Name == "order_by" {
						hasOrderBy = true
						continue
					}
					ff := classifyFilterField(mf)
					filterFields = append(filterFields, ff)
				}
			}
		}

		// Determine update entity field for this method's test data.
		updateEntityField := cm.Entity.Name
		if cm.Operation == "update" && svc.Messages != nil {
			if fields, ok := svc.Messages[cm.Method.InputType]; ok {
				for _, f := range fields {
					if protoTypeMatchesEntity(f.ProtoType, cm.Entity.Name) {
						updateEntityField = naming.ToProtoPascalCase(f.Name)
						break
					}
				}
			}
		}

		mtd := CRUDMethodTemplateData{
			MethodName:        cm.Method.Name,
			InputType:         cm.Method.InputType,
			OutputType:        cm.Method.OutputType,
			EntityName:        cm.Entity.Name,
			EntityLower:       strings.ToLower(cm.Entity.Name),
			Operation:         cm.Operation,
			AuthRequired:      cm.Method.AuthRequired,
			AuthAction:        authAction,
			PkField:           naming.ToProtoPascalCase(cm.Entity.PkField),
			PkColumnName:      cm.Entity.PkField,
			PkGoType:          cm.Entity.PkGoType,
			HasPkInInput:      cm.Operation == "get" || cm.Operation == "update" || cm.Operation == "delete",
			ResponseField:     cm.Entity.Name,
			HasPagination:     hasPagination,
			PaginationStyle:   paginationStyle,
			HasFilters:        len(filterFields) > 0,
			FilterFields:      filterFields,
			HasOrderBy:        hasOrderBy,
			UpdateEntityField: updateEntityField,
			ShapeMismatch:     !shapeOK,
			MismatchReason:    shapeReason,
		}
		allMethods = append(allMethods, mtd)

		ent, ok := entityMap[cm.Entity.Name]
		if !ok {
			// Build proto service message field set for this entity
			protoFieldSet := make(map[string]bool)
			if svc.Messages != nil {
				if msgFields, ok := svc.Messages[cm.Entity.Name]; ok {
					for _, mf := range msgFields {
						protoFieldSet[mf.Name] = true
					}
				}
			}

			// Build entity fields (for update test): fields in both DB entity AND proto service message, minus PK
			var fields []CRUDTestFieldData
			for _, f := range cm.Entity.Fields {
				if f.Name == cm.Entity.PkField {
					continue
				}
				if len(protoFieldSet) > 0 && !protoFieldSet[f.Name] {
					continue
				}
				kind := DetermineFieldKind(f.ProtoType, f.GoType)
				fields = append(fields, CRUDTestFieldData{
					ProtoName: f.GoName,
					GoType:    f.GoType,
					Kind:      kind,
					TestValue: testValueForType(f.GoType),
				})
			}

			// Determine update entity field name from UpdateRequest message
			updateEntityField := cm.Entity.Name
			if svc.Messages != nil {
				updateReqName := "Update" + cm.Entity.Name + "Request"
				if msgFields, ok := svc.Messages[updateReqName]; ok {
					for _, f := range msgFields {
						if protoTypeMatchesEntity(f.ProtoType, cm.Entity.Name) {
							updateEntityField = naming.ToProtoPascalCase(f.Name)
							break
						}
					}
				}
			}

			ent = &CRUDTestEntityData{
				EntityName:        cm.Entity.Name,
				EntityLower:       strings.ToLower(cm.Entity.Name),
				PkField:           naming.ToProtoPascalCase(cm.Entity.PkField),
				PkGoType:          cm.Entity.PkGoType,
				HasTenant:         cm.Entity.HasTenant,
				TenantGoName:      cm.Entity.TenantGoName,
				TenantColumnName:  cm.Entity.TenantColumnName,
				Fields:            fields,
				UpdateEntityField: updateEntityField,
			}
			entityMap[cm.Entity.Name] = ent
			entityOrder = append(entityOrder, cm.Entity.Name)
		}

		// Build CreateFields from the actual create request message
		if cm.Operation == "create" && svc.Messages != nil {
			if msgFields, ok := svc.Messages[cm.Method.InputType]; ok {
				var createFields []CRUDTestFieldData
				for _, f := range msgFields {
					goType := ProtoTypeToGoType(f.ProtoType)
					// Try to get richer GoType from entity definition
					for _, ef := range cm.Entity.Fields {
						if ef.Name == f.Name {
							goType = ef.GoType
							break
						}
					}
					kind := DetermineFieldKind(f.ProtoType, goType)
					createFields = append(createFields, CRUDTestFieldData{
						ProtoName: naming.ToProtoPascalCase(f.Name),
						GoType:    goType,
						Kind:      kind,
						TestValue: testValueForType(goType),
					})
				}
				ent.CreateFields = createFields
			}
		}

		switch cm.Operation {
		case "create":
			ent.HasCreate = true
			ent.CreateMethod = mtd
		case "get":
			ent.HasGet = true
			ent.GetMethod = mtd
		case "list":
			ent.HasList = true
			ent.ListMethod = mtd
		case "update":
			ent.HasUpdate = true
			ent.UpdateMethod = mtd
		case "delete":
			ent.HasDelete = true
			ent.DeleteMethod = mtd
		}
	}

	// Compute HasAllCRUD and build ordered slice
	var entities []CRUDTestEntityData
	for _, name := range entityOrder {
		ent := entityMap[name]
		ent.HasAllCRUD = ent.HasCreate && ent.HasGet && ent.HasList && ent.HasUpdate && ent.HasDelete
		entities = append(entities, *ent)
	}

	testHasTenant := false
	for _, e := range entities {
		if e.HasTenant {
			testHasTenant = true
			break
		}
	}

	return CRUDTestTemplateData{
		Package:        pkg,
		Module:         modulePath,
		ProtoPackage:   protoPackage,
		HasTenant:      testHasTenant,
		Entities:       entities,
		CRUDMethods:    allMethods,
		TestHelperName: ComputeTestHelperName(pkg, projectDir),
	}
}

// testValueForType returns a Go literal suitable for test data based on the Go type.
func testValueForType(goType string) string {
	switch goType {
	case "string":
		return `"test-value"`
	case "int32":
		return "1"
	case "int64":
		return "1"
	case "uint32":
		return "1"
	case "uint64":
		return "1"
	case "float32":
		return "1.0"
	case "float64":
		return "1.0"
	case "bool":
		return "true"
	case "[]byte":
		return `[]byte("test")`
	case "timestamppb.Timestamp", "*timestamppb.Timestamp":
		return "timestamppb.Now()"
	// Wrapper types (google.protobuf.*Value)
	case "*string":
		return `wrapperspb.String("test-value")`
	case "*int32":
		return "wrapperspb.Int32(42)"
	case "*int64":
		return "wrapperspb.Int64(42)"
	case "*uint32":
		return "wrapperspb.UInt32(42)"
	case "*uint64":
		return "wrapperspb.UInt64(42)"
	case "*bool":
		return "wrapperspb.Bool(true)"
	case "*float32":
		return "wrapperspb.Float(1.0)"
	case "*float64":
		return "wrapperspb.Double(1.0)"
	default:
		// Enum types (single-word Go ident like "Status") → use zero value
		// Repeated/map/message types → nil
		if strings.HasPrefix(goType, "[]") || strings.HasPrefix(goType, "map[") || strings.HasPrefix(goType, "*") {
			return "nil"
		}
		// Likely an enum type — use 0
		return "0"
	}
}

// classifySkipField returns true if the field should be skipped during filter classification.
// Pagination fields, ordering companions, and order_by itself are not filters.
func classifySkipField(name string) bool {
	switch name {
	case "page_size", "page_token", "descending", "desc", "sort_order":
		return true
	}
	return false
}

// classifyFilterField builds a FilterFieldData from a proto message field.
func classifyFilterField(mf MessageFieldDef) FilterFieldData {
	goType := ProtoTypeToGoType(mf.ProtoType)

	filterType := "exact"
	switch mf.Name {
	case "search", "query", "q":
		filterType = "search"
	}

	return FilterFieldData{
		ProtoName:  mf.Name,
		GoName:     naming.ToProtoPascalCase(mf.Name),
		ColumnName: mf.Name,
		FieldType:  goType,
		FilterType: filterType,
		IsOptional: mf.IsOptional,
	}
}

// ensureDepsDBField checks the service.go Deps struct for a DB field and adds
// one if missing. The CRUD handlers reference s.deps.DB, so we need it present.
//
// service.go is a Tier-3 user-owned file: forge scaffolds it once at `forge
// add service` time, then never re-renders it. Silently injecting fields on
// every regen broke that contract — a user who hand-wrote a service with
// `List*`-prefixed RPC methods (matched by parseCRUDOperation) but no
// intention of using forge's CRUD codegen would see their service.go grow
// a `DB orm.Context` field and an orm import on the next `forge generate`.
//
// The opt-out: if the user has written a `handlers.go` (the sibling Tier-2
// hand-written-handler file) in the service package, they're signaling that
// they own handler wiring and forge should not touch service.go. The CRUD
// dedup pass in GenerateCRUDHandlers already drops any CRUD method the user
// has implemented in handlers.go; the remaining stubs in handlers_crud_gen.go
// will fail to compile without a DB field, but that failure is loud (a
// `go build` error the user sees directly) rather than a silent mutation of
// their service.go.
//
// A fresh service (no handlers.go on disk) still gets the DB field injected
// automatically — that's the happy path the original code was written for.
func ensureDepsDBField(serviceDir string) error {
	// Opt-out signal: user has written a handlers.go file. They're managing
	// handler wiring (and Deps shape) themselves; don't mutate service.go.
	if _, err := os.Stat(filepath.Join(serviceDir, "handlers.go")); err == nil {
		return nil
	}

	servicePath := filepath.Join(serviceDir, "service.go")
	data, err := os.ReadFile(servicePath)
	if err != nil {
		return err
	}

	content := string(data)

	// If the Deps struct already has a DB field, nothing to do.
	if strings.Contains(content, "DB ") && (strings.Contains(content, "orm.Context") || strings.Contains(content, "orm.Client")) {
		return nil
	}

	// Find the Deps struct and inject the DB field after the opening line.
	marker := "// Add your dependencies here."
	if !strings.Contains(content, marker) {
		// Try to find the Deps struct opening brace
		marker = "type Deps struct {"
		idx := strings.Index(content, marker)
		if idx < 0 {
			return nil // Can't find Deps struct, skip
		}
		// Insert after the opening brace line
		newlineIdx := strings.Index(content[idx:], "\n")
		if newlineIdx < 0 {
			return nil
		}
		insertPos := idx + newlineIdx + 1
		dbField := "\tDB         orm.Context\n"
		content = content[:insertPos] + dbField + content[insertPos:]
	} else {
		// Insert the DB field before the marker comment
		content = strings.Replace(content, marker, "DB         orm.Context\n\t"+marker, 1)
	}

	// Ensure the orm import is present
	if !strings.Contains(content, "\"github.com/reliant-labs/forge/pkg/orm\"") {
		// Find the import block and add the orm import
		importIdx := strings.Index(content, "import (")
		if importIdx >= 0 {
			// Find the closing paren of the import block
			closingIdx := strings.Index(content[importIdx:], ")")
			if closingIdx >= 0 {
				insertPos := importIdx + closingIdx
				content = content[:insertPos] + "\n\t\"github.com/reliant-labs/forge/pkg/orm\"\n" + content[insertPos:]
			}
		}
	}

	return os.WriteFile(servicePath, []byte(content), 0644)
}

// operationToAuthAction maps a CRUD operation to the middleware action constant.
func operationToAuthAction(op string) string {
	switch op {
	case "create":
		return "create"
	case "get":
		return "read"
	case "list":
		return "list"
	case "update":
		return "update"
	case "delete":
		return "delete"
	default:
		return "read"
	}
}

// protoTypeMatchesEntity checks if a proto field type references the given entity.
// Handles both bare types ("Patient") and qualified types ("db.v1.Patient").
func protoTypeMatchesEntity(protoType, entityName string) bool {
	return protoType == entityName || protoType == "db.v1."+entityName
}
