package codegen

import (
	"crypto/sha1"
	"fmt"
	"sort"
	"strings"

	"github.com/jinzhu/inflection"
)

// MockEntityTemplateData holds data for rendering a single entity's TypeScript
// mock data file (e.g., frontends/<fe>/src/mocks/patients.ts).
type MockEntityTemplateData struct {
	EntityName       string       // "Patient" (PascalCase)
	EntityNamePlural string       // "Patients"
	EntitySlug       string       // "patients" (kebab-case for filename)
	SchemaImport     string       // "PatientSchema"
	TypeImport       string       // "Patient"
	ImportPath       string       // "services/clinic/v1/clinic_pb"
	Fields           []MockField  // fields to populate in mock records
	Records          []MockRecord // 10 mock records
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
	// SchemaImportGroups carries the per-ImportPath aggregation of
	// response-schema imports the mock-transport template needs. The
	// template iterates these to emit ONE merged `import { ... } from
	// "@/gen/<path>"` statement per source module, instead of one per
	// entity. Pre-aggregating in Go (vs. with a groupBy template helper)
	// keeps the template loop trivially auditable and lets us preserve
	// entity ordering inside each group.
	SchemaImportGroups []MockTransportSchemaImportGroup
}

// HasWritableEntities reports whether any entity has a Create or Update
// RPC — gates the MessageInitShape type import in the transport template
// (used only by the mutable-store write paths; an unconditional import
// would trip no-unused-vars on read-only projects).
func (d MockTransportTemplateData) HasWritableEntities() bool {
	for _, e := range d.Entities {
		if e.HasCreate || e.HasUpdate {
			return true
		}
	}
	return false
}

// MockTransportSchemaImportGroup bundles every response-schema symbol the
// mock-transport.ts file imports from a single proto-generated module.
// Two entities whose schemas live in the same `@/gen/services/api/v1/api_pb`
// module merge into one import statement; entities pointing at distinct
// modules each get their own group.
type MockTransportSchemaImportGroup struct {
	ImportPath string   // proto module path, e.g. "services/api/v1/api_pb"
	Symbols    []string // schema symbols imported from this module, dedup'd + sorted
}

// BuildMockTransportSchemaImportGroups groups response-schema imports by
// the entity's proto module path. Each entity contributes the same
// per-RPC schema set the per-entity template loop used to emit
// (`{ListResponseType,GetResponseType,CreateResponseType}Schema` gated
// on `HasList`/`HasGet`/`HasCreate||HasUpdate`). Duplicate symbols
// within a group are collapsed; the order is sorted for deterministic
// output across runs.
func BuildMockTransportSchemaImportGroups(entities []MockTransportEntity) []MockTransportSchemaImportGroup {
	// Preserve first-seen ImportPath order for deterministic ordering of
	// groups across runs (matches the order entities arrive in, which is
	// itself stable per ExtractMockTransportEntities).
	pathOrder := make([]string, 0)
	bySym := make(map[string]map[string]struct{}, 0)
	add := func(path, sym string) {
		if path == "" || sym == "" {
			return
		}
		if _, seen := bySym[path]; !seen {
			pathOrder = append(pathOrder, path)
			bySym[path] = make(map[string]struct{})
		}
		bySym[path][sym] = struct{}{}
	}
	for _, e := range entities {
		if e.HasList {
			add(e.ImportPath, e.ListResponseType+"Schema")
		}
		if e.HasGet {
			add(e.ImportPath, e.GetResponseType+"Schema")
		}
		if e.HasCreate || e.HasUpdate {
			add(e.ImportPath, e.CreateResponseType+"Schema")
			// The mutable store builds new entity records on Create/Update,
			// so it needs the entity's own schema — from the file that
			// declares the entity, which may differ from the service file.
			entityPath := e.EntityImportPath
			if entityPath == "" {
				entityPath = e.ImportPath
			}
			add(entityPath, e.SchemaImport)
		}
	}
	// Alphabetical group order: the emitted import statements must satisfy
	// import/order's alphabetize check, so sort by module path rather than
	// first-seen order.
	sortStrings(pathOrder)
	groups := make([]MockTransportSchemaImportGroup, 0, len(pathOrder))
	for _, path := range pathOrder {
		syms := make([]string, 0, len(bySym[path]))
		for s := range bySym[path] {
			syms = append(syms, s)
		}
		sortStrings(syms)
		groups = append(groups, MockTransportSchemaImportGroup{
			ImportPath: path,
			Symbols:    syms,
		})
	}
	return groups
}

