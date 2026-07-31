package codegen

import (
	"crypto/sha1"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jinzhu/inflection"

	"github.com/reliant-labs/forge/internal/schemadef"
	"github.com/reliant-labs/forge/internal/seeddata"
	"github.com/reliant-labs/forge/internal/shadowdb"
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
	Records          []MockRecord // 10 mock records
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

// ScenarioRPCEntry is one unary RPC row in the generated typed scenario
// handler map (src/mocks/scenario-rpcs_gen.ts).
type ScenarioRPCEntry struct {
	Key            string // "demo.v1.TaskService/GetTask" — matches method.parent.typeName dispatch
	RequestType    string // "GetTaskRequest"
	ResponseSchema string // "GetTaskResponseSchema"
}

// ScenarioRPCData drives scenario-rpcs.ts.tmpl: a typed handler map keyed
// by `${serviceTypeName}/${methodName}` whose values take the TYPED request
// and must return a MessageInitShape of the response schema. This is what
// kills the snake_case-payload silent failure: a scenario returning
// `{ user_name: "x" }` for a `userName` field fails tsc instead of
// rendering empty cells.
type ScenarioRPCData struct {
	Entries     []ScenarioRPCEntry
	TypeImports []HookImportGroup // type-only imports, grouped per declaring module
}

// BuildScenarioRPCData collects every unary RPC across all services.
// Streaming RPCs are reachable through the map's string index signature —
// there is no canonical typed return shape for an arbitrary stream.
func BuildScenarioRPCData(services []ServiceDef) ScenarioRPCData {
	var data ScenarioRPCData
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
			data.Entries = append(data.Entries, ScenarioRPCEntry{
				Key:            svc.Package + "." + svc.Name + "/" + m.Name,
				RequestType:    m.InputType,
				ResponseSchema: m.OutputType + "Schema",
			})
		}
	}

	data.TypeImports = flattenImportGroups(buckets)
	return data
}

// ──────────────────────────────────────────────────────────────────────
// Where the frontend's demo data comes from
// ──────────────────────────────────────────────────────────────────────
//
// From the same place the database's does.
//
// This generator used to invent its own: a column called `name` drew from a
// company pool, `status` from {active, pending, inactive, ...}, `title` from a
// documentation-chapter list, a float called `win_probability` from [0,1). The
// seeder ran the same trick until commit 5f8993ab deleted it — what a column
// MEANS is a decision the schema does not carry, so forge cannot derive it —
// leaving one application with TWO demo vocabularies: the database said
// `sample_name_1` where the frontend said "Acme Corp".
//
// Worse than inconsistent, the invented values were not even legal. The mocks
// read no constraints at all, so a `sku` column whose CHECK is
// `^[A-Z]{3}-[0-9]{4}$` mocked as `"sample_sku_3"` — data the very API the
// mock stands in for would reject.
//
// So the mock generator makes no decisions about domain vocabulary. It reads
// the values the project's own seed plan will write, per (table, column, row),
// and renders them as TypeScript. Everything the seeder derives from a
// DECLARATION — db/seeds/vocab.yaml, a CHECK vocabulary, a regex CHECK, length
// and range bounds, the foreign keys, the primary keys — arrives here already
// resolved, and the two can no longer disagree because there is only one
// answer. What the seeder refuses to invent, this refuses to invent too: an
// undeclared column carries seeddata.SyntheticStringPrefix + its own name +
// the row number, in both places.

// SeedProjection is the project's dev dataset as the frontend mock generator
// reads it: the same seeddata.Plan `forge db seed apply` renders, queried
// per cell instead of rendered as SQL.
//
// A nil *SeedProjection is valid everywhere and means "no dataset to agree
// with" — no migrations, no reachable shadow server, or a schema the planner
// refuses (a NOT NULL foreign-key cycle). Every value then falls back to a
// type-correct, self-evidently synthetic literal, which is exactly what the
// seeder would have written for a column nothing describes.
type SeedProjection struct {
	cfg  seeddata.Config
	plan *seeddata.Plan
}

