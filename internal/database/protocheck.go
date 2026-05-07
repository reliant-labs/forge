package database

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Diff represents a single difference between DB schema and proto definition.
type Diff struct {
	Table   string `json:"table"`
	Kind    string `json:"kind"`    // "missing_table", "missing_column", "extra_column", "type_mismatch", "nullable_mismatch"
	Column  string `json:"column,omitempty"`
	Detail  string `json:"detail"`
}

// CheckResult holds the results of a schema-to-proto comparison.
type CheckResult struct {
	Diffs   []Diff `json:"diffs"`
	Checked int    `json:"tables_checked"`
}

// IsClean returns true when no differences were found.
func (r CheckResult) IsClean() bool {
	return len(r.Diffs) == 0
}

// FormatText renders the check result as a human-readable report.
func (r CheckResult) FormatText() string {
	if r.IsClean() {
		return fmt.Sprintf("All %d table(s) match their proto definitions.\n", r.Checked)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d difference(s) across %d table(s):\n\n", len(r.Diffs), r.Checked))

	for _, d := range r.Diffs {
		switch d.Kind {
		case "missing_table":
			sb.WriteString(fmt.Sprintf("  [MISSING TABLE] %s: %s\n", d.Table, d.Detail))
		case "missing_column":
			sb.WriteString(fmt.Sprintf("  [MISSING COLUMN] %s.%s: %s\n", d.Table, d.Column, d.Detail))
		case "extra_column":
			sb.WriteString(fmt.Sprintf("  [EXTRA COLUMN] %s.%s: %s\n", d.Table, d.Column, d.Detail))
		case "type_mismatch":
			sb.WriteString(fmt.Sprintf("  [TYPE MISMATCH] %s.%s: %s\n", d.Table, d.Column, d.Detail))
		case "nullable_mismatch":
			sb.WriteString(fmt.Sprintf("  [NULLABLE MISMATCH] %s.%s: %s\n", d.Table, d.Column, d.Detail))
		default:
			sb.WriteString(fmt.Sprintf("  [%s] %s.%s: %s\n", strings.ToUpper(d.Kind), d.Table, d.Column, d.Detail))
		}
	}

	return sb.String()
}

// ProtoEntity represents a parsed proto message that maps to a DB table.
type ProtoEntity struct {
	MessageName string
	TableName   string
	Fields      []ProtoEntityField
}

// ProtoEntityField represents a parsed proto field with DB annotations.
type ProtoEntityField struct {
	Name      string
	ProtoType string
	IsPrimary bool
	NotNull   bool
}

// CompareSchemaToProtos compares DB tables against proto definitions found in protoDir.
func CompareSchemaToProtos(tables []Table, protoDir string) (CheckResult, error) {
	entities, err := ParseProtoEntities(protoDir)
	if err != nil {
		return CheckResult{}, fmt.Errorf("parsing proto files: %w", err)
	}

	return CompareSchemaToEntities(tables, entities), nil
}

// CompareSchemaToEntities compares DB tables against parsed proto entities.
// This is the pure logic function, testable without file I/O.
func CompareSchemaToEntities(tables []Table, entities []ProtoEntity) CheckResult {
	result := CheckResult{}

	// Build a lookup of proto entities by table name.
	entityByTable := make(map[string]*ProtoEntity)
	for i := range entities {
		entityByTable[entities[i].TableName] = &entities[i]
	}

	for _, table := range tables {
		result.Checked++

		entity, ok := entityByTable[table.Name]
		if !ok {
			result.Diffs = append(result.Diffs, Diff{
				Table:  table.Name,
				Kind:   "missing_table",
				Detail: fmt.Sprintf("table %q exists in DB but has no proto entity", table.Name),
			})
			continue
		}

		// Build field lookup from proto.
		protoFields := make(map[string]*ProtoEntityField)
		for i := range entity.Fields {
			protoFields[entity.Fields[i].Name] = &entity.Fields[i]
		}

		// Check each DB column against proto fields.
		for _, col := range table.Columns {
			pf, ok := protoFields[col.Name]
			if !ok {
				result.Diffs = append(result.Diffs, Diff{
					Table:  table.Name,
					Kind:   "missing_column",
					Column: col.Name,
					Detail: fmt.Sprintf("column %q exists in DB but not in proto message %s", col.Name, entity.MessageName),
				})
				continue
			}

			// Check type compatibility.
			expectedProto, _ := SQLToProtoType(col.Type, col.UDTName)
			if !protoTypesCompatible(expectedProto, pf.ProtoType) {
				result.Diffs = append(result.Diffs, Diff{
					Table:  table.Name,
					Kind:   "type_mismatch",
					Column: col.Name,
					Detail: fmt.Sprintf("DB type %q maps to proto %q but proto has %q", col.Type, expectedProto, pf.ProtoType),
				})
			}

			// Check nullable/not_null mismatch.
			dbNotNull := !col.Nullable
			if pf.NotNull != dbNotNull {
				result.Diffs = append(result.Diffs, Diff{
					Table:  table.Name,
					Kind:   "nullable_mismatch",
					Column: col.Name,
					Detail: fmt.Sprintf("DB not_null=%v but proto not_null=%v", dbNotNull, pf.NotNull),
				})
			}

			delete(protoFields, col.Name)
		}

		// Any remaining proto fields are extras not in DB.
		for name := range protoFields {
			result.Diffs = append(result.Diffs, Diff{
				Table:  table.Name,
				Kind:   "extra_column",
				Column: name,
				Detail: fmt.Sprintf("field %q exists in proto message %s but not in DB", name, entity.MessageName),
			})
		}
	}

	return result
}

// protoTypesCompatible checks if two proto type strings are compatible.
func protoTypesCompatible(expected, actual string) bool {
	// Normalize: strip "google.protobuf." prefix for comparison.
	norm := func(s string) string {
		return strings.TrimPrefix(s, "google.protobuf.")
	}
	return norm(expected) == norm(actual)
}

// ParseProtoEntities parses all .proto files in the given directory tree,
// extracting messages with entity_options annotations.
func ParseProtoEntities(dir string) ([]ProtoEntity, error) {
	var entities []ProtoEntity

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		parsed, err := parseProtoEntityFile(path)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		entities = append(entities, parsed...)
		return nil
	})

	return entities, err
}

