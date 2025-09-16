// atlas-provider-forge reads proto files annotated with EntityOptions and
// FieldOptions, and outputs SQL DDL (CREATE TABLE, CREATE INDEX) to stdout.
//
// Atlas consumes this via `data "external_schema"` in atlas.hcl:
//
//	data "external_schema" "forge" {
//	  program = ["go", "run", "./cmd/atlas-provider-forge", "--dialect=postgres", "--proto-dir=proto/db"]
//	}
//
// Usage:
//
//	atlas-provider-forge --dialect=postgres --proto-dir=proto/db
//	atlas-provider-forge --dialect=sqlite  --proto-dir=proto/db
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/reliant-labs/forge/pkg/dialects/sqlite"
	"github.com/reliant-labs/forge/pkg/orm"
)



var (
	dialectFlag  = flag.String("dialect", "postgres", "SQL dialect: postgres or sqlite")
	protoDirFlag = flag.String("proto-dir", "proto/db", "Directory containing proto files to scan")
)

func main() {
	flag.Parse()

	dialect := resolveDialect(*dialectFlag)

	entities, err := scanProtoDir(*protoDirFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error scanning proto dir: %v\n", err)
		os.Exit(1)
	}

	ddl := generateDDL(entities, dialect)
	fmt.Print(ddl)
}

func resolveDialect(name string) orm.Dialect {
	switch strings.ToLower(name) {
	case "sqlite":
		return &sqlite.Dialect{}
	default:
		return &orm.PostgresDialect{}
	}
}

// entityDef holds parsed entity info from a proto file descriptor.
type entityDef struct {
	tableName  string
	softDelete bool
	timestamps bool
	fields     []columnDef
	indexes    []indexDef
}

type columnDef struct {
	name         string
	protoType    string
	isPK         bool
	notNull      bool
	unique       bool
	defaultValue string
	columnType   string // override
	references   string
	autoInc      bool
}

type indexDef struct {
	name   string
	fields []string
	unique bool
}

// scanProtoDir walks the proto directory looking for .proto files and
// parses them using FileDescriptorProto (reading the text format).
// Since we don't have a full proto compiler, we use a simplified approach:
// read .proto files, compile them with the protoc compiler descriptor,
// or parse them manually.
//
// For the Atlas provider, the simplest reliable approach is to parse
// FileDescriptorSet from serialized descriptors. In practice, users
// will either:
// 1. Pipe `buf build -o -` | atlas-provider-forge --descriptor-set
// 2. Use this tool with --proto-dir and have it call buf/protoc
//
// This implementation reads .proto files and does simple text parsing
// of the annotations. For production, it would use buf to compile descriptors.
func scanProtoDir(dir string) ([]entityDef, error) {
	var entities []entityDef

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		fileEntities, parseErr := parseProtoFile(path)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", path, parseErr)
			return nil
		}
		entities = append(entities, fileEntities...)
		return nil
	})

	return entities, err
}

// parseProtoFile reads a .proto file and extracts entity definitions.
// This uses a basic text-parsing approach for the annotations.
func parseProtoFile(path string) ([]entityDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	return extractEntities(content)
}

// extractEntities does simple text-based extraction of entity annotations.
// This is a pragmatic approach — the protoc plugin uses proper protobuf
// descriptors, while this tool does text parsing for the Atlas use case.
func extractEntities(content string) ([]entityDef, error) {
	var entities []entityDef
	lines := strings.Split(content, "\n")

	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])

		// Look for message declarations.
		if strings.HasPrefix(line, "message ") && strings.HasSuffix(line, "{") {
			msgName := strings.TrimSuffix(strings.TrimPrefix(line, "message "), " {")
			msgName = strings.TrimSpace(msgName)

			// Collect the message body.
			braceCount := 1
			start := i + 1
			i++
			for i < len(lines) && braceCount > 0 {
				for _, ch := range lines[i] {
					if ch == '{' {
						braceCount++
					} else if ch == '}' {
						braceCount--
					}
				}
				i++
			}
			end := i - 1

			body := strings.Join(lines[start:end], "\n")
			ent, ok := parseMessageBody(msgName, body)
			if ok {
				entities = append(entities, ent)
			}
			continue
		}
		i++
	}

	return entities, nil
}

func parseMessageBody(msgName, body string) (entityDef, bool) {
	ent := entityDef{}

	// Look for entity_options.
	if !strings.Contains(body, "entity_options") {
		return ent, false
	}

	// Extract table_name.
	if idx := strings.Index(body, "table_name:"); idx >= 0 {
		ent.tableName = extractQuotedString(body[idx:])
	}
	if ent.tableName == "" {
		ent.tableName = inferTableName(msgName)
	}

	// Extract soft_delete.
	if strings.Contains(body, "soft_delete: true") {
		ent.softDelete = true
	}

	// Extract timestamps.
	if strings.Contains(body, "timestamps: true") {
		ent.timestamps = true
	}

	// Extract indexes from the entity_options block.
	ent.indexes = extractIndexes(body)

	// Extract fields.
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if col, ok := parseFieldLine(line); ok {
			ent.fields = append(ent.fields, col)
		}
	}

	return ent, true
}

