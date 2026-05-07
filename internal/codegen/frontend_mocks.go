package codegen

import (
	"crypto/sha1"
	"fmt"
	"strings"

	"github.com/jinzhu/inflection"
)

// MockEntityTemplateData holds data for rendering a single entity's TypeScript
// mock data file (e.g., frontends/<fe>/src/mocks/patients.ts).
type MockEntityTemplateData struct {
	EntityName       string          // "Patient" (PascalCase)
	EntityNamePlural string          // "Patients"
	EntitySlug       string          // "patients" (kebab-case for filename)
	SchemaImport     string          // "PatientSchema"
	TypeImport       string          // "Patient"
	ImportPath       string          // "services/clinic/v1/clinic_pb"
	Fields           []MockField     // fields to populate in mock records
	Records          []MockRecord    // 10 mock records
}

// MockField describes a single field in the proto message for mock data.
type MockField struct {
	Name      string // camelCase TS field name: "orgId"
	ProtoName string // snake_case proto field name: "org_id"
	TSType    string // "string", "number", "boolean"
}

// MockRecord is a single mock object with field values.
type MockRecord struct {
	Fields []MockFieldValue
}

// MockFieldValue is a field name + its literal TypeScript value.
type MockFieldValue struct {
	Name  string // camelCase field name
	Value string // TypeScript literal: `"abc"`, `42`, `true`
	Last  bool   // true if this is the last field (for comma handling in templates)
}

// MockTransportTemplateData holds data for rendering the mock-transport.ts file.
type MockTransportTemplateData struct {
	Entities []MockTransportEntity
}

// MockTransportEntity represents one entity in the mock transport routing.
type MockTransportEntity struct {
	EntityName       string // "Patient"
	EntityNamePlural string // "Patients"
	EntitySlug       string // "patients" (for mock data import path)
	ServiceName      string // "ClinicService"
	ListRPC          string // "ListPatients"
	GetRPC           string // "GetPatient"
	CreateRPC        string // "CreatePatient"
	UpdateRPC        string // "UpdatePatient"
	DeleteRPC        string // "DeletePatient"
	HasList          bool
	HasGet           bool
	HasCreate        bool
	HasUpdate        bool
	HasDelete        bool
	ImportPath       string // proto import path for type imports
	TypeImport       string // "Patient"
	SchemaImport     string // "PatientSchema"
	// Response/request type names
	ListResponseType   string
	GetResponseType    string
	CreateRequestType  string
	CreateResponseType string
	UpdateRequestType  string
	GetRequestType     string
	DeleteRequestType  string
}