// sortStrings is a tiny stdlib-free sort helper for the schema-symbol
// list. Kept inline so the codegen package doesn't grow a sort import
// just for this single call site (other code paths here already avoid
// sort to keep the dependency surface lean).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// MockTransportEntity represents one entity in the mock transport routing.
type MockTransportEntity struct {
	EntityName       string // "Patient"
	EntityNamePlural string // "Patients"
	EntitySlug       string // "patients" (for mock data import path)
	ServiceName      string // "ClinicService" (short, used in display only)
	// ServiceTypeName is the FULLY-QUALIFIED proto service name, e.g.
	// "demo.v1.ClinicService". Connect v2's runtime
	// `method.parent.typeName` returns this form, so the mock transport
	// must build case keys from it (`${ServiceTypeName}/${RPC}`) or the
	// fall-through dispatch silently never matches.
	ServiceTypeName string // "demo.v1.ClinicService"
	ListRPC         string // "ListPatients"
	GetRPC          string // "GetPatient"
	CreateRPC       string // "CreatePatient"
	UpdateRPC       string // "UpdatePatient"
	DeleteRPC       string // "DeletePatient"
	HasList         bool
	HasGet          bool
	HasCreate       bool
	HasUpdate       bool
	HasDelete       bool
	// ItemsField is the camelCase (protojson) name of the list response's
	// repeated field — the key the mock List handler must set on the
	// ListXxxResponse it builds. It mirrors PageTemplateData.ItemsField:
	// the ACTUAL repeated proto field name (e.g. `keys`), not the
	// camelCased entity plural. The mock store variable keeps the plural
	// camelCase identifier; only the response-message KEY uses this.
	ItemsField string
	// PkFieldCamel is the camelCase name of the entity message's
	// PRIMARY-KEY field ("id", "usageEventId", ...). The mutable session
	// store keys records by this field; hardcoding "id" breaks for entities
	// whose PK column isn't literally `id` (e.g. a `usage_event_id` PK
	// projects to a message with no `id` field, failing `tsc`).
	PkFieldCamel string
	// GetEntityFieldCamel / CreateEntityFieldCamel are the camelCase names
	// of the field that wraps the entity on the Get / Create+Update RESPONSE
	// messages. The proto is free to name this field anything
	// (`GetLLMKeyResponse { LLMKey key = 1; }` → `key`, not `lLMKey`), so the
	// mock dispatch must read it off the response descriptor instead of
	// assuming `camelCase(EntityName)` — a wrong key fails `tsc` with "object
	// literal may only specify known properties".
	GetEntityFieldCamel    string
	CreateEntityFieldCamel string
	ImportPath             string // service proto import path for response-schema imports
	// EntityImportPath is the module declaring the ENTITY message schema
	// ("db/v1/patients_pb"). May differ from ImportPath when the entity
	// lives in its own proto file; the mutable-store Create/Update paths
	// need the entity schema to build new records.
	EntityImportPath string
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

// ScenarioRpcEntry is one unary RPC row in the generated typed scenario
// handler map (src/mocks/scenario-rpcs_gen.ts).
type ScenarioRpcEntry struct {
	Key            string // "demo.v1.TaskService/GetTask" — matches method.parent.typeName dispatch
	RequestType    string // "GetTaskRequest"
	ResponseSchema string // "GetTaskResponseSchema"
}

// ScenarioRpcData drives scenario-rpcs.ts.tmpl: a typed handler map keyed
// by `${serviceTypeName}/${methodName}` whose values take the TYPED request
// and must return a MessageInitShape of the response schema. This is what
// kills the snake_case-payload silent failure: a scenario returning
// `{ user_name: "x" }` for a `userName` field fails tsc instead of
// rendering empty cells.
type ScenarioRpcData struct {
	Entries     []ScenarioRpcEntry
	TypeImports []HookImportGroup // type-only imports, grouped per declaring module
}

// BuildScenarioRpcData collects every unary RPC across all services.
// Streaming RPCs are reachable through the map's string index signature —
// there is no canonical typed return shape for an arbitrary stream.
func BuildScenarioRpcData(services []ServiceDef) ScenarioRpcData {
	var data ScenarioRpcData
	buckets := map[string]map[string]struct{}{}
	add := func(path, sym string) {
		set, ok := buckets[path]
		if !ok {
			set = map[string]struct{}{}
			buckets[path] = set
		}
		set[sym] = struct{}{}
	}

	for _, svc := range services {
		for _, m := range svc.Methods {
			if m.ClientStreaming || m.ServerStreaming {
				continue
			}
			inPath := m.InputProtoFile
			if inPath == "" {
				inPath = svc.ProtoFile
			}
			outPath := m.OutputProtoFile
			if outPath == "" {
				outPath = svc.ProtoFile
			}
			add(ProtoFileToTSImportPath(inPath), m.InputType)
			add(ProtoFileToTSImportPath(outPath), m.OutputType+"Schema")
			data.Entries = append(data.Entries, ScenarioRpcEntry{
				Key:            svc.Package + "." + svc.Name + "/" + m.Name,
				RequestType:    m.InputType,
				ResponseSchema: m.OutputType + "Schema",
			})
		}
	}

	data.TypeImports = flattenImportGroups(buckets)
	return data
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
	// The entity's Schema (PatientSchema, ProductSchema, etc.) lives in the
	// entity's proto file, which may be separate from the service proto
	// (typically `db/v1/*.proto` vs `services/<svc>/v1/*.proto`). Using
	// the service's file silently emits imports for symbols that don't
	// exist in that file — the bundle fails to load at runtime.
	importSource := entity.ProtoFile
	if importSource == "" {
		importSource = svc.ProtoFile
	}
	importPath := ProtoFileToTSImportPath(importSource)

	var fields []MockField
	for _, f := range entity.Fields {
		fields = append(fields, MockField{
			Name:      fieldNameToCamel(f.Name),
			ProtoName: f.Name,
			TSType:    protoTypeToTSType(effectiveMockProtoType(f)),
		})
	}

	// Generate 10 mock records with deterministic values
	records := make([]MockRecord, 10)
	for i := 0; i < 10; i++ {
		var fieldValues []MockFieldValue
		for j, f := range entity.Fields {
			ef := f
			ef.ProtoType = effectiveMockProtoType(f)
			val := mockGenerateValue(entity.TableName, ef, i)
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

// effectiveMockProtoType returns the proto type string the mock generators
// (protoTypeToTSType, mockGenerateValue) should see for a field. Those
// helpers detect repeated scalars via a `repeated `/`[]` prefix on the proto
// type, but EntityField.ProtoType carries only the element kind ("string")
// for repeated scalars — the repeated-ness lives in Kind
// (FieldKindRepeatedScalar) / GoType ("[]string"). Without this, a
// `repeated string models` field mocks a scalar `"sample_models_1"` and
// fails `tsc` against the protobuf-es `string[]` field. Re-encoding the
// prefix here keeps the fix local to mock codegen and leaves the
// ORM/migration consumers of ProtoType untouched.
func effectiveMockProtoType(f EntityField) string {
	if f.Kind == FieldKindRepeatedScalar && !strings.HasPrefix(f.ProtoType, "repeated ") && !strings.HasPrefix(f.ProtoType, "[]") {
		return "repeated " + f.ProtoType
	}
	return f.ProtoType
}

// mockPkFieldCamel returns the camelCase name of the entity's primary-key
// field for use as the mutable-store map key, defaulting to "id" when the
// entity carries no explicit PK.
//
// The store keys MOCK records, which are projections of the entity's WIRE
// message (EntityDef.Fields) — so the chosen field must EXIST on that wire
// message, or `e.<field>` fails `tsc`. The DB PK column ("id") is the natural
// first choice, but it is not always a wire field: a table can carry a
// surrogate `id` PK while the published proto exposes a domain key
// (`usage_event_id`) and omits `id` entirely. Resolution order:
//  1. the DB PK field, if it appears among the wire fields;
//  2. a `<entity_singular>_id` wire field (the conventional domain key);
//  3. the first wire field;
//  4. "id" as a last resort (matches the page generator's fallback).
func mockPkFieldCamel(e EntityDef) string {
	wire := make(map[string]bool, len(e.Fields))
	for _, f := range e.Fields {
		wire[f.Name] = true
	}
	if e.PkField != "" && wire[e.PkField] {
		return fieldNameToCamel(e.PkField)
	}
	if conventional := inflection.Singular(e.TableName) + "_id"; wire[conventional] {
		return fieldNameToCamel(conventional)
	}
	if len(e.Fields) > 0 {
		return fieldNameToCamel(e.Fields[0].Name)
	}
	return "id"
}

// responseEntityField returns the camelCase name of the field on respType
// (a Get/Create/Update response message) that wraps the entity message. The
// proto names this field freely (`GetLLMKeyResponse { LLMKey key = 1; }` →
// `key`), so the mock dispatch reads it off the response descriptor by
// matching the field's message type against the entity, falling back to the
// camelCased entity name for older descriptors / unresolvable cases.
func responseEntityField(svc ServiceDef, respType, entityName string) string {
	if respType != "" && svc.Messages != nil {
		if fields, ok := svc.Messages[respType]; ok {
			for _, f := range fields {
				if fieldMatchesEntity(f, entityName) {
					return fieldNameToCamel(f.Name)
				}
			}
		}
	}
	return fieldNameToCamel(entityName)
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
			entityDef, ok := entityMap[page.EntityName]
			if !ok {
				continue
			}
			entityImportPath := importPath
			if entityDef.ProtoFile != "" {
				entityImportPath = ProtoFileToTSImportPath(entityDef.ProtoFile)
			}
			result = append(result, MockTransportEntity{
				EntityName:             page.EntityName,
				EntityNamePlural:       page.EntityNamePlural,
				EntitySlug:             page.EntitySlug,
				ServiceName:            svc.Name,
				ServiceTypeName:        svc.Package + "." + svc.Name,
				ListRPC:                page.ListRPC,
				GetRPC:                 page.GetRPC,
				CreateRPC:              page.CreateRPC,
				UpdateRPC:              page.UpdateRPC,
				DeleteRPC:              page.DeleteRPC,
				HasList:                page.HasList,
				HasGet:                 page.HasGet,
				HasCreate:              page.HasCreate,
				HasUpdate:              page.HasUpdate,
				HasDelete:              page.HasDelete,
				ItemsField:             page.ItemsField,
				PkFieldCamel:           mockPkFieldCamel(entityDef),
				GetEntityFieldCamel:    responseEntityField(svc, page.GetResponseType, page.EntityName),
				CreateEntityFieldCamel: responseEntityField(svc, page.CreateResponseType, page.EntityName),
				ImportPath:             importPath,
				EntityImportPath:       entityImportPath,
				TypeImport:             page.EntityName,
				SchemaImport:           page.EntityName + "Schema",
				ListResponseType:       page.ListResponseType,
				GetResponseType:        page.GetResponseType,
				CreateRequestType:      page.CreateRequestType,
				CreateResponseType:     page.CreateResponseType,
				UpdateRequestType:      page.UpdateRequestType,
				GetRequestType:         page.GetRequestType,
				DeleteRequestType:      page.DeleteRequestType,
			})
		}
	}
	// Deterministic, alphabetical entity order: the transport template
	// emits one `import * as <x>Mocks from "@/mocks/<slug>"` per entity,
	// and import/order's alphabetize check requires ascending paths.
	sort.Slice(result, func(i, j int) bool { return result[i].EntitySlug < result[j].EntitySlug })
	return result
}

// isBigIntProtoType reports whether protobuf-es v2 emits a TypeScript
// `bigint` (rather than `number`) field for the given proto scalar type.
// The default protobuf-es jstype for 64-bit integer scalars is bigint,
// so int64 / uint64 / sint64 / fixed64 / sfixed64 all need bigint
// literals (`5n`, `BigInt("...")`) in mock data — emitting a plain
// number literal produces `Type 'number' is not assignable to type
// 'bigint'` under `tsc --noEmit`.
//
// Projects can override the jstype to JS_STRING / JS_NORMAL at the
// proto level, but the mock-data generator has no signal for that
// today — when the override is in play the user will hit a separate
// compile error pointing at the mismatch and can hand-patch.
func isBigIntProtoType(protoType string) bool {
	switch protoType {
	case "int64", "uint64", "sint64", "fixed64", "sfixed64":
		return true
	}
	return false
}

// protoTypeToTSType maps proto field types to TypeScript types.
func protoTypeToTSType(protoType string) string {
	// Repeated scalars ("repeated string" from entity descriptors,
	// "[]string" from message descriptors) project to element[] arrays.
	if base, ok := strings.CutPrefix(protoType, "repeated "); ok {
		return protoTypeToTSType(base) + "[]"
	}
	if base, ok := strings.CutPrefix(protoType, "[]"); ok {
		return protoTypeToTSType(base) + "[]"
	}
	switch protoType {
	case "bool":
		return "boolean"
	case "int32", "uint32", "sint32",
		"fixed32", "sfixed32",
		"float", "double":
		return "number"
	case "int64", "uint64", "sint64", "fixed64", "sfixed64":
		// protobuf-es v2 emits bigint for 64-bit integer scalars by
		// default. See isBigIntProtoType for the override caveat.
		return "bigint"
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

	// Repeated scalar fields — emit a small deterministic array of
	// element-typed mocks so the fixture type-checks against the
	// protobuf-es `element[]` field.
	if base, ok := strings.CutPrefix(protoType, "repeated "); ok {
		elem := f
		elem.ProtoType = base
		a := mockGenerateValue(tableName, elem, i)
		b := mockGenerateValue(tableName, elem, i+1)
		return fmt.Sprintf("[%s, %s]", a, b)
	}

	// Primary key. UUIDs are the project default, but if the proto types
	// the id field as a 64-bit integer (some projects use distributed
	// counters / snowflake-style ids) protobuf-es emits it as bigint and
	// a string literal will not type-check. Emit `BigInt("<n>")` so the
	// mock value stays deterministic without depending on parsing
	// hex-of-hash into a numeric literal.
	if col == "id" {
		uuid := mockDeterministicUUID(fmt.Sprintf("%s.%d", tableName, i))
		if isBigIntProtoType(protoType) {
			return fmt.Sprintf("BigInt(%q)", fmt.Sprintf("%d", i+1))
		}
		return fmt.Sprintf("%q", uuid)
	}

	// Foreign key references — same bigint caveat as the primary key.
	if strings.HasSuffix(col, "_id") && col != "id" {
		refTable := strings.TrimSuffix(col, "_id") + "s"
		uuid := mockDeterministicUUID(fmt.Sprintf("%s.%d", refTable, i%10))
		if isBigIntProtoType(protoType) {
			return fmt.Sprintf("BigInt(%q)", fmt.Sprintf("%d", (i%10)+1))
		}
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

	// Numeric types. 64-bit integer scalars project to TypeScript
	// `bigint` under protobuf-es v2's defaults — emit a `<n>n` bigint
	// literal rather than a plain number to keep `tsc --noEmit` clean.
	switch protoType {
	case "int32", "uint32", "sint32",
		"fixed32", "sfixed32":
		return mockGenerateIntegerValue(col, i)
	case "int64", "uint64", "sint64", "fixed64", "sfixed64":
		return mockGenerateIntegerValue(col, i) + "n"
	case "float", "double":
		// Plausibility: probability/rate/ratio-shaped fields stay in
		// [0, 1); percent-shaped fields stay in [0, 100]. Unbounded values
		// like 73.5 in a "win_probability" column teach downstream LLMs
		// (and humans skimming fixtures) the wrong data shape.
		lower := strings.ToLower(col)
		switch {
		case strings.Contains(lower, "probability") || strings.Contains(lower, "ratio") ||
			strings.HasSuffix(lower, "_rate") || lower == "rate" || strings.Contains(lower, "confidence") ||
			strings.Contains(lower, "score") && strings.Contains(lower, "normalized"):
			return fmt.Sprintf("%.2f", float64((i%10))*0.1+0.05)
		case strings.Contains(lower, "percent") || strings.HasSuffix(lower, "_pct"):
			return fmt.Sprintf("%.1f", float64((i%20))*5.0)
		}
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