func parseFieldLine(line string) (columnDef, bool) {
	// Match proto field declarations: type name = number [options];
	// e.g.: string id = 1 [(forge.options.v1.field_options) = {...}];
	parts := strings.Fields(line)
	if len(parts) < 4 {
		return columnDef{}, false
	}

	// Skip option lines, comments, and index blocks.
	if strings.HasPrefix(line, "option ") || strings.HasPrefix(line, "//") ||
		strings.HasPrefix(line, "{") || strings.HasPrefix(line, "}") ||
		strings.HasPrefix(line, "indexes:") || strings.HasPrefix(line, "name:") ||
		strings.HasPrefix(line, "fields:") || strings.HasPrefix(line, "unique:") ||
		strings.HasPrefix(line, "repeated") || strings.HasPrefix(line, "table_name:") ||
		strings.HasPrefix(line, "soft_delete:") || strings.HasPrefix(line, "timestamps:") {
		return columnDef{}, false
	}

	protoType := parts[0]
	fieldName := parts[1]

	// Handle google.protobuf.Timestamp type.
	if protoType == "google.protobuf.Timestamp" {
		protoType = "google.protobuf.Timestamp"
	}

	// Check for "= N" pattern to confirm this is a field declaration.
	hasAssign := false
	for _, p := range parts {
		if p == "=" {
			hasAssign = true
			break
		}
	}
	if !hasAssign {
		return columnDef{}, false
	}

	col := columnDef{
		name:      toSnake(fieldName),
		protoType: protoType,
	}

	// Parse field options if present.
	if strings.Contains(line, "field_options") {
		optStr := line[strings.Index(line, "{"):]
		if idx := strings.Index(optStr, "}"); idx >= 0 {
			optStr = optStr[:idx+1]
		}

		if strings.Contains(optStr, "primary_key: true") {
			col.isPK = true
		}
		if strings.Contains(optStr, "not_null: true") {
			col.notNull = true
		}
		if strings.Contains(optStr, "unique: true") {
			col.unique = true
		}
		if strings.Contains(optStr, "auto_increment: true") {
			col.autoInc = true
		}

		if idx := strings.Index(optStr, "default_value:"); idx >= 0 {
			col.defaultValue = extractQuotedString(optStr[idx:])
		}
		if idx := strings.Index(optStr, "column_type:"); idx >= 0 {
			col.columnType = extractQuotedString(optStr[idx:])
		}
		if idx := strings.Index(optStr, "references:"); idx >= 0 {
			col.references = extractQuotedString(optStr[idx:])
		}
	}

	return col, true
}

func extractIndexes(body string) []indexDef {
	var indexes []indexDef

	// Find all index blocks within the entity_options.
	// They look like:
	//   indexes: [{name: "idx_foo" fields: ["a", "b"] unique: true}]
	// or multiline.
	rest := body
	for {
		idx := strings.Index(rest, "name:")
		if idx < 0 {
			break
		}
		// Check this is within an indexes context.
		rest = rest[idx:]

		var idxDef indexDef
		idxDef.name = extractQuotedString(rest)

		// Find fields.
		if fIdx := strings.Index(rest, "fields:"); fIdx >= 0 && fIdx < 200 {
			fieldStr := rest[fIdx:]
			idxDef.fields = extractStringList(fieldStr)
		}

		if strings.Contains(rest[:min(200, len(rest))], "unique: true") {
			idxDef.unique = true
		}

		if idxDef.name != "" && len(idxDef.fields) > 0 {
			indexes = append(indexes, idxDef)
		}

		rest = rest[1:]
	}

	return indexes
}

func extractQuotedString(s string) string {
	start := strings.IndexByte(s, '"')
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(s[start+1:], '"')
	if end < 0 {
		return ""
	}
	return s[start+1 : start+1+end]
}

