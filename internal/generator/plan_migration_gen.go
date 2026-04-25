package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// protoTypeToSQL maps proto field types to Postgres-compatible SQL types.
var protoTypeToSQL = map[string]string{
	"string":                    "TEXT",
	"int32":                     "INTEGER",
	"int64":                     "BIGINT",
	"bool":                      "BOOLEAN",
	"float":                     "REAL",
	"double":                    "DOUBLE PRECISION",
	"bytes":                     "BYTEA",
	"google.protobuf.Timestamp": "TIMESTAMPTZ",
	"timestamp":                 "TIMESTAMPTZ",
}

// GeneratePlanMigrations generates db/migrations/00001_init.up.sql and
// db/migrations/00001_init.down.sql from plan entities.
func GeneratePlanMigrations(root string, entities []config.PlanEntity) error {
	if len(entities) == 0 {
		return nil
	}

	migDir := filepath.Join(root, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		return fmt.Errorf("create db/migrations directory: %w", err)
	}

	upSQL := buildUpMigration(entities)
	downSQL := buildDownMigration(entities)

	upPath := filepath.Join(migDir, "00001_init.up.sql")
	downPath := filepath.Join(migDir, "00001_init.down.sql")

	if err := os.WriteFile(upPath, []byte(upSQL), 0644); err != nil {
		return fmt.Errorf("write up migration: %w", err)
	}
	if err := os.WriteFile(downPath, []byte(downSQL), 0644); err != nil {
		return fmt.Errorf("write down migration: %w", err)
	}

	return nil
}

// buildUpMigration generates CREATE TABLE statements for all entities.
func buildUpMigration(entities []config.PlanEntity) string {
	var sb strings.Builder

	for i, ent := range entities {
		if i > 0 {
			sb.WriteString("\n")
		}

		tableName := resolveTableName(ent)
		sb.WriteString(fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n", tableName))

		// Collect column definitions.
		var colDefs []string

		// Auto-add an id primary key column if none is defined.
		hasPK := false
		for _, f := range ent.Fields {
			if f.PrimaryKey || f.Name == "id" {
				hasPK = true
				break
			}
		}
		if !hasPK {
			colDefs = append(colDefs, "    id UUID PRIMARY KEY DEFAULT gen_random_uuid()")
		}

		for _, f := range ent.Fields {
			colDefs = append(colDefs, buildColumnDef(f))
		}

		// Auto-generated timestamp fields.
		if ent.Timestamps {
			colDefs = append(colDefs, "    created_at TIMESTAMPTZ NOT NULL DEFAULT now()")
			colDefs = append(colDefs, "    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()")
		}

		// Auto-generated soft delete field.
		if ent.SoftDelete {
			colDefs = append(colDefs, "    deleted_at TIMESTAMPTZ")
		}

		sb.WriteString(strings.Join(colDefs, ",\n"))
		sb.WriteString("\n);\n")

		// Create indexes for tenant key fields.
		for _, f := range ent.Fields {
			if f.TenantKey {
				colName := f.Name
				sb.WriteString(fmt.Sprintf("\nCREATE INDEX IF NOT EXISTS idx_%s_%s ON %s(%s);\n", tableName, colName, tableName, colName))
			}
		}
	}

	return sb.String()
}

// buildColumnDef generates a single column definition line.
func buildColumnDef(f config.PlanEntityField) string {
	colName := f.Name
	sqlType := mapProtoToSQL(f.Type)

	parts := []string{fmt.Sprintf("    %s %s", colName, sqlType)}

	if f.PrimaryKey {
		parts = append(parts, "PRIMARY KEY")
	}
	if f.NotNull || f.TenantKey {
		parts = append(parts, "NOT NULL")
	}
	if f.Unique {
		parts = append(parts, "UNIQUE")
	}
	if f.Default != "" {
		// SQL expressions (parenthesized or known keywords) are emitted unquoted;
		// literal values are single-quoted with proper escaping.
		switch {
		case strings.HasPrefix(f.Default, "("):
			parts = append(parts, fmt.Sprintf("DEFAULT %s", f.Default))
		case strings.EqualFold(f.Default, "CURRENT_TIMESTAMP"),
			strings.EqualFold(f.Default, "CURRENT_DATE"),
			strings.EqualFold(f.Default, "CURRENT_TIME"),
			strings.EqualFold(f.Default, "NULL"),
			strings.EqualFold(f.Default, "TRUE"),
			strings.EqualFold(f.Default, "FALSE"):
			parts = append(parts, fmt.Sprintf("DEFAULT %s", f.Default))
		default:
			escaped := strings.ReplaceAll(f.Default, "'", "''")
			parts = append(parts, fmt.Sprintf("DEFAULT '%s'", escaped))
		}
	}
	if f.References != "" {
		// Convert "users.id" to "REFERENCES users(id)"
		refParts := strings.SplitN(f.References, ".", 2)
		if len(refParts) == 2 {
			parts = append(parts, fmt.Sprintf("REFERENCES %s(%s)", refParts[0], refParts[1]))
		}
	}

	return strings.Join(parts, " ")
}

// buildDownMigration generates DROP TABLE statements in reverse order.
func buildDownMigration(entities []config.PlanEntity) string {
	var sb strings.Builder

	// Drop in reverse order to handle foreign key dependencies.
	for i := len(entities) - 1; i >= 0; i-- {
		tableName := resolveTableName(entities[i])
		sb.WriteString(fmt.Sprintf("DROP TABLE IF EXISTS %s;\n", tableName))
	}

	return sb.String()
}

// resolveTableName returns the table name for an entity: uses TableName if set,
// otherwise pluralizes + snake_cases the entity name.
func resolveTableName(ent config.PlanEntity) string {
	if ent.TableName != "" {
		return ent.TableName
	}
	return naming.Pluralize(naming.ToSnakeCase(ent.Name))
}

// mapProtoToSQL converts a proto type to a SQL type.
func mapProtoToSQL(protoType string) string {
	// Handle repeated types → Postgres arrays (e.g. "repeated string" → "TEXT[]")
	if base, ok := strings.CutPrefix(protoType, "repeated "); ok {
		if sqlType, ok := protoTypeToSQL[base]; ok {
			return sqlType + "[]"
		}
		return "TEXT[]"
	}
	if sqlType, ok := protoTypeToSQL[protoType]; ok {
		return sqlType
	}
	return "TEXT"
}