// BuildSeedProjection resolves the project's seed plan from its migrations.
// cfg is the project's own seed configuration (forge.yaml database.seed) —
// the salt and row counts the dev dataset is built with — passed in rather
// than re-derived here so the mocks and `forge db seed apply` cannot be
// looking at different plans.
func BuildSeedProjection(projectDir string, cfg seeddata.Config) *SeedProjection {
	migDir := filepath.Join(projectDir, "db", "migrations")
	tables, err := schemadef.ApplyAndIntrospectAt(migDir, shadowdb.Resolve(projectDir))
	if err != nil || len(tables) == 0 {
		return nil
	}
	// Vocab problems are reported by the seed CLI, which is where a project
	// asks for its dataset; a bad overlay here just means built-ins.
	vocab, _ := seeddata.LoadVocab(seeddata.VocabPath(migDir))
	return newSeedProjection(tables, cfg, vocab)
}

// newSeedProjection builds the projection from an already-introspected
// schema. Split out so the agreement between a mock literal and the seeded
// cell can be asserted without a database.
func newSeedProjection(tables []schemadef.Table, cfg seeddata.Config, vocab *seeddata.Vocab) *SeedProjection {
	plan, err := seeddata.BuildPlan(tables, seeddata.PoolsFromTables(tables), cfg)
	if err != nil {
		return nil
	}
	plan.SetBounds(seeddata.BoundsFromTables(tables))
	plan.ApplyVocab(vocab)
	return &SeedProjection{cfg: cfg, plan: plan}
}

// Value returns the value the seeded database holds at (table, column, row),
// raw and unquoted. ok is false for a cell with no plain scalar spelling —
// NULL, an array, a boolean — and for every cell when there is no plan.
func (p *SeedProjection) Value(table, column string, row int) (string, bool) {
	if p == nil || p.plan == nil {
		return "", false
	}
	return p.plan.SeedValue(table, column, row)
}

// Rows is how many rows the dataset holds for a table. 0 means "no plan", and
// the caller keeps its own default.
func (p *SeedProjection) Rows(table string) int {
	if p == nil || p.plan == nil {
		return 0
	}
	return p.cfg.EffectiveRows(table)
}