func extractStringList(s string) []string {
	// Extract ["a", "b", "c"] style lists.
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return nil
	}
	end := strings.IndexByte(s[start:], ']')
	if end < 0 {
		return nil
	}
	inner := s[start+1 : start+end]

	var result []string
	parts := strings.Split(inner, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// protoTypeToSQL maps a proto field type to a SQL column type for the given dialect.
// This is atlas-provider-specific logic; the new ORM's MapFieldType works with
// ORM FieldType constants, not proto type strings.
func protoTypeToSQL(dialect orm.Dialect, protoType string) string {
	switch dialect.(type) {
	case *sqlite.Dialect:
		switch protoType {
		case "string":
			return "TEXT"
		case "int32", "uint32":
			return "INTEGER"
		case "int64", "uint64":
			return "INTEGER"
		case "bool":
			return "INTEGER"
		case "float", "double":
			return "REAL"
		case "bytes":
			return "BLOB"
		case "google.protobuf.Timestamp":
			return "DATETIME"
		default:
			return "TEXT"
		}
	default: // PostgreSQL
		switch protoType {
		case "string":
			return "TEXT"
		case "int32", "uint32":
			return "INTEGER"
		case "int64", "uint64":
			return "BIGINT"
		case "bool":
			return "BOOLEAN"
		case "float":
			return "REAL"
		case "double":
			return "DOUBLE PRECISION"
		case "bytes":
			return "BYTEA"
		case "google.protobuf.Timestamp":
			return "TIMESTAMPTZ"
		default:
			return "TEXT"
		}
	}
}

// generateDDL produces SQL DDL statements for all entities.
func generateDDL(entities []entityDef, dialect orm.Dialect) string {
	var sb strings.Builder

	for i, ent := range entities {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(generateCreateTable(ent, dialect))

		for _, idx := range ent.indexes {
			sb.WriteString(generateCreateIndex(ent.tableName, idx))
		}
	}

	return sb.String()
}

func generateCreateTable(ent entityDef, dialect orm.Dialect) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("CREATE TABLE %s (\n", ent.tableName))

	var colDefs []string
	var constraints []string

	for _, col := range ent.fields {
		colSQL := generateColumnDef(col, dialect)
		colDefs = append(colDefs, "  "+colSQL)

		if col.isPK {
			constraints = append(constraints, fmt.Sprintf("  PRIMARY KEY (%s)", col.name))
		}
		if col.unique {
			constraints = append(constraints, fmt.Sprintf("  UNIQUE (%s)", col.name))
		}
		if col.references != "" {
			parts := strings.SplitN(col.references, ".", 2)
			if len(parts) == 2 {
				constraints = append(constraints, fmt.Sprintf("  FOREIGN KEY (%s) REFERENCES %s(%s)", col.name, parts[0], parts[1]))
			}
		}
	}

	// Auto-generate timestamp columns when timestamps: true is set.
	if ent.timestamps {
		tsType := protoTypeToSQL(dialect, "google.protobuf.Timestamp")
		colDefs = append(colDefs, fmt.Sprintf("  created_at %s NOT NULL DEFAULT NOW()", tsType))
		colDefs = append(colDefs, fmt.Sprintf("  updated_at %s NOT NULL DEFAULT NOW()", tsType))
	}

	// Auto-generate deleted_at column when soft_delete: true is set.
	if ent.softDelete {
		tsType := protoTypeToSQL(dialect, "google.protobuf.Timestamp")
		colDefs = append(colDefs, fmt.Sprintf("  deleted_at %s", tsType))
	}

	allParts := append(colDefs, constraints...)
	sb.WriteString(strings.Join(allParts, ",\n"))
	sb.WriteString("\n);\n")

	return sb.String()
}

func generateColumnDef(col columnDef, dialect orm.Dialect) string {
	var parts []string

	parts = append(parts, col.name)

	// Column type — handle auto_increment for Postgres (SERIAL/BIGSERIAL).
	if col.columnType != "" {
		parts = append(parts, col.columnType)
	} else if col.autoInc {
		// Use serial types for Postgres, plain INTEGER for SQLite (which auto-increments INTEGER PKs).
		switch dialect.(type) {
		case *orm.PostgresDialect:
			switch col.protoType {
			case "int64", "uint64":
				parts = append(parts, "BIGSERIAL")
			default:
				parts = append(parts, "SERIAL")
			}
		default:
			parts = append(parts, protoTypeToSQL(dialect, col.protoType))
		}
	} else {
		parts = append(parts, protoTypeToSQL(dialect, col.protoType))
	}

	if col.notNull {
		parts = append(parts, "NOT NULL")
	}

	if col.defaultValue != "" {
		parts = append(parts, "DEFAULT", col.defaultValue)
	}

	return strings.Join(parts, " ")
}

func generateCreateIndex(tableName string, idx indexDef) string {
	unique := ""
	if idx.unique {
		unique = "UNIQUE "
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s);\n",
		unique,
		idx.name,
		tableName,
		strings.Join(idx.fields, ", "),
	)
}

func inferTableName(messageName string) string {
	s := toSnake(messageName)
	if strings.HasSuffix(s, "y") {
		return s[:len(s)-1] + "ies"
	}
	if strings.HasSuffix(s, "s") {
		return s + "es"
	}
	return s + "s"
}

func toSnake(s string) string {
	var result []rune
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			result = append(result, '_')
		}
		result = append(result, unicode.ToLower(r))
	}
	return string(result)
}