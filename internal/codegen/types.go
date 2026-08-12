package codegen

import "strings"

// ServiceDef represents a parsed Connect RPC service definition.
type ServiceDef struct {
	Name       string // "EchoService"
	Package    string // "echo.v1"
	GoPackage  string // "github.com/.../gen/proto/echo/v1"
	PkgName    string // "echov1"
	Methods    []Method
	ProtoFile  string
	ModulePath string                       // e.g., "github.com/demo-project"
	Messages   map[string][]MessageFieldDef // message name → fields (e.g., "ListPatientsRequest" → [...])

	// Schemas is the deep type graph for full JSON-Schema emission:
	// fully-qualified message name → fields, covering
	// every message transitively reachable from any method's input or
	// output type. Well-known types (google.protobuf.*) are NOT
	// included — consumers map those to fixed JSON encodings matching
	// protojson. Empty/nil on descriptors produced by forge versions
	// before this field existed; consumers must fall back to the
	// shallow Messages map in that case. Keyed by fully-qualified name
	// (e.g. "shop.v1.Address") so cross-package short-name collisions
	// can't alias two different messages.
	Schemas map[string][]SchemaFieldDef `json:",omitempty"`

	// SchemaFiles maps a fully-qualified message name (the same keys used
	// in Schemas) to the proto file that physically DECLARES that message.
	// For a message declared in the service's own proto file this equals
	// ProtoFile; for a message pulled in from another file (e.g. a shared
	// `shared.proto` holding the domain entity messages while the CRUD
	// service lives in `services/<svc>/v1/<svc>.proto`) it differs. Codegen
	// that emits a `*Schema` / type import for a message must resolve the
	// import path from the message's DEFINING file, not the service file —
	// otherwise it imports a symbol from a `_pb.ts` module that doesn't
	// export it. Empty/nil on descriptors produced by forge versions before
	// this field existed; consumers must fall back to the service ProtoFile.
	SchemaFiles map[string]string `json:",omitempty"`

	// Enums maps fully-qualified enum name → declared value names, in
	// proto declaration order, for every enum reachable through
	// Schemas. protojson encodes enums as their value-name strings, so
	// this is exactly the "enum" list a JSON Schema needs.
	Enums map[string][]string `json:",omitempty"`
}

// Method represents a single RPC method.
type Method struct {
	Name            string
	InputType       string
	OutputType      string
	ClientStreaming bool
	ServerStreaming bool
	// AuthRequired is (forge.v1.method).auth_required: true unless the
	// proto explicitly opts out, since the field is `optional bool` and
	// an unannotated RPC is taken to want a principal.
	//
	// It is INFORMATIONAL and FAILS OPEN. Nothing consults it at
	// runtime: it feeds `forge project map`/`graph`,
	// and it selects which sentence the scaffolded handler comment shows
	// the author. An RPC declaring auth_required: true and doing nothing
	// about it serves anonymous callers, which is what a measured run
	// found in 17 of 20 generated CRUD RPCs. Making it fail closed is an
	// interceptor decision, not a codegen one.
	AuthRequired bool
	// Errors records the Connect/gRPC error codes this method may return,
	// derived from (forge.v1.method).errors. Values match connect.Code
	// names (e.g. "NotFound", "PermissionDenied"). Surfaced through
	// generated code so handler authors see the typed error contract at
	// a glance. Informational at runtime — no enforcement (yet).
	Errors []string
	// InputTypeFQ / OutputTypeFQ are the fully-qualified names of the
	// request/response messages (e.g. "shop.v1.CreateOrderRequest",
	// "google.protobuf.Empty"). They key into ServiceDef.Schemas for
	// deep JSON-Schema emission. Empty on descriptors produced by
	// older forge versions — consumers fall back to the short-name
	// InputType/OutputType + Messages map.
	InputTypeFQ  string `json:",omitempty"`
	OutputTypeFQ string `json:",omitempty"`
	// InputProtoFile / OutputProtoFile record the proto file path that
	// physically declares the input/output message. For RPCs whose
	// request/response live in the same proto file as the service these
	// equal ServiceDef.ProtoFile, but they differ when an RPC references
	// a message from another file (e.g. services/users/v1/users.proto's
	// ListUsers returns shared/v1/types.proto's Page). The frontend hooks
	// generator groups imports by these paths so each cross-file message
	// is imported from its declaring _pb.ts file rather than silently
	// referenced as an unresolved identifier.
	InputProtoFile  string
	OutputProtoFile string
}

