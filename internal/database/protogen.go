package database

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/jinzhu/inflection"
	"github.com/reliant-labs/forge/internal/naming"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// SQLToProtoType maps a Postgres data type (from information_schema.columns.data_type
// or udt_name) to a protobuf scalar type. Returns the proto type and whether
// google.protobuf.Timestamp needs to be imported.
func SQLToProtoType(sqlType, udtName string) (protoType string, needsTimestamp bool) {
	// Normalize to lowercase for matching.
	lower := strings.ToLower(sqlType)
	udt := strings.ToLower(udtName)

	switch lower {
	case "integer", "int", "int4":
		return "int32", false
	case "smallint", "int2":
		return "int32", false
	case "bigint", "int8":
		return "int64", false
	case "real":
		return "float", false
	case "double precision":
		return "double", false
	case "boolean":
		return "bool", false
	case "text", "character varying", "character", "varchar", "char":
		return "string", false
	case "uuid":
		return "string", false
	case "bytea":
		return "bytes", false
	case "numeric", "decimal":
		return "string", false
	case "json", "jsonb":
		return "string", false
	case "timestamp without time zone", "timestamp with time zone":
		return "google.protobuf.Timestamp", true
	case "date":
		return "string", false
	case "time without time zone", "time with time zone":
		return "string", false
	case "interval":
		return "string", false
	case "array":
		// For arrays, map the element type and mark as repeated.
		// The udt_name for arrays starts with _ (e.g., _int4, _text).
		elemType := strings.TrimPrefix(udt, "_")
		pt, ts := SQLToProtoType(elemType, elemType)
		return "repeated " + pt, ts
	case "user-defined":
		// Enums and custom types — fall through to udt_name matching.
	}

	// Fallback: try matching on udt_name.
	switch udt {
	case "int4":
		return "int32", false
	case "int2":
		return "int32", false
	case "int8":
		return "int64", false
	case "float4":
		return "float", false
	case "float8":
		return "double", false
	case "bool":
		return "bool", false
	case "text", "varchar", "bpchar":
		return "string", false
	case "uuid":
		return "string", false
	case "bytea":
		return "bytes", false
	case "numeric":
		return "string", false
	case "json", "jsonb":
		return "string", false
	case "timestamp", "timestamptz":
		return "google.protobuf.Timestamp", true
	case "serial":
		return "int32", false
	case "bigserial", "serial8":
		return "int64", false
	}

	// Unknown types default to string.
	return "string", false
}

// ProtoField represents a field in a proto message for template rendering.
type ProtoField struct {
	ProtoType  string
	ProtoName  string
	Number     int
	IsPrimary  bool
	NotNull    bool
	HasDefault bool
	Default    string
	References string // "table.column" for FKs
	IsRepeated bool
}

// FieldOptionsString returns the textproto representation of field_options for this field.
func (f ProtoField) FieldOptionsString() string {
	var opts []string
	if f.IsPrimary {
		opts = append(opts, "primary_key: true")
	}
	if f.NotNull && !f.IsPrimary {
		opts = append(opts, "not_null: true")
	}
	if f.References != "" {
		opts = append(opts, fmt.Sprintf("references: %q", f.References))
	}
	return strings.Join(opts, ", ")
}

// HasFieldOptions returns true if this field has any field_options to emit.
func (f ProtoField) HasFieldOptions() bool {
	return f.IsPrimary || (f.NotNull && !f.IsPrimary) || f.References != ""
}

// ProtoMessage represents a proto message for template rendering.
type ProtoMessage struct {
	MessageName   string
	TableName     string
	Fields        []ProtoField
	HasTimestamps bool
	HasSoftDelete bool
	NeedsTimestamp bool
}

// ProtoFileData is the top-level data passed to the proto file template.
type ProtoFileData struct {
	Package   string
	GoPackage string
	Messages  []ProtoMessage
}