// mockSeedNamespace is a fixed UUID namespace. Arbitrary UUIDv4 chosen once:
// changing it changes every generated id.
var mockSeedNamespace = [16]byte{
	0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// mockDeterministicUUID generates a UUID v5-style deterministic UUID from a
// name. It is reached only when there is no seed plan — with one, a primary
// key's mock value is the primary key the dataset actually holds.
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

// mockRecordCount is how many rows of fixtures an entity gets when the
// dataset has at least that many — enough to fill a list page. A dataset
// configured with fewer rows than this caps it: the fixtures are the first N
// rows of the dev database, and padding past its end with invented values
// would put the two back into disagreement on exactly the rows nobody looked
// at.
const mockRecordCount = 10

// EntityDefToMockData converts an EntityDef (parsed from proto) and its
// associated ServiceDef into MockEntityTemplateData for template rendering.
// seed is the project's dev dataset (nil when there is none) — see
// SeedProjection: every value below is the one the database holds for the
// same row.
func EntityDefToMockData(entity EntityDef, svc ServiceDef, seed *SeedProjection) MockEntityTemplateData {
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

	rowCount := mockRecordCount
	if n := seed.Rows(entity.TableName); n > 0 && n < rowCount {
		rowCount = n
	}
	records := make([]MockRecord, rowCount)
	for i := 0; i < rowCount; i++ {
		var fieldValues []MockFieldValue
		for j, f := range entity.Fields {
			ef := f
			ef.ProtoType = effectiveMockProtoType(f)
			val := mockGenerateValue(seed, entity.TableName, ef, i, svc)
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
	if alreadyRepeated(f.ProtoType) {
		return f.ProtoType
	}
	// Both repeated scalars AND repeated messages carry only the element kind
	// in ProtoType (a repeated `string` field is ProtoType "string"; a repeated
	// message is ProtoType "message") — the repeated-ness lives in
	// Kind/GoType. Without re-encoding the prefix, a repeated-message field
	// would mock `{}` (an object) against the protobuf-es `Foo[]` array type and
	// fail `tsc`, exactly as a repeated scalar mocked a bare scalar.
	if isRepeatedEntityField(f) {
		return "repeated " + f.ProtoType
	}
	return f.ProtoType
}

// isRepeatedEntityField reports whether an entity wire field is `repeated`.
//
// There is no single flag to read: schemaFieldToEntityField encodes the
// descriptor's repeated bit differently per kind — in Kind for scalars and
// messages, and in the GoType "[]" prefix for enums, whose Kind stays
// FieldKindEnum because the conversion generator branches on it.
//
// `bytes` is the trap in the prefix test and the reason this is not one
// strings.HasPrefix: its SINGULAR Go type is already []byte, so for that
// one kind the prefix means nothing.
func isRepeatedEntityField(f EntityField) bool {
	switch f.Kind {
	case FieldKindRepeatedScalar, FieldKindRepeatedMessage:
		return true
	case FieldKindEnum:
		return strings.HasPrefix(f.GoType, "[]")
	}
	return false
}

// alreadyRepeated reports whether a proto-type string already carries a
// repeated/array marker (so re-encoding the prefix would double it).
func alreadyRepeated(protoType string) bool {
	return strings.HasPrefix(protoType, "repeated ") || strings.HasPrefix(protoType, "[]")
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

// protoTypeToTSType maps proto field types to TypeScript types. Scalars
// come from the one projection (protoScalarTS); the remaining arms cover
// the structured kinds a descriptor can carry.
func protoTypeToTSType(protoType string) string {
	// Repeated scalars ("repeated string" from entity descriptors,
	// "[]string" from message descriptors) project to element[] arrays.
	if base, ok := strings.CutPrefix(protoType, "repeated "); ok {
		return protoTypeToTSType(base) + "[]"
	}
	if base, ok := strings.CutPrefix(protoType, "[]"); ok {
		return protoTypeToTSType(base) + "[]"
	}
	if ts, ok := protoScalarTSType(protoType); ok {
		return ts
	}
	switch protoType {
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

// mockGenerateValue produces a TypeScript literal value for the given entity
// field and row index.
//
// The value is the one the project's dev dataset holds at that cell whenever
// the dataset can answer — vocabulary, CHECK-constrained values, keys and
// their references all arrive already resolved, so the mocks and the database
// cannot disagree. Everything below the projection is the no-dataset
// fallback: type-correct, deterministic, and self-evidently invented.
func mockGenerateValue(seed *SeedProjection, tableName string, f EntityField, i int, svc ServiceDef) string {
	col := f.Name
	protoType := f.ProtoType

	// Repeated scalar fields — emit a small deterministic array of
	// element-typed mocks so the fixture type-checks against the
	// protobuf-es `element[]` field.
	if base, ok := strings.CutPrefix(protoType, "repeated "); ok {
		elem := f
		elem.ProtoType = base
		a := mockGenerateValue(seed, tableName, elem, i, svc)
		b := mockGenerateValue(seed, tableName, elem, i+1, svc)
		return fmt.Sprintf("[%s, %s]", a, b)
	}

	ts, isScalar := protoScalarTSType(protoType)

	// What the database will hold at this cell, when there is a database to
	// agree with.
	if raw, ok := seed.Value(tableName, col, i); ok {
		if lit, ok := mockSeededLiteral(raw, f, ts, isScalar); ok {
			return lit
		}
		// An enum column's cell is the VALUE NAME the seeder wrote
		// ("ORDER_STATUS_ACTIVE"); protobuf-es represents an enum field as
		// its wire NUMBER, so the name has to be resolved through the
		// enum's declaration order before it can be a TypeScript literal.
		if lit, ok := mockSeededEnumLiteral(raw, f, protoType, svc); ok {
			return lit
		}
	}

	// Primary key, with no dataset to take it from. UUIDs are the project
	// default, but an identifier is a value like any other and its literal
	// has to be typed like any other: a project using distributed counters
	// types its id as int64, which protobuf-es emits as bigint, and a UUID
	// string will not type-check against it.
	if isScalar && col == "id" {
		if ts == "bigint" {
			return fmt.Sprintf("BigInt(%q)", fmt.Sprintf("%d", i+1))
		}
		if ts == "string" {
			return fmt.Sprintf("%q", mockDeterministicUUID(fmt.Sprintf("%s.%d", tableName, i)))
		}
	}

	// Timestamp fields — decided by the field's DECLARED type, never by its
	// name. A `string issued_at` or an epoch `int64 expires_at` is not a
	// Timestamp, and a `google.protobuf.Timestamp valid_from` is one; a
	// name test gets both backwards and emits TypeScript the frontend
	// typecheck lane rejects on a file the author is not allowed to edit.
	if isTimestampProtoField(f) {
		return mockGenerateTimestamp(col, i)
	}

	// Scalars: one literal per TypeScript type protobuf-es declares, so a
	// kind forge has never seen before cannot silently mock as a string.
	if isScalar {
		switch ts {
		case "boolean":
			if i%2 == 0 {
				return "true"
			}
			return "false"
		case "bigint":
			// 64-bit integers are bigint under protobuf-es's default
			// jstype — a plain number literal fails `tsc --noEmit`.
			return mockGenerateIntegerValue(i) + "n"
		case "number":
			if protoType == "float" || protoType == "double" {
				return mockGenerateFloatValue(i)
			}
			return mockGenerateIntegerValue(i)
		case "Uint8Array":
			return mockGenerateBytesValue(i)
		case "string":
			return fmt.Sprintf("%q", mockGenerateStringValue(col, i))
		}
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

	// Anything left is a kind with no scalar projection (a well-known
	// type carried through as its own name); a quoted string is the
	// inert fallback.
	return fmt.Sprintf("%q", mockGenerateStringValue(col, i))
}

// mockSeededLiteral renders one seeded cell as a TypeScript literal of the
// field's own declared type. ok is false when the raw value cannot BE that
// type — a text placeholder against a `double` field, say, which happens when
// the proto and the column disagree — and the caller then falls back rather
// than emitting TypeScript that fails `tsc` on a file the author may not edit.
func mockSeededLiteral(raw string, f EntityField, ts string, isScalar bool) (string, bool) {
	// A Timestamp field's cell is the instant the seeder wrote; protobuf-es
	// v2 wants a Timestamp object, not the ISO string.
	if isTimestampProtoField(f) {
		return fmt.Sprintf("timestampFromDate(new Date(%q))", raw), true
	}
	if !isScalar {
		return "", false
	}
	switch ts {
	case "string":
		return strconv.Quote(raw), true
	case "boolean":
		// The seeded cell is the SQL keyword the INSERT carries, which is
		// also the TypeScript spelling. Anything else against a bool field
		// (a proto/column disagreement) falls back rather than emitting a
		// literal `tsc` rejects.
		if raw != "true" && raw != "false" {
			return "", false
		}
		return raw, true
	case "number":
		if !isNumericLiteral(raw) {
			return "", false
		}
		return raw, true
	case "bigint":
		if !isNumericLiteral(raw) {
			return "", false
		}
		// 64-bit integers are bigint under protobuf-es's default jstype;
		// BigInt("…") keeps an id that outruns Number.MAX_SAFE_INTEGER exact.
		return fmt.Sprintf("BigInt(%q)", raw), true
	}
	return "", false
}

// mockSeededEnumLiteral renders one seeded cell of an ENUM column as the
// TypeScript literal protobuf-es expects: the value's wire NUMBER.
//
// The seeder writes the value NAME into the column (a text column whose
// CHECK vocabulary is the enum's members, or a native pg enum), because
// that is what the database stores. protobuf-es represents an enum field
// as a number at runtime, so a quoted name would fail `tsc` against the
// generated field type. The number is read from the enum's own
// declaration in the proto — svc.Enums carries the value names in
// declaration order — and never guessed from the seeded string.
//
// ok is false when the field is not an enum, the enum is unresolvable
// (cross-package, or a descriptor without the deep schema), or the
// seeded value is not one of its declared members. The caller then falls
// back rather than emitting an ordinal that means a different member.
func mockSeededEnumLiteral(raw string, f EntityField, protoType string, svc ServiceDef) (string, bool) {
	if protoType != "enum" && f.Kind != FieldKindEnum {
		return "", false
	}
	values, ok := mockEnumValueNames(f, svc)
	if !ok {
		return "", false
	}
	// Index in declaration order IS the wire number for a zero-based,
	// gap-free enum — which is what forge generates and what buf's
	// ENUM_ZERO_VALUE_SUFFIX lint keeps projects on. It is the only signal
	// available: the proto scan captures value NAMES in order and discards
	// the numbers (see rawEnumValueRE), so a hand-written enum that skips
	// or reorders numbers would resolve to the wrong member here. Carrying
	// the declared numbers through the scan is what would make this exact
	// rather than conventional.
	for n, name := range values {
		if name == raw {
			return strconv.Itoa(n), true
		}
	}
	return "", false
}

// mockEnumValueNames resolves the declared value names of an enum-typed
// field, in proto declaration order. ok is false when the field carries no
// resolvable enum type — the same unresolvable cases crud_convert's
// enumWireGoName refuses.
func mockEnumValueNames(f EntityField, svc ServiceDef) ([]string, bool) {
	fq := f.MessageType
	if fq == "" {
		return nil, false
	}
	values := svc.Enums[fq]
	if len(values) == 0 {
		return nil, false
	}
	return values, true
}

// isNumericLiteral reports whether raw is a bare number — what a numeric
// column's seeded cell decodes to, and what a TypeScript number/bigint
// literal may be built from without quoting.
func isNumericLiteral(raw string) bool {
	if raw == "" {
		return false
	}
	if _, err := strconv.ParseFloat(raw, 64); err != nil {
		return false
	}
	return true
}

// mockGenerateFloatValue is the float a column carries when no dataset
// answers for it: a deterministic run, and no claim about what the column
// means. It used to read the column's NAME — `probability`/`ratio`/`_rate`
// drew from [0,1), `percent`/`_pct` from [0,100] — which is the same guess
// the seeder deleted: a bounded column states its bounds in a range CHECK,
// and the plan reads them.
func mockGenerateFloatValue(i int) string {
	return fmt.Sprintf("%.2f", float64(i+1)*10.5)
}

// mockGenerateBytesValue produces the deterministic Uint8Array literal a
// `bytes` column mocks as.
//
// The bytes are a short deterministic run and nothing more: a BYTEA
// column is opaque by declaration — the schema does not say whether it
// holds a thumbnail, a signature or a protobuf — so any "plausible"
// content would be a domain guess the generator has no basis for, and
// the string pools that serve `name`/`title` are not even the right
// TYPE here. Length varies with the row so a fixture set doesn't imply
// every blob is the same size.
func mockGenerateBytesValue(i int) string {
	n := 3 + i%4
	parts := make([]string, 0, n)
	for b := 0; b < n; b++ {
		parts = append(parts, fmt.Sprintf("%d", (i*7+b*31+1)%256))
	}
	return "new Uint8Array([" + strings.Join(parts, ", ") + "])"
}

// isTimestampProtoField reports whether a field is declared as a
// google.protobuf.Timestamp. Descriptors collapse every message field's
// ProtoType to the literal "message" and carry the fully-qualified name in
// MessageType, while some projections spell the FQ name (or the bare
// "Timestamp") in ProtoType directly — both are the same declaration.
func isTimestampProtoField(f EntityField) bool {
	const wkt = "google.protobuf.Timestamp"
	return f.MessageType == wkt || f.ProtoType == wkt || f.ProtoType == "Timestamp"
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

// mockGenerateIntegerValue is the integer a column carries when no dataset
// answers for it. It used to read the column's NAME — `age` became 20-70,
// `price`/`amount`/`*_cents` became thousands, `quantity`/`count` multiples of
// five — the same guess about what a column MEANS that the seeder deleted.
// What a number should be is either declared (a range CHECK, which the plan
// reads) or it is not forge's to know.
func mockGenerateIntegerValue(i int) string {
	return fmt.Sprintf("%d", i+1)
}

// mockGenerateStringValue is the string a column carries when no dataset
// answers for it: the seeder's own stamp, the column's name as a LABEL, and
// the row number — the identical spelling internal/seeddata gives a column
// nothing describes, so the two agree even here.
//
// It used to dispatch on the column's name into vocabulary pools: `name` drew
// from a company list, `status` from {active, pending, ...}, `title` from a
// documentation-chapter list. That is a decision about what a column MEANS,
// the schema does not carry it, and forge cannot derive it — the seeder
// deleted the identical dispatch in 5f8993ab. A project teaches BOTH its
// vocabulary in one place, db/seeds/vocab.yaml, and it arrives here through
// the plan.
func mockGenerateStringValue(col string, i int) string {
	return seeddata.SyntheticStringPrefix + col + "_" + strconv.Itoa(i+1)
}