// MessageFieldDef represents a single field in a proto message definition.
type MessageFieldDef struct {
	Name       string // proto field name: "page_size", "search", "active"
	ProtoType  string // "int32", "string", "bool"
	IsOptional bool   // true if the field has the "optional" label
	// MessageType carries the referenced message's name for message-typed
	// fields (e.g. "Item" for `Item item = 1;`, "google.protobuf.FieldMask"
	// for masks). ProtoType collapses every message field to the literal
	// "message" — which is how the CRUD shape matcher could never match an
	// update request's entity field against the entity name (the false
	// custom-read-shape stub — then spelled FORGE_CRUD_SHAPE_MISMATCH —
	// on forge's own scaffold). Additive:
	// `json:",omitempty"` keeps old descriptors parseable.
	MessageType string `json:",omitempty"`
}

// SchemaFieldDef is the deep-schema sibling of MessageFieldDef: one
// field of a message in ServiceDef.Schemas, carrying enough type
// information to project a full (nested) JSON Schema without consulting
// the proto source. Unlike MessageFieldDef.ProtoType (which collapses
// messages/enums/maps into opaque strings), this keeps the type graph:
// message and enum fields name their fully-qualified target so a schema
// emitter can $ref into a shared definitions block.
type SchemaFieldDef struct {
	Name string `json:"name"` // proto field name, snake_case ("page_size")
	// Kind is the proto scalar kind name ("string", "int32", "bool",
	// "bytes", ...) or one of the structured markers "message", "enum",
	// "map".
	Kind string `json:"kind"`
	// TypeName is the fully-qualified message/enum name when Kind is
	// "message" or "enum" (e.g. "shop.v1.Address",
	// "google.protobuf.Timestamp"). Empty for scalars and maps.
	TypeName string `json:"type_name,omitempty"`
	// Repeated marks proto `repeated` fields (JSON arrays). Always
	// false for maps — proto maps are repeated entry messages under
	// the hood, but their JSON encoding is an object, not an array.
	Repeated bool `json:"repeated,omitempty"`
	// Optional is true when the field carries the explicit `optional`
	// label (proto3 field presence). Optional fields stay out of JSON
	// Schema `required` lists.
	Optional bool `json:"optional,omitempty"`
	// Oneof is the containing oneof group's name for members of a
	// real (non-synthetic) oneof. proto3 `optional` fields use a
	// synthetic oneof internally; those report "" here and Optional
	// true instead.
	Oneof string `json:"oneof,omitempty"`
	// Map-typed fields (Kind == "map") record the key/value kinds.
	// MapValueTypeName names the fully-qualified message/enum when the
	// value kind is "message"/"enum".
	MapKeyKind       string `json:"map_key_kind,omitempty"`
	MapValueKind     string `json:"map_value_kind,omitempty"`
	MapValueTypeName string `json:"map_value_type_name,omitempty"`
	// Validate carries the field's protovalidate (buf.validate.field)
	// rules, projected to a DB CHECK at entity birth and a zod validator
	// in the generated form (the wire is enforced by the interceptor).
	// nil when the field carries no rules. Populated by both extractors —
	// the compiled descriptor and the raw proto scan.
	Validate *FieldConstraints `json:"validate,omitempty"`
	// ValidateOptions is the field's `(buf.validate.field)` inline-options
	// block exactly as authored, collapsed to one line
	// (`[(buf.validate.field).string.min_len = 1]`), or "" when the field
	// carries no protovalidate rule. Unlike Validate — the lossy subset
	// forge projects to SQL/zod — this is the whole declaration, so a
	// request message that FLATTENS this field (Create<Entity>Request) can
	// re-declare the SAME rules and have the wire interceptor enforce them.
	// Populated by the raw proto scan, which reads the authored spelling off
	// the file the injection edits. `json:",omitempty"` keeps old
	// descriptors parseable (additive contract).
	ValidateOptions string `json:"validate_options,omitempty"`
	// Secret marks a field carrying the `// forge:secret` leading-comment
	// marker: the column is real (schema truth), but the generated
	// proto←entity conversion (the toProto read path) NEVER packs it, so
	// Get/List responses omit it — while Create/Update stay free to set it.
	// Read off the compiled descriptor's source-code comments (the raw
	// proto scan does not need it — birth stores the column unconditionally).
	// `json:",omitempty"` keeps old descriptors parseable (additive contract).
	Secret bool `json:"secret,omitempty"`
	// ReadOnly marks a field carrying the `// forge:read-only` marker: the
	// INPUT-side mirror of Secret. The column is real and stays on the entity
	// message + Get/List responses (readable), but the field is OMITTED from
	// the generated Create/Update REQUEST messages — a value (status,
	// computed price, lifecycle timestamp) the client must not write. Read
	// off BOTH extractors (the compiled descriptor's source-code comments AND
	// the raw proto scan — a brand-new `// forge:entity` message births its
	// quintet from the raw scan, before the descriptor knows it).
	// `json:",omitempty"` keeps old descriptors parseable (additive contract).
	ReadOnly bool `json:"read_only,omitempty"`
}