var protoFuncMap = template.FuncMap{
	"needsTimestamp": func(msgs []ProtoMessage) bool {
		for _, m := range msgs {
			if m.NeedsTimestamp {
				return true
			}
		}
		return false
	},
}

var protoFileTmpl = template.Must(template.New("proto").Funcs(protoFuncMap).Parse(`syntax = "proto3";

package {{ .Package }};
{{ if needsTimestamp .Messages }}
import "google/protobuf/timestamp.proto";
{{ end -}}
import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";

option go_package = "{{ .GoPackage }}";
{{ range .Messages }}
message {{ .MessageName }} {
  option (forge.options.v1.entity_options) = {
    table_name: "{{ .TableName }}"
    timestamps: {{ .HasTimestamps }}
    soft_delete: {{ .HasSoftDelete }}
  };
{{ range .Fields }}
  {{ .ProtoType }} {{ .ProtoName }} = {{ .Number }}{{ if .HasFieldOptions }} [(forge.options.v1.field_options) = { {{ .FieldOptionsString }} }]{{ end }};
{{ end -}}
}
{{ end -}}
`))

// GenerateProtoFiles generates proto files from the given database tables.
// One proto file is generated per table, written to outputDir.
func GenerateProtoFiles(tables []Table, outputDir, goModule string) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	for _, table := range tables {
		if err := generateProtoFile(table, outputDir, goModule); err != nil {
			return fmt.Errorf("generating proto for table %s: %w", table.Name, err)
		}
	}
	return nil
}

func generateProtoFile(table Table, outputDir, goModule string) error {
	msg := TableToProtoMessage(table)

	data := ProtoFileData{
		Package:   "db.v1",
		GoPackage: goModule + "/gen/db/v1;dbv1",
		Messages:  []ProtoMessage{msg},
	}

	filename := filepath.Join(outputDir, table.Name+".proto")

	var buf bytes.Buffer
	if err := protoFileTmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}

	return os.WriteFile(filename, buf.Bytes(), 0644)
}

// TableToProtoMessage converts a Table to a ProtoMessage for rendering.
func TableToProtoMessage(table Table) ProtoMessage {
	// Build FK lookup: column -> "refTable.refColumn"
	fkLookup := make(map[string]string)
	for _, fk := range table.ForeignKeys {
		fkLookup[fk.Column] = fk.ReferencedTable + "." + fk.ReferencedColumn
	}

	msg := ProtoMessage{
		MessageName: tableNameToMessageName(table.Name),
		TableName:   table.Name,
	}

	fieldNum := 1
	for _, col := range table.Columns {
		protoType, needsTs := SQLToProtoType(col.Type, col.UDTName)
		if needsTs {
			msg.NeedsTimestamp = true
		}

		isRepeated := strings.HasPrefix(protoType, "repeated ")

		field := ProtoField{
			ProtoType:  protoType,
			ProtoName:  naming.ToSnakeCase(col.Name),
			Number:     fieldNum,
			IsPrimary:  col.IsPrimary,
			NotNull:    !col.Nullable,
			HasDefault: col.Default != "",
			Default:    col.Default,
			References: fkLookup[col.Name],
			IsRepeated: isRepeated,
		}

		msg.Fields = append(msg.Fields, field)
		fieldNum++

		// Detect timestamps/soft_delete patterns.
		switch col.Name {
		case "created_at", "updated_at":
			msg.HasTimestamps = true
		case "deleted_at":
			msg.HasSoftDelete = true
		}
	}

	return msg
}

// tableNameToMessageName converts a snake_case table name to PascalCase message name.
// "user_accounts" -> "UserAccount", "addresses" -> "Address", "statuses" -> "Status".
func tableNameToMessageName(tableName string) string {
	// Singularize each underscore-delimited word so compound names work correctly.
	parts := strings.Split(tableName, "_")
	titleCaser := cases.Title(language.English)
	for i, p := range parts {
		parts[i] = titleCaser.String(inflection.Singular(p))
	}
	return strings.Join(parts, "")
}