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
	Package       string       // Go package name, e.g. "patients"
	Module        string       // Go module path, e.g. "github.com/demo-project"
	ProtoPackage  string       // e.g. "proto/services/patients"
	DBPackagePath string       // e.g. "github.com/demo-project/gen/db/v1"
	HasPagination bool         // true if any list method uses pagination
	HasFilters    bool         // true if any list method has filter fields
	HasOrderBy    bool         // true if any list method has order_by
	NeedsORM      bool         // true if pagination, filters, or ordering requires orm import
	HasTenant     bool         // true if any CRUD method operates on a tenant-scoped entity
	CRUDMethods   []CRUDMethodTemplateData
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
	HasOrderBy        bool   // true if list method has order_by field
	HasTenant         bool   // true when the entity has a tenant key field
	TenantGoName      string // e.g., "OrgId", "TenantId" (PascalCase Go field name on entity)
	TenantColumnName  string // e.g., "org_id", "tenant_id"
	UpdateEntityField string // e.g., "Project" — Go field name in the update request that holds the entity
	CreateFields      []CreateFieldData // fields from the create request message
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
	pkg := toServicePackage(svc.Name)
	targetDir := filepath.Join(projectDir, "handlers", pkg)

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

	// Build template data
	data := buildCRUDTemplateData(svc, filteredMethods, modulePath)

	content, err := templates.ServiceTemplates().Render("handlers_crud_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_gen.go.tmpl: %w", err)
	}

	relPath := filepath.Join("handlers", pkg, "handlers_crud_gen.go")
	if _, err := checksums.WriteGeneratedFile(projectDir, relPath, content, cs, true); err != nil {
		return fmt.Errorf("write handlers_crud_gen.go: %w", err)
	}
	return nil
}

func buildCRUDTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath string) CRUDTemplateData {
	pkg := toServicePackage(svc.Name)

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

		// Detect pagination for list operations: check if the input type
		// follows AIP-158 naming (List<Entity>Request implies page_size).
		hasPagination := false
		paginationStyle := ""
		if cm.Operation == "list" && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			hasPagination = true
			paginationStyle = "cursor"
		}

		// Detect filters and ordering from request message fields.
		var filterFields []FilterFieldData
		hasOrderBy := false
		if cm.Operation == "list" && svc.Messages != nil {
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
		var createFields []CreateFieldData
		if cm.Operation == "create" && svc.Messages != nil {
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
		})
	}

	// Check if any method has pagination, filters, or ordering
	hasPagination := false
	hasFilters := false
	hasOrderBy := false
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
	}
	hasTenant := false
	for _, m := range methods {
		if m.HasTenant {
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
		CRUDMethods:   methods,
	}
}

// CRUDTestTemplateData holds all data needed to render the CRUD test template.
type CRUDTestTemplateData struct {
	Package       string                    // Go package name, e.g. "patients"
	Module        string                    // Go module path, e.g. "github.com/demo-project"
	ProtoPackage  string                    // e.g. "proto/services/patients"
	HasTenant     bool                      // true if any entity has tenant isolation
	Entities      []CRUDTestEntityData      // Grouped per-entity test data
	CRUDMethods   []CRUDMethodTemplateData  // All CRUD methods (for individual error tests)
	// TestHelperName mirrors ServiceTemplateData.TestHelperName: the suffix
	// the bootstrap testing generator emits on `app.NewTest<X>` /
	// `app.NewTest<X>Server`. CRUD test scaffolds use this rather than
	// pascal-casing Package so the call site matches the actual factory
	// when an internal package shares the service's leaf name.
	TestHelperName string
}

// CRUDTestEntityData groups CRUD operations by entity for lifecycle tests.
type CRUDTestEntityData struct {
	EntityName       string // "Patient"
	EntityLower      string // "patient"
	PkField          string // "Id"
	PkGoType         string // "int64"
	HasCreate        bool
	HasGet           bool
	HasList          bool
	HasUpdate        bool
	HasDelete        bool
	HasAllCRUD       bool   // true if all 5 operations exist
	HasTenant        bool   // true when the entity has a tenant key field
	TenantGoName     string // e.g., "OrgId"
	TenantColumnName string // e.g., "org_id"
	CreateMethod     CRUDMethodTemplateData
	GetMethod        CRUDMethodTemplateData
	ListMethod       CRUDMethodTemplateData
	UpdateMethod     CRUDMethodTemplateData
	DeleteMethod     CRUDMethodTemplateData
	Fields           []CRUDTestFieldData // entity proto message fields (minus PK, minus deleted_at)
	CreateFields     []CRUDTestFieldData // fields from the CreateRequest message
	UpdateEntityField string             // Go field name holding entity in UpdateRequest, e.g. "Project"
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
	pkg := toServicePackage(svc.Name)
	targetDir := filepath.Join(projectDir, "handlers", pkg)

	unitPath := filepath.Join(targetDir, "handlers_crud_gen_test.go")
	integrationPath := filepath.Join(targetDir, "handlers_crud_integration_test.go")
	if len(crudMethods) == 0 {
		_ = os.Remove(unitPath)
		_ = os.Remove(integrationPath)
		return nil
	}

	data := buildCRUDTestTemplateData(svc, crudMethods, modulePath, projectDir)

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	unitContent, err := templates.ServiceTemplates().Render("handlers_crud_test_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_test_gen.go.tmpl: %w", err)
	}
	if err := writeScaffoldFile(projectDir, filepath.Join("handlers", pkg, "handlers_crud_gen_test.go"), unitContent, cs); err != nil {
		return err
	}

	integrationContent, err := templates.ServiceTemplates().Render("handlers_crud_integration_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_integration_test.go.tmpl: %w", err)
	}
	return writeScaffoldFile(projectDir, filepath.Join("handlers", pkg, "handlers_crud_integration_test.go"), integrationContent, cs)
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
func writeScaffoldFile(projectDir, relPath string, content []byte, cs *checksums.FileChecksums) error {
	fullPath := filepath.Join(projectDir, relPath)
	if existing, err := os.ReadFile(fullPath); err == nil {
		if !bytes.Contains(existing, []byte("FORGE_SCAFFOLD:")) {
			// User has cleared every marker — they own the file now.
			return nil
		}
	}
	if _, err := checksums.WriteGeneratedFile(projectDir, relPath, content, cs, true); err != nil {
		return err
	}
	return nil
}

func buildCRUDTestTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath, projectDir string) CRUDTestTemplateData {
	pkg := toServicePackage(svc.Name)

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

		hasPagination := false
		paginationStyle := ""
		if cm.Operation == "list" && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			hasPagination = true
			paginationStyle = "cursor"
		}

		// Detect filters and ordering from request message fields.
		var filterFields []FilterFieldData
		hasOrderBy := false
		if cm.Operation == "list" && svc.Messages != nil {
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
func ensureDepsDBField(serviceDir string) error {
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