// IsInputEmpty returns true if the input type is google.protobuf.Empty.
func (m Method) IsInputEmpty() bool {
	return m.InputType == "google.protobuf.Empty"
}

// IsOutputEmpty returns true if the output type is google.protobuf.Empty.
func (m Method) IsOutputEmpty() bool {
	return m.OutputType == "google.protobuf.Empty"
}

// GoInputType returns the Go type reference for the input (handles Empty).
func (m Method) GoInputType() string {
	if m.IsInputEmpty() {
		return "emptypb.Empty"
	}
	return "pb." + m.InputType
}

// GoOutputType returns the Go type reference for the output (handles Empty).
func (m Method) GoOutputType() string {
	if m.IsOutputEmpty() {
		return "emptypb.Empty"
	}
	return "pb." + m.OutputType
}

// EntityDef is a database entity: the join of an introspected table
// from the APPLIED schema (db/migrations executed against the shadow
// DB — the storage truth) with the service-proto CRUD message shape
// (the wire truth).
//
// Columns drive the ORM and entity structs; Fields drive the frontend
// and the proto<->entity conversion in the CRUD wiring. SoftDelete /
// Timestamps are conventions read off real columns
// (deleted_at, created_at+updated_at) — never annotations.
type EntityDef struct {
	Name      string        // "Patient"
	TableName string        // "patients"
	PkField   string        // "id"
	PkGoType  string        // "string"
	Fields    []EntityField // wire-message fields (service proto)
	ProtoFile string        // proto file declaring the wire message
	// Columns is the introspected applied schema for the entity's table.
	Columns []EntityColumn `json:",omitempty"`
	// SearchColumns are the text columns the generated list search
	// filter matches against (convention: every text column).
	SearchColumns []string `json:",omitempty"`
	SoftDelete    bool     `json:",omitempty"`
	Timestamps    bool     `json:",omitempty"`
}

