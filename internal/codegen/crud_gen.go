package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinzhu/inflection"
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
	CRUDMethods   []CRUDMethodTemplateData
	Converters    []ConverterTemplateData
}

// CRUDMethodTemplateData holds per-method template data.
type CRUDMethodTemplateData struct {
	MethodName    string // "CreatePatient"
	InputType     string // "CreatePatientRequest"
	OutputType    string // "CreatePatientResponse"
	EntityName    string // "Patient"
	EntityLower   string // "patient"
	Operation     string // "create", "get", "list", "update", "delete"
	AuthRequired  bool
	AuthAction    string // "create", "read", "list", "update", "delete" (middleware constant)
	PkField       string // "Id"
	PkGoType      string // "int64"
	HasPkInInput  bool   // true if the request message likely has an ID field
	ResponseField string // "Patient" — the proto field name in the response that holds the entity
}

// ConverterTemplateData holds per-entity converter function data.
type ConverterTemplateData struct {
	EntityName string        // "Patient"
	FuncPrefix string        // "patient" (lowercase)
	Fields     []ConverterFieldData
}

// ConverterFieldData holds per-field converter data.
type ConverterFieldData struct {
	ProtoName string // "PatientId"
	GoName    string // "PatientId" (entity Go name)
	Skip      bool   // true if types don't match and we should skip
	Comment   string // explanation when skipped
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
func GenerateCRUDHandlers(svc ServiceDef, crudMethods []CRUDMethod, modulePath string, projectDir string) error {
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

	// Build template data
	data := buildCRUDTemplateData(svc, filteredMethods, modulePath)

	content, err := templates.RenderServiceTemplate("service/handlers_crud_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_gen.go.tmpl: %w", err)
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(crudGenPath, content, 0644)
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

	// Collect unique entities for converter generation
	entitySet := make(map[string]EntityDef)
	var methods []CRUDMethodTemplateData
	for _, cm := range crudMethods {
		entitySet[cm.Entity.Name] = cm.Entity

		authAction := operationToAuthAction(cm.Operation)

		methods = append(methods, CRUDMethodTemplateData{
			MethodName:    cm.Method.Name,
			InputType:     cm.Method.InputType,
			OutputType:    cm.Method.OutputType,
			EntityName:    cm.Entity.Name,
			EntityLower:   strings.ToLower(cm.Entity.Name),
			Operation:     cm.Operation,
			AuthRequired:  cm.Method.AuthRequired,
			AuthAction:    authAction,
			PkField:       toGoFieldName(cm.Entity.PkField),
			PkGoType:      cm.Entity.PkGoType,
			HasPkInInput:  cm.Operation == "get" || cm.Operation == "update" || cm.Operation == "delete",
			ResponseField: cm.Entity.Name,
		})
	}

	// Build converter data
	var converters []ConverterTemplateData
	for _, entity := range entitySet {
		var fields []ConverterFieldData
		for _, f := range entity.Fields {
			fields = append(fields, ConverterFieldData{
				ProtoName: f.GoName,
				GoName:    f.GoName,
			})
		}
		converters = append(converters, ConverterTemplateData{
			EntityName: entity.Name,
			FuncPrefix: strings.ToLower(entity.Name[:1]) + entity.Name[1:],
			Fields:     fields,
		})
	}

	return CRUDTemplateData{
		Package:       pkg,
		Module:        modulePath,
		ProtoPackage:  protoPackage,
		DBPackagePath: modulePath + "/gen/db/v1",
		CRUDMethods:   methods,
		Converters:    converters,
	}
}

// CRUDTestTemplateData holds all data needed to render the CRUD test template.
type CRUDTestTemplateData struct {
	Package       string                    // Go package name, e.g. "patients"
	Module        string                    // Go module path, e.g. "github.com/demo-project"
	ProtoPackage  string                    // e.g. "proto/services/patients"
	Entities      []CRUDTestEntityData      // Grouped per-entity test data
	CRUDMethods   []CRUDMethodTemplateData  // All CRUD methods (for individual error tests)
}

// CRUDTestEntityData groups CRUD operations by entity for lifecycle tests.
type CRUDTestEntityData struct {
	EntityName    string // "Patient"
	EntityLower   string // "patient"
	PkField       string // "Id"
	PkGoType      string // "int64"
	HasCreate     bool
	HasGet        bool
	HasList       bool
	HasUpdate     bool
	HasDelete     bool
	HasAllCRUD    bool   // true if all 5 operations exist
	CreateMethod  CRUDMethodTemplateData
	GetMethod     CRUDMethodTemplateData
	ListMethod    CRUDMethodTemplateData
	UpdateMethod  CRUDMethodTemplateData
	DeleteMethod  CRUDMethodTemplateData
	Fields        []CRUDTestFieldData // entity fields for constructing test data
}

// CRUDTestFieldData holds per-field data for generating test values.
type CRUDTestFieldData struct {
	ProtoName string // "Name"
	GoType    string // "string"
	TestValue string // `"test-value"` or `1` or `true`
}

// GenerateCRUDTests generates handlers_crud_test_gen.go for a service with CRUD methods.
func GenerateCRUDTests(svc ServiceDef, crudMethods []CRUDMethod, modulePath string, projectDir string) error {
	pkg := toServicePackage(svc.Name)
	targetDir := filepath.Join(projectDir, "handlers", pkg)

	testGenPath := filepath.Join(targetDir, "handlers_crud_test_gen.go")
	if len(crudMethods) == 0 {
		_ = os.Remove(testGenPath)
		return nil
	}

	data := buildCRUDTestTemplateData(svc, crudMethods, modulePath)

	content, err := templates.RenderServiceTemplate("service/handlers_crud_test_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_test_gen.go.tmpl: %w", err)
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(testGenPath, content, 0644)
}

func buildCRUDTestTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath string) CRUDTestTemplateData {
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
		mtd := CRUDMethodTemplateData{
			MethodName:    cm.Method.Name,
			InputType:     cm.Method.InputType,
			OutputType:    cm.Method.OutputType,
			EntityName:    cm.Entity.Name,
			EntityLower:   strings.ToLower(cm.Entity.Name),
			Operation:     cm.Operation,
			AuthRequired:  cm.Method.AuthRequired,
			AuthAction:    authAction,
			PkField:       toGoFieldName(cm.Entity.PkField),
			PkGoType:      cm.Entity.PkGoType,
			HasPkInInput:  cm.Operation == "get" || cm.Operation == "update" || cm.Operation == "delete",
			ResponseField: cm.Entity.Name,
		}
		allMethods = append(allMethods, mtd)

		ent, ok := entityMap[cm.Entity.Name]
		if !ok {
			// Build test field data
			var fields []CRUDTestFieldData
			for _, f := range cm.Entity.Fields {
				// Skip the PK field in create payloads (usually auto-generated)
				if f.Name == cm.Entity.PkField {
					continue
				}
				fields = append(fields, CRUDTestFieldData{
					ProtoName: f.GoName,
					GoType:    f.GoType,
					TestValue: testValueForType(f.GoType),
				})
			}

			ent = &CRUDTestEntityData{
				EntityName:  cm.Entity.Name,
				EntityLower: strings.ToLower(cm.Entity.Name),
				PkField:     toGoFieldName(cm.Entity.PkField),
				PkGoType:    cm.Entity.PkGoType,
				Fields:      fields,
			}
			entityMap[cm.Entity.Name] = ent
			entityOrder = append(entityOrder, cm.Entity.Name)
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

	return CRUDTestTemplateData{
		Package:      pkg,
		Module:       modulePath,
		ProtoPackage: protoPackage,
		Entities:     entities,
		CRUDMethods:  allMethods,
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
	default:
		return `"test-value"`
	}
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