// mockSeedNamespace matches the namespace used in seed_gen.go for deterministic UUIDs.
var mockSeedNamespace = [16]byte{
	0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// mockDeterministicUUID generates a UUID v5-style deterministic UUID from a name.
// This mirrors the seed_gen.go deterministicUUID function to produce identical values.
func mockDeterministicUUID(name string) string {
	h := sha1.New()
	h.Write(mockSeedNamespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// EntityDefToMockData converts an EntityDef (parsed from proto) and its associated
// ServiceDef into MockEntityTemplateData for template rendering. It generates
// the same deterministic mock values as seed_gen.go.
func EntityDefToMockData(entity EntityDef, svc ServiceDef) MockEntityTemplateData {
	plural := inflection.Plural(entity.Name)
	importPath := ProtoFileToTSImportPath(svc.ProtoFile)

	var fields []MockField
	for _, f := range entity.Fields {
		fields = append(fields, MockField{
			Name:      fieldNameToCamel(f.Name),
			ProtoName: f.Name,
			TSType:    protoTypeToTSType(f.ProtoType),
		})
	}

	// Generate 10 mock records with deterministic values
	records := make([]MockRecord, 10)
	for i := 0; i < 10; i++ {
		var fieldValues []MockFieldValue
		for j, f := range entity.Fields {
			val := mockGenerateValue(entity.TableName, f, i)
			fieldValues = append(fieldValues, MockFieldValue{
				Name:  fieldNameToCamel(f.Name),
				Value: val,
				Last:  j == len(entity.Fields)-1,
			})
		}
		records[i] = MockRecord{Fields: fieldValues}
	}

	return MockEntityTemplateData{
		EntityName:       entity.Name,
		EntityNamePlural: plural,
		EntitySlug:       PascalToKebab(plural),
		SchemaImport:     entity.Name + "Schema",
		TypeImport:       entity.Name,
		ImportPath:       importPath,
		Fields:           fields,
		Records:          records,
	}
}

// ExtractMockTransportEntities builds MockTransportEntity data from services
// and entity definitions. It pairs CRUD page data with entity info.
func ExtractMockTransportEntities(services []ServiceDef, entities []EntityDef) []MockTransportEntity {
	// Build a lookup from entity name to EntityDef
	entityMap := make(map[string]EntityDef, len(entities))
	for _, e := range entities {
		entityMap[e.Name] = e
	}

	var result []MockTransportEntity
	for _, svc := range services {
		pages := ExtractCRUDEntities(svc)
		importPath := ProtoFileToTSImportPath(svc.ProtoFile)

		for _, page := range pages {
			// Only include entities that have corresponding entity definitions
			// (i.e., actual database entities with mock data). Non-CRUD RPCs
			// like GetStatus match CRUD patterns but don't have entities.
			if _, ok := entityMap[page.EntityName]; !ok {
				continue
			}
			result = append(result, MockTransportEntity{
				EntityName:         page.EntityName,
				EntityNamePlural:   page.EntityNamePlural,
				EntitySlug:         page.EntitySlug,
				ServiceName:        svc.Name,
				ListRPC:            page.ListRPC,
				GetRPC:             page.GetRPC,
				CreateRPC:          page.CreateRPC,
				UpdateRPC:          page.UpdateRPC,
				DeleteRPC:          page.DeleteRPC,
				HasList:            page.HasList,
				HasGet:             page.HasGet,
				HasCreate:          page.HasCreate,
				HasUpdate:          page.HasUpdate,
				HasDelete:          page.HasDelete,
				ImportPath:         importPath,
				TypeImport:         page.EntityName,
				SchemaImport:       page.EntityName + "Schema",
				ListResponseType:   page.ListResponseType,
				GetResponseType:    page.GetResponseType,
				CreateRequestType:  page.CreateRequestType,
				CreateResponseType: page.CreateResponseType,
				UpdateRequestType:  page.UpdateRequestType,
				GetRequestType:     page.GetRequestType,
				DeleteRequestType:  page.DeleteRequestType,
			})
		}
	}
	return result
}

// protoTypeToTSType maps proto field types to TypeScript types.
func protoTypeToTSType(protoType string) string {
	switch protoType {
	case "bool":
		return "boolean"
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64",
		"float", "double":
		return "number"
	case "google.protobuf.Timestamp":
		return "string"
	case "enum":
		return "number"
	case "message":
		return "object"
	default:
		return "string"
	}
}

// mockGenerateValue produces a TypeScript literal value for the given entity field
// and row index. The values are deterministic and match seed_gen.go output.
func mockGenerateValue(tableName string, f EntityField, i int) string {
	col := f.Name
	protoType := f.ProtoType

	// Primary key
	if col == "id" {
		uuid := mockDeterministicUUID(fmt.Sprintf("%s.%d", tableName, i))
		return fmt.Sprintf("%q", uuid)
	}

	// Foreign key references
	if strings.HasSuffix(col, "_id") && col != "id" {
		refTable := strings.TrimSuffix(col, "_id") + "s"
		uuid := mockDeterministicUUID(fmt.Sprintf("%s.%d", refTable, i%10))
		return fmt.Sprintf("%q", uuid)
	}

	// Timestamp fields
	if col == "created_at" || col == "updated_at" || col == "deleted_at" ||
		strings.HasSuffix(col, "_at") {
		return mockGenerateTimestamp(col, i)
	}

	// Boolean
	if protoType == "bool" {
		if i%2 == 0 {
			return "true"
		}
		return "false"
	}

	// Numeric types
	switch protoType {
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64":
		return mockGenerateIntegerValue(col, i)
	case "float", "double":
		return fmt.Sprintf("%.2f", float64(i+1)*10.5)
	}

	// Enum fields — use value 1 (first non-UNSPECIFIED value) to avoid overflow
	// since some enums have fewer than 5 values.
	if protoType == "enum" {
		return "1"
	}

	// Message fields — use empty object
	if protoType == "message" {
		return "{}"
	}

	// String fields
	return fmt.Sprintf("%q", mockGenerateStringValue(col, i))
}

func mockGenerateTimestamp(col string, i int) string {
	day := (i % 28) + 1
	var hour int
	switch col {
	case "updated_at":
		hour = 12
	default:
		hour = 8
	}
	// protobuf-es v2 expects Timestamp objects, not ISO strings.
	// Use timestampFromDate() from @bufbuild/protobuf/wkt.
	return fmt.Sprintf(`timestampFromDate(new Date("2024-01-%.2dT%.2d:00:00Z"))`, day, hour)
}

func mockGenerateIntegerValue(col string, i int) string {
	switch {
	case col == "age":
		return fmt.Sprintf("%d", 20+(i%50))
	case col == "quantity" || col == "count":
		return fmt.Sprintf("%d", (i+1)*5)
	case col == "price" || col == "amount" || strings.HasSuffix(col, "_cents"):
		return fmt.Sprintf("%d", (i+1)*1000)
	case col == "sort_order" || col == "position" || col == "priority":
		return fmt.Sprintf("%d", i+1)
	default:
		return fmt.Sprintf("%d", i+1)
	}
}

// mockGenerateStringValue produces the same string values as seed_gen.go.
func mockGenerateStringValue(col string, i int) string {
	switch {
	case col == "name":
		return mockSampleNames[i%len(mockSampleNames)]
	case col == "first_name":
		return mockSampleFirstNames[i%len(mockSampleFirstNames)]
	case col == "last_name":
		return mockSampleLastNames[i%len(mockSampleLastNames)]
	case col == "email":
		return fmt.Sprintf("user%d@example.com", i+1)
	case col == "phone" || col == "phone_number":
		return fmt.Sprintf("+1555%07d", i+1)
	case col == "title":
		return mockSampleTitles[i%len(mockSampleTitles)]
	case col == "description":
		return mockSampleDescriptions[i%len(mockSampleDescriptions)]
	case col == "url" || col == "website" || col == "homepage":
		return fmt.Sprintf("https://example.com/%s/%d", col, i+1)
	case col == "status":
		return mockSampleStatuses[i%len(mockSampleStatuses)]
	case col == "role":
		return mockSampleRoles[i%len(mockSampleRoles)]
	case col == "type" || col == "kind" || col == "category":
		return mockSampleTypes[i%len(mockSampleTypes)]
	case col == "slug":
		return fmt.Sprintf("item-%d", i+1)
	case col == "username":
		return fmt.Sprintf("user_%d", i+1)
	default:
		return fmt.Sprintf("sample_%s_%d", col, i+1)
	}
}

// Sample data arrays — mirrors seed_gen.go for deterministic parity.
var (
	mockSampleNames = []string{
		"Acme Corp", "Globex Industries", "Initech Solutions",
		"Umbrella Holdings", "Soylent Inc", "Stark Enterprises",
		"Wayne Industries", "Oscorp Technologies", "Hooli Systems",
		"Pied Piper",
	}
	mockSampleFirstNames = []string{
		"Alice", "Bob", "Charlie", "Diana", "Edward",
		"Fiona", "George", "Hannah", "Ivan", "Julia",
	}
	mockSampleLastNames = []string{
		"Anderson", "Baker", "Chen", "Davis", "Evans",
		"Foster", "Garcia", "Harris", "Ibrahim", "Johnson",
	}
	mockSampleTitles = []string{
		"Getting Started Guide", "API Integration Manual",
		"Security Best Practices", "Performance Tuning",
		"Architecture Overview", "Deployment Playbook",
		"Data Migration Handbook", "Monitoring Setup",
		"Incident Response Plan", "Onboarding Checklist",
	}
	mockSampleDescriptions = []string{
		"Comprehensive guide for new team members and initial setup.",
		"Step-by-step instructions for integrating with external APIs.",
		"Best practices for securing production environments.",
		"Techniques for optimizing database queries and response times.",
		"High-level overview of the system architecture and design decisions.",
		"Detailed procedures for deploying services to production.",
		"Instructions for migrating data between schema versions.",
		"How to set up monitoring, alerting, and dashboards.",
		"Procedures for identifying, triaging, and resolving incidents.",
		"Checklist for onboarding new services and dependencies.",
	}
	mockSampleStatuses = []string{
		"active", "pending", "inactive", "archived", "suspended",
	}
	mockSampleRoles = []string{
		"admin", "member", "viewer", "editor", "owner",
	}
	mockSampleTypes = []string{
		"standard", "premium", "enterprise", "trial", "free",
	}
)