// EntityColumn is one introspected column of an entity's table.
type EntityColumn struct {
	Name string // column name, snake_case
	// Type is the canonical type: "string", "int64", "float64",
	// "bool", "time", "json", "bytes" (matches schemadef.CanonicalType).
	Type    string
	IsArray bool
	NotNull bool
	IsPK    bool
	// DeclType is the declared SQL type verbatim ("TIMESTAMPTZ").
	DeclType string `json:",omitempty"`
	Default  string `json:",omitempty"`
	// IsGenerated marks GENERATED ALWAYS AS (...) STORED columns — the DB
	// computes them, so the ORM must never write them (Bun's ,scanonly).
	IsGenerated bool `json:",omitempty"`
	// Immutable marks a column declared `forge:immutable` in its catalog
	// comment (COMMENT ON COLUMN). It projects to Bun's `skipupdate`, so a
	// full-replace UPDATE leaves the stored value alone.
	Immutable bool `json:",omitempty"`
	// Version marks a column declared `forge:version` in its catalog
	// comment: the entity opts into optimistic concurrency control. See
	// forge/pkg/crud's meta.versionColumn for the write-path behavior.
	Version bool `json:",omitempty"`
	// FillStrategy carries a `forge:fill=<strategy>` declaration verbatim
	// ("ulid" or "handler"), "" when the column declares none. It names WHO
	// fills a column the wire never carries a value for — "ulid" makes the
	// ORM generate one at Create (see forge/pkg/crud's meta.fillULIDCols);
	// "handler" is pure acknowledgement that suppresses the
	// unsatisfiable-column lint and changes no codegen behavior. See
	// schemadef.ColumnMarkerFill for the full grammar and rationale.
	FillStrategy string `json:",omitempty"`
}

// FieldKind classifies a proto field for code generation branching.
type FieldKind string

// The FieldKind values a proto field can be classified as. Kinds drive
// which conversion path the CRUD generator emits for the field.
const (
	FieldKindScalar          FieldKind = "scalar"
	FieldKindEnum            FieldKind = "enum"
	FieldKindMessage         FieldKind = "message"
	FieldKindMap             FieldKind = "map"
	FieldKindRepeatedScalar  FieldKind = "repeated_scalar"
	FieldKindRepeatedMessage FieldKind = "repeated_message"
	FieldKindWrapper         FieldKind = "wrapper"   // google.protobuf.*Value
	FieldKindTimestamp       FieldKind = "timestamp" // google.protobuf.Timestamp
)

// EntityField represents a single field in an entity.
type EntityField struct {
	Name      string    // Proto field name: "patient_id"
	GoName    string    // Go name: "PatientId"
	ProtoType string    // "int64", "string", etc.
	GoType    string    // "int64", "string", etc.
	Kind      FieldKind // scalar, enum, message, etc.
	IsFK      bool
	FKTable   string // "patients" (if FK)
	// Optional is true when the wire field carries the explicit proto3
	// `optional` label. protobuf-go generates a POINTER struct field
	// (*string, *int64, ...) for explicit-presence scalars, so
	// conversion codegen must pair optional scalars like wrappers
	// (nil-safe pointer handling — see promoteOptionalScalar), never
	// like plain scalars. GoType/Kind here stay the PLAIN spelling; the
	// conversion layer promotes at its boundary so other consumers
	// (frontend projections) are untouched. Additive:
	// `json:",omitempty"` keeps old descriptors parseable.
	Optional bool `json:",omitempty"`
	// MessageType carries the fully-qualified message name for
	// message-typed fields (e.g. "google.protobuf.Timestamp"). ProtoType
	// collapses these to "message", which made every timestamp column
	// degrade to TEXT in plan-based migrations/ORM. Additive.
	MessageType string `json:",omitempty"`
	// Secret carries the field's `// forge:secret` marker through to the
	// conversion generator: a secret field is skipped in the toProto (read)
	// direction — never packed onto a Get/List response — while staying
	// settable on Create/Update. Additive (`json:",omitempty"`).
	Secret bool `json:",omitempty"`
	// ReadOnly carries the field's `// forge:read-only` marker: the field is
	// not client-writable, so the CRUD test's mutable/clobber-field
	// selection (and any other client-input projection) skips it. The value
	// stays on the entity + read responses; only the client-settable request
	// surfaces exclude it. Additive (`json:",omitempty"`).
	ReadOnly bool `json:",omitempty"`
	// MapValueKind is the proto kind of a map field's VALUE ("string",
	// "int64", "message", "enum", ...). GoType collapses every map to the
	// marker "map[string]string", so without this the conversion generator
	// cannot tell a map of scalars (which JSON round-trips as-is) from a
	// map of messages or enums (which do NOT — they would store protobuf
	// struct internals and enum wire NUMBERS respectively). Empty for
	// non-map fields. Additive (`json:",omitempty"`).
	MapValueKind string `json:",omitempty"`
}

