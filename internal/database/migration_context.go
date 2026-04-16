package database

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MigrationOptions controls what context is included when creating a new migration.
type MigrationOptions struct {
	DSN      string // If set, introspect the live DB for schema
	ProtoDir string // Directory to scan for proto files (default: "proto/")
	FromProto bool  // Auto-generate CREATE TABLE SQL from proto messages
}

// MigrationContext holds all gathered context for generating rich migration comments.
type MigrationContext struct {
	MigrationName     string
	CreatedAt         time.Time
	ParsedTables      []ParsedTable     // Schema reconstructed from existing migrations
	LiveTables        []Table           // Schema from live DB introspection (if --dsn)
	ProtoModels       []ProtoModel      // Proto message definitions
	PreviousMigration *PreviousMigrationInfo
	MigrationHistory  []string          // List of all previous migration filenames
	SchemaDiffs       []SchemaDiffEntry // Diff between proto models and schema
}

// ParsedTable represents a table reconstructed from parsing migration SQL files.
type ParsedTable struct {
	Name    string
	Columns []ParsedColumn
}

// ParsedColumn represents a column extracted from a CREATE TABLE or ALTER TABLE statement.
type ParsedColumn struct {
	Name       string
	Type       string
	Constraint string // e.g. "PRIMARY KEY", "NOT NULL", "UNIQUE", "REFERENCES users(id)"
}

// ProtoModel represents a proto message found in a .proto file.
type ProtoModel struct {
	Name     string
	FileName string
	Fields   []ProtoModelField
}

// ProtoModelField represents a field in a proto message.
type ProtoModelField struct {
	Name      string
	ProtoType string
	Number    int
}

// PreviousMigrationInfo holds information about the most recent migration.
type PreviousMigrationInfo struct {
	Filename string
	Content  string
}

// SchemaDiffEntry describes a difference between proto models and the current schema.
type SchemaDiffEntry struct {
	Kind    string // "NEW_TABLE", "NEW_COLUMN"
	Message string
}

// --- Schema parsing from migration files ---

