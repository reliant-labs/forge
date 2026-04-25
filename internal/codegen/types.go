package codegen

import "strings"

// ServiceDef represents a parsed Connect RPC service definition.
type ServiceDef struct {
	Name       string   // "EchoService"
	Package    string   // "echo.v1"
	GoPackage  string   // "github.com/.../gen/proto/echo/v1"
	PkgName    string   // "echov1"
	Methods    []Method
	ProtoFile  string
	ModulePath string // e.g., "github.com/demo-project"
	Messages   map[string][]MessageFieldDef // message name → fields (e.g., "ListPatientsRequest" → [...])
}

// Method represents a single RPC method.
type Method struct {
	Name           string
	InputType      string
	OutputType     string
	ClientStreaming bool
	ServerStreaming bool
	AuthRequired   bool     // from forge.options.v1.method_options.auth_required
	RequiredRoles  []string // from forge.options.v1.method_options.required_roles
}

// MessageFieldDef represents a single field in a proto message definition.
type MessageFieldDef struct {
	Name       string // proto field name: "page_size", "search", "active"
	ProtoType  string // "int32", "string", "bool"
	IsOptional bool   // true if the field has the "optional" label
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

// EntityDef represents a parsed database entity from proto files.
type EntityDef struct {
	Name             string        // "Patient"
	TableName        string        // "patients"
	PkField          string        // "id"
	PkGoType         string        // "int64"
	Fields           []EntityField // all fields including PK
	ProtoFile        string        // "proto/services/patients/v1/patients.proto"
	HasTenant        bool          // true when the entity has a tenant key field
	TenantFieldName  string        // proto field name: "org_id"
	TenantGoName     string        // Go name: "OrgId"
	TenantColumnName string        // DB column: "org_id"
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