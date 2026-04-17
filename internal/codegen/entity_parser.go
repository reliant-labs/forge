package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/reliant-labs/forge/internal/naming"
)

// EntityDef represents a parsed database entity from proto/db/.
type EntityDef struct {
	Name      string        // "Patient"
	TableName string        // "patients"
	PkField   string        // "id"
	PkGoType  string        // "int64"
	Fields    []EntityField // all fields including PK
	ProtoFile string        // "proto/db/v1/entities.proto"
}

// EntityField represents a single field in an entity.
type EntityField struct {
	Name      string // Proto field name: "patient_id"
	GoName    string // Go name: "PatientId"
	ProtoType string // "int64", "string", etc.
	GoType    string // "int64", "string", etc.
	IsFK      bool
	FKTable   string // "patients" (if FK)
}

// ParseEntityProtos scans proto/db/ for entity message definitions and returns
// them as EntityDefs. It uses the protocompile parser (same as service parser).
func ParseEntityProtos(projectDir string) ([]EntityDef, error) {
	dbProtoDir := filepath.Join(projectDir, "proto", "db")
	if _, err := os.Stat(dbProtoDir); os.IsNotExist(err) {
		return nil, nil
	}

	var entities []EntityDef
	err := filepath.Walk(dbProtoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		parsed, err := parseEntityProtoFile(path)
		if err != nil {
			return fmt.Errorf("parse entity proto %s: %w", path, err)
		}
		entities = append(entities, parsed...)
		return nil
	})
	return entities, err
}

func parseEntityProtoFile(path string) ([]EntityDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	handler := reporter.NewHandler(reporter.NewReporter(
		func(err reporter.ErrorWithPos) error { return err },
		nil,
	))

	fileNode, err := parser.Parse(path, strings.NewReader(string(data)), handler)
	if err != nil {
		return nil, fmt.Errorf("proto parse error: %w", err)
	}

	var entities []EntityDef
	for _, decl := range fileNode.Decls {
		msgNode, ok := decl.(*ast.MessageNode)
		if !ok {
			continue
		}

		ent := parseEntityMessage(msgNode, path)
		if ent != nil {
			entities = append(entities, *ent)
		}
	}
	return entities, nil
}

// parseEntityMessage extracts an EntityDef from a proto message node.
// It considers any message in proto/db/ with an "id" field as an entity.
func parseEntityMessage(msg *ast.MessageNode, protoFile string) *EntityDef {
	name := string(msg.Name.AsIdentifier())

	var fields []EntityField
	var pkField string
	var pkGoType string

	for _, elem := range msg.Decls {
		fieldNode, ok := elem.(*ast.FieldNode)
		if !ok {
			continue
		}

		fieldName := string(fieldNode.Name.AsIdentifier())
		protoType := extractFieldType(fieldNode)
		goType := entityProtoTypeToGoType(protoType)
		goName := naming.ToPascalCase(fieldName)

		ef := EntityField{
			Name:      fieldName,
			GoName:    goName,
			ProtoType: protoType,
			GoType:    goType,
		}

		// Detect primary key (first field named "id")
		if fieldName == "id" && pkField == "" {
			pkField = fieldName
			pkGoType = goType
		}

		// Detect foreign keys: fields ending in _id (but not "id" itself)
		if strings.HasSuffix(fieldName, "_id") && fieldName != "id" {
			ef.IsFK = true
			refEntity := strings.TrimSuffix(fieldName, "_id")
			ef.FKTable = naming.Pluralize(refEntity)
		}

		fields = append(fields, ef)
	}

	// Only treat as entity if it has an "id" field
	if pkField == "" {
		return nil
	}

	return &EntityDef{
		Name:      name,
		TableName: naming.Pluralize(naming.ToSnakeCase(name)),
		PkField:   pkField,
		PkGoType:  pkGoType,
		Fields:    fields,
		ProtoFile: protoFile,
	}
}

// extractFieldType returns the type name from a field node.
func extractFieldType(f *ast.FieldNode) string {
	if f.FldType == nil {
		return "string"
	}
	return string(f.FldType.AsIdentifier())
}

// entityProtoTypeToGoType converts a proto type name to a Go type for entity fields.
// Extends the base protoTypeToGoType with additional proto scalar types.
func entityProtoTypeToGoType(protoType string) string {
	switch protoType {
	case "sint32", "sfixed32":
		return "int32"
	case "sint64", "sfixed64":
		return "int64"
	case "uint32", "fixed32":
		return "uint32"
	case "uint64", "fixed64":
		return "uint64"
	case "bytes":
		return "[]byte"
	case "google.protobuf.Timestamp":
		return "timestamppb.Timestamp"
	default:
		return protoTypeToGoType(protoType)
	}
}