var (
	messageRe    = regexp.MustCompile(`^message\s+(\w+)\s*\{`)
	tableNameRe  = regexp.MustCompile(`table_name:\s*"([^"]+)"`)
	fieldLineRe  = regexp.MustCompile(`^\s*(repeated\s+)?([\w.]+)\s+(\w+)\s*=\s*\d+`)
	primaryKeyRe = regexp.MustCompile(`primary_key:\s*true`)
	notNullRe    = regexp.MustCompile(`not_null:\s*true`)
	entityOptRe  = regexp.MustCompile(`entity_options`)
)

func parseProtoEntityFile(path string) ([]ProtoEntity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entities []ProtoEntity
	var current *ProtoEntity
	var depth int
	var inEntityOptions bool

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Skip comments.
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		if current == nil {
			// Look for message start.
			if matches := messageRe.FindStringSubmatch(trimmed); matches != nil {
				current = &ProtoEntity{MessageName: matches[1]}
				depth = 1
			}
			continue
		}

		// Track brace depth.
		depth += strings.Count(trimmed, "{")
		depth -= strings.Count(trimmed, "}")

		if depth <= 0 {
			// Message closed. Only keep it if it had entity_options (i.e., a table_name).
			if current.TableName != "" {
				entities = append(entities, *current)
			}
			current = nil
			depth = 0
			inEntityOptions = false
			continue
		}

		// Detect entity_options block.
		if entityOptRe.MatchString(trimmed) {
			inEntityOptions = true
		}

		if inEntityOptions {
			if matches := tableNameRe.FindStringSubmatch(trimmed); matches != nil {
				current.TableName = matches[1]
			}
			// End of entity_options when we see };
			if strings.Contains(trimmed, "};") || strings.HasSuffix(trimmed, "};") {
				inEntityOptions = false
			}
		}

		// Parse field lines (only at message depth 1, not inside nested messages).
		if depth == 1 && !inEntityOptions {
			if matches := fieldLineRe.FindStringSubmatch(trimmed); matches != nil {
				protoType := matches[2]
				if matches[1] != "" {
					protoType = "repeated " + protoType
				}
				field := ProtoEntityField{
					Name:      matches[3],
					ProtoType: protoType,
					IsPrimary: primaryKeyRe.MatchString(trimmed),
					NotNull:   notNullRe.MatchString(trimmed),
				}
				current.Fields = append(current.Fields, field)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return entities, nil
}