// ConfigField represents a single field in a config proto message
// with ConfigFieldOptions annotations.
type ConfigField struct {
	Name         string // Proto field name (e.g., "database_url")
	GoName       string // Go field name (e.g., "DatabaseUrl")
	GoType       string // Go type (e.g., "string", "int32", "bool")
	ProtoType    string // Proto type (e.g., "string", "int32", "bool")
	EnvVar       string // From config_field.env_var
	Flag         string // From config_field.flag
	DefaultValue string // From config_field.default_value
	Required     bool   // From config_field.required
	Description  string // From config_field.description
	Sensitive    bool   // From config_field.sensitive — projects to a Secret in deploy
	Category     string // From config_field.category — groups fields in deploy gen

	// Role is the (forge.v1.config).role annotation as the bare enum spelling
	// (e.g. "CONFIG_FIELD_ROLE_MODE"; "" for UNSPECIFIED). Config codegen
	// keys semantic behavior (Mode()) on THIS, never on the
	// field NAME — so renaming a field never changes behavior, and naming a
	// field "environment" without the annotation never auto-enables dev mode.
	// `json:",omitempty"` keeps old descriptors readable (additive contract).
	Role string `json:",omitempty"`

	// MessageType names the referenced config message when this field is
	// a component config-block reference (ProtoType == "message"), e.g. a
	// root `AppConfig` field `TraderConfig trader = 21;` records
	// MessageType "TraderConfig". Empty for scalar fields. Block-reference
	// fields carry no env_var/flag of their own — env binding lives on the
	// referenced message's leaf fields. `json:",omitempty"` keeps old
	// descriptors readable (additive contract, see audit-json skill).
	MessageType string `json:",omitempty"`
}

// ConfigMessage represents a parsed config proto message.
type ConfigMessage struct {
	Name   string        // Message name (e.g., "AppConfig")
	Fields []ConfigField // Fields with config_field annotations

	// Binary is the cmd/<name> leaf this config belongs to, from the
	// message-level (forge.v1.binary_config).binary annotation. Empty means
	// the message is not bound to a binary: either the project's single
	// project-global AppConfig (the common case — nothing to annotate) or a
	// shared block composed onto other configs (BaseConfig).
	//
	// It is what makes per-binary ownership DISJOINT: a binary's generated
	// loader, KCL schema and env projection are built from the message
	// carrying its name, so deleting a field from one binary's config
	// cannot reach another's. Keyed on the ANNOTATION, never on the message
	// NAME — renaming AdminConfig does not re-point it at another binary.
	// `json:",omitempty"` keeps descriptors written by older forge binaries
	// readable (additive contract, see the audit-json skill).
	Binary string `json:",omitempty"`

	// Frontend is the forge.yaml frontends[].name whose BUNDLE loads this
	// config, from the message-level (forge.v1.frontend_config).frontend
	// annotation. Empty means the message is not bound to a frontend.
	//
	// It plays the same disjoint-ownership role Binary does, one layer out:
	// a frontend's generated TypeScript module, KCL schema and per-env
	// projection are built from the message carrying its name. The
	// difference is what the binding IMPLIES about secrecy — a
	// frontend-bound message is delivered to a browser, so every field it
	// carries is public and a `sensitive` field reaching one is a
	// generate-time error (see ValidateFrontendConfigs).
	//
	// A message may carry BOTH annotations only if that is genuinely
	// intended; the shared-definition pattern is to compose one unannotated
	// block (BaseConfig) onto an annotated binary config AND an annotated
	// frontend config, so the shared fact is declared once.
	Frontend string `json:",omitempty"`
}

