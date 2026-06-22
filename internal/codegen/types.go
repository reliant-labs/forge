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

	// Schemas is the deep type graph for full JSON-Schema emission
	// (MCP manifest): fully-qualified message name → fields, covering
	// every message transitively reachable from any method's input or
	// output type. Well-known types (google.protobuf.*) are NOT
	// included — consumers map those to fixed JSON encodings matching
	// protojson. Empty/nil on descriptors produced by forge versions
	// before this field existed; consumers must fall back to the
	// shallow Messages map in that case. Keyed by fully-qualified name
	// (e.g. "shop.v1.Address") so cross-package short-name collisions
	// can't alias two different messages.
	Schemas map[string][]SchemaFieldDef `json:",omitempty"`

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
	AuthRequired    bool // from (forge.v1.method).auth_required; defaults to true (fail-closed) when unannotated
	// RequiredRoles is the per-method role allow-list. NO proto annotation
	// populates it — forge.v1.MethodOptions has no required_roles field;
	// role policy is code (the user-owned handlers/<svc>/authorizer.go),
	// not proto. The field exists so the authorizer table shape can carry
	// roles if a non-proto source ever supplies them; today it is always
	// empty in parsed descriptors.
	RequiredRoles []string
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
// Timestamps / HasTenant are conventions read off real columns
// (deleted_at, created_at+updated_at, tenant_id) — never annotations.
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
	SearchColumns    []string `json:",omitempty"`
	SoftDelete       bool     `json:",omitempty"`
	Timestamps       bool     `json:",omitempty"`
	HasTenant        bool     // true when the table has a tenant_id column
	TenantFieldName  string   // proto field name: "tenant_id"
	TenantGoName     string   // Go name: "TenantId"
	TenantColumnName string   // DB column: "tenant_id"
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
}

// FieldKind classifies a proto field for code generation branching.
type FieldKind string

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
	// MessageType carries the fully-qualified message name for
	// message-typed fields (e.g. "google.protobuf.Timestamp"). ProtoType
	// collapses these to "message", which made every timestamp column
	// degrade to TEXT in plan-based migrations/ORM. Additive.
	MessageType string `json:",omitempty"`
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
	// keys semantic behavior (Mode()/DevAuthBypass()) on THIS, never on the
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
		// Scalar types — check for repeated
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

// ProtoTypeToGoType converts a proto type to its Go equivalent.
func ProtoTypeToGoType(protoType string) string {
	switch protoType {
	case "string":
		return "string"
	case "int32":
		return "int32"
	case "int64":
		return "int64"
	case "bool":
		return "bool"
	case "float":
		return "float32"
	case "double":
		return "float64"
	default:
		return "string"
	}
}