var (
	createTableRe = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\(`)
	alterTableRe  = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(?:ONLY\s+)?(\w+)\s+ADD\s+(?:COLUMN\s+)?(\w+)\s+(.+?)(?:;|$)`)
	dropTableRe   = regexp.MustCompile(`(?i)DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	columnLineRe  = regexp.MustCompile(`^\s*(\w+)\s+(\S+(?:\s+\S+)?)\s*(.*?)(?:,?\s*)$`)
)

// ParseMigrationsForSchema reads all .up.sql files in dir (sorted by name) and
// reconstructs a best-effort schema by parsing CREATE TABLE and ALTER TABLE statements.
func ParseMigrationsForSchema(dir string) ([]ParsedTable, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading migrations directory: %w", err)
	}

	// Collect and sort .up.sql files.
	var upFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			upFiles = append(upFiles, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(upFiles)

	tableMap := make(map[string]*ParsedTable)

	for _, path := range upFiles {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}

		parseSQL(string(content), tableMap)
	}

	// Convert map to sorted slice.
	var tables []ParsedTable
	for _, t := range tableMap {
		tables = append(tables, *t)
	}
	sort.Slice(tables, func(i, j int) bool {
		return tables[i].Name < tables[j].Name
	})

	return tables, nil
}

// parseSQL processes a single SQL file's content and updates the table map.
func parseSQL(content string, tableMap map[string]*ParsedTable) {
	// Strip SQL comment lines (-- ...) to avoid parsing context comments as SQL.
	content = stripSQLComments(content)

	// Handle DROP TABLE — remove from map.
	for _, match := range dropTableRe.FindAllStringSubmatch(content, -1) {
		delete(tableMap, match[1])
	}

	// Handle CREATE TABLE.
	parseCreateTables(content, tableMap)

	// Handle ALTER TABLE ADD COLUMN.
	for _, match := range alterTableRe.FindAllStringSubmatch(content, -1) {
		tableName := match[1]
		colName := match[2]
		colRest := strings.TrimSpace(match[3])

		t, ok := tableMap[tableName]
		if !ok {
			t = &ParsedTable{Name: tableName}
			tableMap[tableName] = t
		}

		// Parse the type and constraints from the rest.
		parts := strings.SplitN(colRest, " ", 2)
		colType := parts[0]
		constraint := ""
		if len(parts) > 1 {
			constraint = strings.TrimSpace(parts[1])
			// Clean trailing semicolons.
			constraint = strings.TrimRight(constraint, ";")
		}

		t.Columns = append(t.Columns, ParsedColumn{
			Name:       colName,
			Type:       colType,
			Constraint: constraint,
		})
	}
}

// stripSQLComments removes lines that are SQL comments (starting with --).
func stripSQLComments(content string) string {
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "--") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// parseCreateTables extracts CREATE TABLE blocks and their column definitions.
func parseCreateTables(content string, tableMap map[string]*ParsedTable) {
	matches := createTableRe.FindAllStringSubmatchIndex(content, -1)
	for _, loc := range matches {
		tableName := content[loc[2]:loc[3]]
		// Find the matching closing paren by counting parens from the opening.
		start := loc[1] // position after the opening paren
		depth := 1
		end := start
		for end < len(content) && depth > 0 {
			if content[end] == '(' {
				depth++
			} else if content[end] == ')' {
				depth--
			}
			if depth > 0 {
				end++
			}
		}

		if depth != 0 {
			continue
		}

		body := content[start:end]
		t := &ParsedTable{Name: tableName}
		t.Columns = parseColumnDefs(body)
		tableMap[tableName] = t
	}
}

// parseColumnDefs parses the body between CREATE TABLE parens into column definitions.
func parseColumnDefs(body string) []ParsedColumn {
	var columns []ParsedColumn
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		// Skip table-level constraints (PRIMARY KEY, UNIQUE, CONSTRAINT, CHECK, FOREIGN KEY).
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "PRIMARY KEY") ||
			strings.HasPrefix(upper, "UNIQUE") ||
			strings.HasPrefix(upper, "CONSTRAINT") ||
			strings.HasPrefix(upper, "CHECK") ||
			strings.HasPrefix(upper, "FOREIGN KEY") ||
			strings.HasPrefix(upper, "EXCLUDE") {
			continue
		}

		matches := columnLineRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		colName := matches[1]
		colType := matches[2]
		rest := strings.TrimSpace(matches[3])

		// Clean trailing commas.
		rest = strings.TrimRight(rest, ",")
		rest = strings.TrimSpace(rest)

		columns = append(columns, ParsedColumn{
			Name:       colName,
			Type:       colType,
			Constraint: rest,
		})
	}
	return columns
}

// --- Proto model scanning ---

var protoMessageRe = regexp.MustCompile(`^message\s+(\w+)\s*\{`)
var protoFieldRe = regexp.MustCompile(`^\s*(repeated\s+)?([\w.]+)\s+(\w+)\s*=\s*(\d+)`)

// ScanProtoModels scans the given directory recursively for .proto files and
// extracts all message definitions (not just entity-annotated ones).
func ScanProtoModels(dir string) ([]ProtoModel, error) {
	var models []ProtoModel

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip inaccessible files/dirs silently.
		}
		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		parsed, err := parseProtoMessages(path)
		if err != nil {
			return nil // Skip unparseable files.
		}
		models = append(models, parsed...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return models, nil
}

// parseProtoMessages parses all messages from a single .proto file.
func parseProtoMessages(path string) ([]ProtoModel, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	relPath := filepath.Base(path)

	var models []ProtoModel
	var current *ProtoModel
	var depth int

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Skip comment-only lines.
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		if current == nil {
			if matches := protoMessageRe.FindStringSubmatch(trimmed); matches != nil {
				current = &ProtoModel{
					Name:     matches[1],
					FileName: relPath,
				}
				depth = 1
			}
			continue
		}

		depth += strings.Count(trimmed, "{")
		depth -= strings.Count(trimmed, "}")

		if depth <= 0 {
			models = append(models, *current)
			current = nil
			depth = 0
			continue
		}

		// Only parse fields at top level of the message (depth == 1).
		if depth == 1 {
			if matches := protoFieldRe.FindStringSubmatch(trimmed); matches != nil {
				protoType := matches[2]
				if matches[1] != "" {
					protoType = "repeated " + protoType
				}
				fieldNum := 0
				fmt.Sscanf(matches[4], "%d", &fieldNum)
				current.Fields = append(current.Fields, ProtoModelField{
					Name:      matches[3],
					ProtoType: protoType,
					Number:    fieldNum,
				})
			}
		}
	}

	// If we're still inside a message at EOF, add it anyway.
	if current != nil {
		models = append(models, *current)
	}

	return models, scanner.Err()
}

// --- Schema diff ---

// ComputeSchemaDiff compares proto models against parsed tables to find
// new tables and new columns that exist in proto but not in the schema.
func ComputeSchemaDiff(tables []ParsedTable, models []ProtoModel) []SchemaDiffEntry {
	tableSet := make(map[string]*ParsedTable)
	for i := range tables {
		tableSet[tables[i].Name] = &tables[i]
	}

	var diffs []SchemaDiffEntry
	for _, model := range models {
		tableName := protoMessageToTableName(model.Name)
		t, exists := tableSet[tableName]
		if !exists {
			diffs = append(diffs, SchemaDiffEntry{
				Kind:    "NEW_TABLE",
				Message: fmt.Sprintf("%s has no corresponding table %q", model.Name, tableName),
			})
			continue
		}

		// Check for new columns.
		colSet := make(map[string]bool)
		for _, c := range t.Columns {
			colSet[c.Name] = true
		}
		for _, f := range model.Fields {
			colName := protoFieldToColumnName(f.Name)
			if !colSet[colName] {
				diffs = append(diffs, SchemaDiffEntry{
					Kind:    "NEW_COLUMN",
					Message: fmt.Sprintf("%s.%s exists in proto but not in table %q", model.Name, f.Name, tableName),
				})
			}
		}
	}

	return diffs
}

// protoMessageToTableName converts "UserPreference" to "user_preferences" (snake_case, pluralized).
func protoMessageToTableName(name string) string {
	// Convert PascalCase to snake_case.
	var result []rune
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			result = append(result, '_')
		}
		result = append(result, r)
	}
	snake := strings.ToLower(string(result))

	// Simple pluralization: add 's' if not already ending in 's'.
	if !strings.HasSuffix(snake, "s") {
		snake += "s"
	}
	return snake
}

// protoFieldToColumnName converts a proto field name to a column name (already snake_case in proto).
func protoFieldToColumnName(name string) string {
	return strings.ToLower(name)
}

// --- Previous migration ---

// GetPreviousMigration reads the most recent .up.sql file from the migrations dir.
func GetPreviousMigration(dir string) (*PreviousMigrationInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var upFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			upFiles = append(upFiles, e.Name())
		}
	}

	if len(upFiles) == 0 {
		return nil, nil
	}

	sort.Strings(upFiles)
	latest := upFiles[len(upFiles)-1]

	content, err := os.ReadFile(filepath.Join(dir, latest))
	if err != nil {
		return nil, err
	}

	return &PreviousMigrationInfo{
		Filename: latest,
		Content:  string(content),
	}, nil
}

// GetMigrationHistory returns a sorted list of all migration filenames (just .up.sql).
func GetMigrationHistory(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// --- Context comment generation ---

// GenerateContextComment builds the full SQL comment block for a new migration.
func GenerateContextComment(ctx *MigrationContext) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("-- Migration: %s\n", ctx.MigrationName))
	sb.WriteString(fmt.Sprintf("-- Created: %s\n", ctx.CreatedAt.Format(time.RFC3339)))
	sb.WriteString("--\n")

	// Schema section.
	hasParsedSchema := len(ctx.ParsedTables) > 0
	hasLiveSchema := len(ctx.LiveTables) > 0

	if hasLiveSchema {
		sb.WriteString("-- === CURRENT SCHEMA (from database) ===\n")
		for _, t := range ctx.LiveTables {
			writeTableComment(&sb, t.Name, liveTableToColumns(t))
		}
		sb.WriteString("--\n")
	} else if hasParsedSchema {
		sb.WriteString("-- === CURRENT SCHEMA (from existing migrations) ===\n")
		for _, t := range ctx.ParsedTables {
			writeParsedTableComment(&sb, t)
		}
		sb.WriteString("--\n")
	}

	// Proto models section.
	if len(ctx.ProtoModels) > 0 {
		sb.WriteString("-- === PROTO MODELS (potential new tables/columns) ===\n")
		for _, m := range ctx.ProtoModels {
			sb.WriteString(fmt.Sprintf("-- message %s {\n", m.Name))
			for _, f := range m.Fields {
				sb.WriteString(fmt.Sprintf("--   %s %s = %d;\n", f.ProtoType, f.Name, f.Number))
			}
			sb.WriteString("-- }\n")
		}
		sb.WriteString("--\n")
	}

	// Previous migration.
	if ctx.PreviousMigration != nil {
		sb.WriteString(fmt.Sprintf("-- === PREVIOUS MIGRATION (%s) ===\n", ctx.PreviousMigration.Filename))
		lines := strings.Split(strings.TrimSpace(ctx.PreviousMigration.Content), "\n")
		for _, line := range lines {
			sb.WriteString(fmt.Sprintf("-- %s\n", line))
		}
		sb.WriteString("--\n")
	}

	// Migration history.
	if len(ctx.MigrationHistory) > 0 {
		sb.WriteString("-- === MIGRATION HISTORY ===\n")
		for _, name := range ctx.MigrationHistory {
			sb.WriteString(fmt.Sprintf("-- %s\n", name))
		}
		sb.WriteString("--\n")
	}

	// Schema diff.
	if len(ctx.SchemaDiffs) > 0 {
		sb.WriteString("-- === DIFF (proto vs schema) ===\n")
		for _, d := range ctx.SchemaDiffs {
			sb.WriteString(fmt.Sprintf("-- %s: %s\n", d.Kind, d.Message))
		}
		sb.WriteString("--\n")
	}

	sb.WriteString("-- Write your migration SQL below:\n\n")

	return sb.String()
}

type commentColumn struct {
	name       string
	typeName   string
	constraint string
}

func liveTableToColumns(t Table) []commentColumn {
	var cols []commentColumn
	for _, c := range t.Columns {
		constraint := ""
		var parts []string
		if c.IsPrimary {
			parts = append(parts, "PRIMARY KEY")
		}
		if !c.Nullable {
			parts = append(parts, "NOT NULL")
		}
		if c.Default != "" {
			parts = append(parts, "DEFAULT "+c.Default)
		}
		constraint = strings.Join(parts, " ")

		typeName := c.Type
		if c.UDTName != "" && c.UDTName != strings.ToLower(c.Type) {
			typeName = strings.ToUpper(c.UDTName)
		} else {
			typeName = strings.ToUpper(c.Type)
		}

		cols = append(cols, commentColumn{
			name:       c.Name,
			typeName:   typeName,
			constraint: constraint,
		})
	}
	return cols
}

func writeTableComment(sb *strings.Builder, name string, cols []commentColumn) {
	sb.WriteString(fmt.Sprintf("--\n-- TABLE %s (\n", name))
	for i, c := range cols {
		line := fmt.Sprintf("--   %s %s", c.name, c.typeName)
		if c.constraint != "" {
			line += " " + c.constraint
		}
		if i < len(cols)-1 {
			line += ","
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("-- );\n")
}

func writeParsedTableComment(sb *strings.Builder, t ParsedTable) {
	sb.WriteString(fmt.Sprintf("--\n-- TABLE %s (\n", t.Name))
	for i, c := range t.Columns {
		line := fmt.Sprintf("--   %s %s", c.Name, strings.ToUpper(c.Type))
		if c.Constraint != "" {
			line += " " + c.Constraint
		}
		if i < len(t.Columns)-1 {
			line += ","
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("-- );\n")
}

// --- Proto to CREATE TABLE ---

// ProtoToSQL maps a proto type to a PostgreSQL type.
func ProtoToSQL(protoType string) string {
	switch strings.ToLower(protoType) {
	case "string":
		return "TEXT"
	case "int32", "sint32", "uint32", "fixed32", "sfixed32":
		return "INTEGER"
	case "int64", "sint64", "uint64", "fixed64", "sfixed64":
		return "BIGINT"
	case "float":
		return "REAL"
	case "double":
		return "DOUBLE PRECISION"
	case "bool":
		return "BOOLEAN"
	case "bytes":
		return "BYTEA"
	case "google.protobuf.timestamp":
		return "TIMESTAMPTZ"
	default:
		return "TEXT"
	}
}

// ProtoToCreateTable generates a CREATE TABLE SQL statement from a proto model.
// It always adds id, created_at, updated_at columns.
func ProtoToCreateTable(model ProtoModel) string {
	tableName := protoMessageToTableName(model.Name)
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("CREATE TABLE %s (\n", tableName))
	sb.WriteString("    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),\n")

	for _, f := range model.Fields {
		colName := protoFieldToColumnName(f.Name)
		// Skip id, created_at, updated_at — we add those ourselves.
		if colName == "id" || colName == "created_at" || colName == "updated_at" {
			continue
		}
		sqlType := ProtoToSQL(f.ProtoType)
		sb.WriteString(fmt.Sprintf("    %s %s NOT NULL,\n", colName, sqlType))
	}

	sb.WriteString("    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),\n")
	sb.WriteString("    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()\n")
	sb.WriteString(");\n")

	return sb.String()
}

// GatherMigrationContext collects all available context for a new migration.
func GatherMigrationContext(name, migDir string, opts MigrationOptions) (*MigrationContext, error) {
	ctx := &MigrationContext{
		MigrationName: name,
		CreatedAt:     time.Now().UTC(),
	}

	// Parse existing migrations for schema.
	parsed, err := ParseMigrationsForSchema(migDir)
	if err != nil {
		// Non-fatal: we'll just skip this section.
		parsed = nil
	}
	ctx.ParsedTables = parsed

	// Live DB introspection.
	if opts.DSN != "" {
		db, err := ConnectDB(opts.DSN)
		if err == nil {
			defer db.Close()
			tables, err := IntrospectSchema(db, "")
			if err == nil {
				ctx.LiveTables = tables
			}
		}
	}

	// Proto model scanning.
	protoDir := opts.ProtoDir
	if protoDir == "" {
		protoDir = "proto/"
	}
	models, err := ScanProtoModels(protoDir)
	if err != nil {
		// Non-fatal: proto dir may not exist.
		models = nil
	}
	ctx.ProtoModels = models

	// Previous migration.
	prev, err := GetPreviousMigration(migDir)
	if err == nil {
		ctx.PreviousMigration = prev
	}

	// Migration history.
	history, err := GetMigrationHistory(migDir)
	if err == nil {
		ctx.MigrationHistory = history
	}

	// Schema diff (use parsed tables since they don't require a DB connection).
	schemaTables := ctx.ParsedTables
	if len(ctx.LiveTables) > 0 {
		// If we have live tables, convert them to parsed format for diffing.
		schemaTables = liveToParsed(ctx.LiveTables)
	}
	if len(schemaTables) > 0 && len(ctx.ProtoModels) > 0 {
		ctx.SchemaDiffs = ComputeSchemaDiff(schemaTables, ctx.ProtoModels)
	}

	return ctx, nil
}

// liveToParsed converts live Table objects to ParsedTable for schema diffing.
func liveToParsed(tables []Table) []ParsedTable {
	var result []ParsedTable
	for _, t := range tables {
		pt := ParsedTable{Name: t.Name}
		for _, c := range t.Columns {
			pt.Columns = append(pt.Columns, ParsedColumn{
				Name: c.Name,
				Type: c.Type,
			})
		}
		result = append(result, pt)
	}
	return result
}