// DetermineFieldKind classifies a field based on its ProtoType and GoType.
func DetermineFieldKind(protoType, goType string) FieldKind {
	switch protoType {
	case "enum":
		return FieldKindEnum
	case "message":
		// Check for well-known wrapper/timestamp types via GoType
		switch {
		case goType == "*timestamppb.Timestamp":
			return FieldKindTimestamp
		case strings.HasPrefix(goType, "*") && isWrapperGoType(goType):
			return FieldKindWrapper
		case strings.HasPrefix(goType, "map["):
			return FieldKindMap
		case strings.HasPrefix(goType, "[]") && strings.HasPrefix(goType[2:], "*"):
			return FieldKindRepeatedMessage
		default:
			return FieldKindMessage
		}
	default:
		// Scalar types — a "[]" prefix means `repeated`, EXCEPT for
		// `bytes`, whose singular Go type is already a slice. A `bytes`
		// field classified as repeated_scalar would be paired against an
		// array column and converted element-wise, so the discriminator
		// for it is the extra slice level: []byte is one value, [][]byte
		// is many.
		if protoType == "bytes" {
			if goType == "[][]byte" {
				return FieldKindRepeatedScalar
			}
			return FieldKindScalar
		}
		if strings.HasPrefix(goType, "[]") {
			return FieldKindRepeatedScalar
		}
		return FieldKindScalar
	}
}

// isWrapperGoType returns true if the Go type is a well-known protobuf wrapper
// that unwraps to a scalar (e.g. *string from StringValue, *int32 from Int32Value).
func isWrapperGoType(goType string) bool {
	switch goType {
	case "*string", "*int32", "*int64", "*uint32", "*uint64",
		"*bool", "*float32", "*float64":
		return true
	}
	return false
}

// ProtoTypeToGoType converts a proto SCALAR kind to the Go type
// protoc-gen-go generates for it.
//
// This is not forge's choice to make: the generated `pb` package already
// declares the field, and every projection that disagrees with it produces
// code that either refuses a legal shape or does not compile. Nine of the
// fifteen kinds used to answer "string" here — every unsigned, zigzag and
// fixed-width integer, plus `bytes` — with two different consequences:
//
//   - `bytes` and the eight integer kinds were rejected by the CRUD
//     conversion gate, so an app declaring one could not run
//     `forge generate` at all;
//   - `repeated bytes` PASSED, because the wire side answered []string and
//     the column side (dbBaseGoType, collapsing every non-BIGINT[] array)
//     also answered []string. Two wrong answers agreeing looked exactly
//     like a mapped pairing, and the generator emitted
//     `append([]string(nil), m.Chunks...)` against protoc-gen-go's real
//     [][]byte field.
//
// A non-scalar kind ("message", "enum", "map", "group") has no scalar Go
// type; callers branch on those BEFORE reaching here (see
// schemaFieldToEntityField, classifyFilterField), and the "string" answer
// is the inert fallback for a kind that never should have arrived. It is
// spelled here, at the one site that needs it, rather than as a `default:`
// arm on the table: a fallback inside the projection is reached by an
// unrecognised kind too, and cannot be told apart from a mapping.
func ProtoTypeToGoType(protoType string) string {
	if goType, ok := ProtoScalarGoType(protoType); ok {
		return goType
	}
	return "string"
}
