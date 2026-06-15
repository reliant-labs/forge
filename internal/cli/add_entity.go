package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/schemadef"
)

// entityFieldType describes one entry in the `forge add entity` type
// vocabulary: how a declared field projects to SQL (the schema truth)
// and, once, to the service-proto wire messages.
type entityFieldType struct {
	SQL      string // column type in the emitted migration
	Proto    string // proto3 field type ("repeated string" for arrays)
	ZeroSQL  string // DEFAULT expression for NOT NULL columns
	Repeated bool
}

// entityTypeVocab is the documented `field:type` vocabulary.
//
//	type      SQL column           proto field
//	string    TEXT                 string
//	int       BIGINT               int64
//	int64     BIGINT               int64
//	float     DOUBLE PRECISION     double
//	bool      BOOLEAN              bool
//	time      TIMESTAMPTZ          google.protobuf.Timestamp
//	[]string  TEXT[]               repeated string
//	[]int     BIGINT[]             repeated int64
//	json      JSONB                string (JSON text on the wire)
//
// SQL is plain postgres — the shadow schema (internal/schemadef) and
// pkg/testkit both apply it verbatim to real postgres.
var entityTypeVocab = map[string]entityFieldType{
	"string":   {SQL: "TEXT", Proto: "string", ZeroSQL: "''"},
	"int":      {SQL: "BIGINT", Proto: "int64", ZeroSQL: "0"},
	"int64":    {SQL: "BIGINT", Proto: "int64", ZeroSQL: "0"},
	"float":    {SQL: "DOUBLE PRECISION", Proto: "double", ZeroSQL: "0"},
	"bool":     {SQL: "BOOLEAN", Proto: "bool", ZeroSQL: "FALSE"},
	"time":     {SQL: "TIMESTAMPTZ", Proto: "google.protobuf.Timestamp"},
	"[]string": {SQL: "TEXT[]", Proto: "repeated string", ZeroSQL: "'{}'", Repeated: true},
	"[]int":    {SQL: "BIGINT[]", Proto: "repeated int64", ZeroSQL: "'{}'", Repeated: true},
	"json":     {SQL: "JSONB", Proto: "string", ZeroSQL: "'{}'"},
}

type entityField struct {
	Name string // snake_case
	Type entityFieldType
	Decl string // the type keyword as written ("[]string")
}

func newAddEntityCmd() *cobra.Command {
	var (
		serviceFlag  string
		tableFlag    string
		softDelete   bool
		noTimestamps bool
		noRPCs       bool
	)
	cmd := &cobra.Command{
		Use:   "entity <name> [field:type ...]",
		Short: "Add a database entity: emits the SQL migration (the schema truth) and scaffolds the CRUD wire contract",
		Long: `Add a database entity.

SQL is the schema language: this command writes a migration creating the
table — db/migrations/NNNNN_create_<table>.up.sql (+ .down.sql). Running
` + "`forge generate`" + ` afterwards projects the APPLIED schema into entity
structs, the ORM, CRUD wiring, and frontend pages. Evolve the entity by
writing further migrations; the projections follow the schema.

By default the matching CRUD messages and RPCs are also scaffolded into
the service proto (the one-time schema→wire projection); pass --no-rpcs
to skip that, or rerun later for a service that already has them.

Field types: string, int, int64, float, bool, time, []string, []int, json

Example:
  forge add entity bookmark url:string title:string tags:[]string done:bool`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddEntity(args[0], args[1:], addEntityOpts{
				Service:      serviceFlag,
				Table:        tableFlag,
				SoftDelete:   softDelete,
				NoTimestamps: noTimestamps,
				NoRPCs:       noRPCs,
			})
		},
	}
	cmd.Flags().StringVar(&serviceFlag, "service", "", "service whose proto receives the CRUD RPCs (default: the project's only service)")
	cmd.Flags().StringVar(&tableFlag, "table", "", "table name override (default: pluralized snake_case of <name>)")
	cmd.Flags().BoolVar(&softDelete, "soft-delete", false, "add deleted_at TIMESTAMPTZ — deletes become UPDATEs, reads filter IS NULL")
	cmd.Flags().BoolVar(&noTimestamps, "no-timestamps", false, "skip the managed created_at/updated_at columns")
	cmd.Flags().BoolVar(&noRPCs, "no-rpcs", false, "emit only the migration; do not touch the service proto")
	return cmd
}

type addEntityOpts struct {
	Service      string
	Table        string
	SoftDelete   bool
	NoTimestamps bool
	NoRPCs       bool
}

func runAddEntity(name string, fieldArgs []string, opts addEntityOpts) error {
	ctxLabel := fmt.Sprintf("forge add entity %s", name)

	if err := validateIdentifier(name); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid entity name", "",
			"use a name starting with a letter, containing letters/digits/_ (e.g. bookmark)", err)
	}

	fields, err := parseEntityFieldArgs(fieldArgs)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid field list", "",
			"declare fields as name:type — types: string, int, int64, float, bool, time, []string, []int, json", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	snake := naming.ToSnakeCase(name)
	table := opts.Table
	if table == "" {
		table = naming.Pluralize(snake)
	}
	pascal := naming.ToPascalCase(snake)

	// Refuse names colliding with managed convention columns.
	for _, f := range fields {
		switch f.Name {
		case "id", schemadef.ColCreatedAt, schemadef.ColUpdatedAt, schemadef.ColDeletedAt:
			return cliutil.UserErr(ctxLabel,
				fmt.Sprintf("field %q is managed by convention and added automatically", f.Name), "",
				"drop it from the field list (use --soft-delete for deleted_at, omit --no-timestamps for created_at/updated_at)")
		}
	}

	// Refuse when the table already exists in the applied schema.
	migDir := filepath.Join(root, "db", "migrations")
	if tables, terr := schemadef.ApplyAndIntrospect(migDir); terr == nil {
		for _, t := range tables {
			if t.Name == table {
				return cliutil.UserErr(ctxLabel,
					fmt.Sprintf("table %q already exists in the applied schema (db/migrations)", table), migDir,
					"evolve the entity by writing a new ALTER TABLE migration instead — the ORM follows the applied schema on the next `forge generate`")
			}
		}
	}

	// ── 1. the migration: SQL is the schema truth ────────────────────
	upPath, downPath, err := writeEntityMigration(migDir, table, fields, opts)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel, "write migration", migDir, "verify db/migrations is writable", err)
	}
	fmt.Printf("✅ Created %s\n", upPath)
	fmt.Printf("✅ Created %s\n", downPath)

	// ── 2. the one-time schema→wire projection (optional) ────────────
	if !opts.NoRPCs {
		protoPath, perr := resolveServiceProto(root, opts.Service)
		if perr != nil {
			fmt.Printf("⚠️  Skipping CRUD proto scaffold: %v\n", perr)
			fmt.Println("   (re-run with --service <name>, or add the messages/RPCs by hand)")
		} else if perr := injectEntityCRUDProto(protoPath, pascal, fields, opts); perr != nil {
			return cliutil.WrapUserErr(ctxLabel, "scaffold CRUD proto", protoPath,
				"add the CRUD messages/RPCs by hand or re-run with --no-rpcs", perr)
		} else {
			fmt.Printf("✅ Scaffolded CRUD messages + RPCs for %s in %s\n", pascal, protoPath)
		}
	}

	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Review the migration — it is yours; adjust constraints/defaults freely.")
	fmt.Println("  2. Run `forge generate` — the entity struct, ORM, CRUD wiring and")
	fmt.Println("     frontend pages are projected from the APPLIED schema.")
	fmt.Println("  3. Evolve with further migrations; projections follow the schema.")
	return nil
}

func parseEntityFieldArgs(args []string) ([]entityField, error) {
	var fields []entityField
	seen := map[string]bool{}
	for _, a := range args {
		nameStr, typeStr, ok := strings.Cut(a, ":")
		if !ok {
			return nil, fmt.Errorf("field %q is not name:type", a)
		}
		fname := naming.ToSnakeCase(nameStr)
		if fname == "" {
			return nil, fmt.Errorf("field %q has an empty name", a)
		}
		if seen[fname] {
			return nil, fmt.Errorf("field %q declared twice", fname)
		}
		seen[fname] = true
		ft, ok := entityTypeVocab[typeStr]
		if !ok {
			known := make([]string, 0, len(entityTypeVocab))
			for k := range entityTypeVocab {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("field %q: unknown type %q (known: %s)", fname, typeStr, strings.Join(known, ", "))
		}
		fields = append(fields, entityField{Name: fname, Type: ft, Decl: typeStr})
	}
	return fields, nil
}

// nextMigrationNumber returns the next 5-digit migration sequence number
// for dir, scanning existing NNNNN_*.sql files.
func nextMigrationNumber(dir string) int {
	maxN := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}
	for _, e := range entries {
		base := e.Name()
		if !strings.HasSuffix(base, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(base, "_")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(prefix); err == nil && n > maxN {
			maxN = n
		}
	}
	return maxN + 1
}

func writeEntityMigration(migDir, table string, fields []entityField, opts addEntityOpts) (string, string, error) {
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		return "", "", err
	}
	n := nextMigrationNumber(migDir)
	base := fmt.Sprintf("%05d_create_%s", n, table)
	upPath := filepath.Join(migDir, base+".up.sql")
	downPath := filepath.Join(migDir, base+".down.sql")

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", table)
	// String PK with the empty-id guard: empty-id rows were the
	// silent-upsert data-loss vector the lifecycle gate pins.
	b.WriteString("    id TEXT PRIMARY KEY CHECK (id <> '')")
	for _, f := range fields {
		b.WriteString(",\n")
		fmt.Fprintf(&b, "    %s %s", f.Name, f.Type.SQL)
		if f.Type.ZeroSQL != "" {
			fmt.Fprintf(&b, " NOT NULL DEFAULT %s", f.Type.ZeroSQL)
		}
	}
	if !opts.NoTimestamps {
		// Parenthesized (now()) and bare now() are both valid postgres;
		// the parens are kept for continuity with existing migrations.
		b.WriteString(",\n    created_at TIMESTAMPTZ NOT NULL DEFAULT (now())")
		b.WriteString(",\n    updated_at TIMESTAMPTZ NOT NULL DEFAULT (now())")
	}
	if opts.SoftDelete {
		b.WriteString(",\n    deleted_at TIMESTAMPTZ")
	}
	b.WriteString("\n);\n")

	if err := os.WriteFile(upPath, []byte(b.String()), 0o644); err != nil {
		return "", "", err
	}
	down := fmt.Sprintf("DROP TABLE %s;\n", table)
	if err := os.WriteFile(downPath, []byte(down), 0o644); err != nil {
		return "", "", err
	}
	return upPath, downPath, nil
}

// resolveServiceProto locates the service proto file the CRUD scaffold
// goes into. With --service it resolves that service; otherwise the
// project must have exactly one service under proto/services/.
func resolveServiceProto(root, service string) (string, error) {
	svcRoot := filepath.Join(root, "proto", "services")
	if service != "" {
		pkg := naming.ServicePackage(service)
		p := filepath.Join(svcRoot, pkg, "v1", pkg+".proto")
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("service proto not found at %s", p)
		}
		return p, nil
	}
	entries, err := os.ReadDir(svcRoot)
	if err != nil {
		return "", fmt.Errorf("no proto/services directory (%v)", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 1 {
		return "", fmt.Errorf("project has %d services; pass --service to pick one", len(dirs))
	}
	p := filepath.Join(svcRoot, dirs[0], "v1", dirs[0]+".proto")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("service proto not found at %s", p)
	}
	return p, nil
}

// injectEntityCRUDProto appends the entity's wire message + CRUD
// request/response messages to the service proto and the five CRUD RPCs
// to the service block. Skips gracefully when `message <Entity>` already
// exists (the wire contract was hand-written or scaffolded earlier).
func injectEntityCRUDProto(protoPath, entity string, fields []entityField, opts addEntityOpts) error {
	raw, err := os.ReadFile(protoPath)
	if err != nil {
		return err
	}
	content := string(raw)

	if strings.Contains(content, "message "+entity+" {") {
		fmt.Printf("ℹ️  message %s already declared in %s — leaving the wire contract as-is\n", entity, filepath.Base(protoPath))
		return nil
	}

	// Ensure required imports. forge/v1/forge.proto is load-bearing: the
	// injected RPCs carry (forge.v1.method) options, and a service proto
	// fresh from `forge add service` has no imports at all — without this
	// line every subsequent `forge generate` fails in buf with
	// "unknown extension forge.v1.method".
	for _, imp := range []string{
		"forge/v1/forge.proto",
		"google/protobuf/timestamp.proto",
		"google/protobuf/field_mask.proto",
	} {
		content = ensureProtoImport(content, imp)
	}

	// RPCs go inside the service block.
	rpcs := buildEntityCRUDRPCs(entity)
	withRPCs, err := insertIntoServiceBlock(content, rpcs)
	if err != nil {
		return err
	}

	// Messages go at the end of the file.
	msgs := buildEntityCRUDMessages(entity, fields, opts)
	out := strings.TrimRight(withRPCs, "\n") + "\n\n" + msgs

	return os.WriteFile(protoPath, []byte(out), 0o644)
}

// ensureProtoImport adds `import "<path>";` after the last existing
// import (or after the package line) when missing.
func ensureProtoImport(content, path string) string {
	if strings.Contains(content, `import "`+path+`"`) {
		return content
	}
	lines := strings.Split(content, "\n")
	insertAt := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "import ") {
			insertAt = i
		}
	}
	if insertAt == -1 {
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "package ") {
				insertAt = i
				break
			}
		}
	}
	if insertAt == -1 {
		return content
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt+1]...)
	out = append(out, `import "`+path+`";`)
	out = append(out, lines[insertAt+1:]...)
	return strings.Join(out, "\n")
}

// insertIntoServiceBlock inserts text before the closing brace of the
// first `service <Name> { ... }` block.
func insertIntoServiceBlock(content, text string) (string, error) {
	idx := strings.Index(content, "\nservice ")
	if idx < 0 {
		if strings.HasPrefix(content, "service ") {
			idx = 0
		} else {
			return "", fmt.Errorf("no service block found")
		}
	}
	open := strings.Index(content[idx:], "{")
	if open < 0 {
		return "", fmt.Errorf("malformed service block")
	}
	depth := 0
	for i := idx + open; i < len(content); i++ {
		switch content[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return content[:i] + text + content[i:], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced braces in service block")
}

func buildEntityCRUDRPCs(entity string) string {
	plural := naming.Pluralize(entity)
	var b strings.Builder
	fmt.Fprintf(&b, "\n  // %s CRUD — scaffolded by `forge add entity`; the wire contract is yours.\n", entity)
	for _, op := range []struct{ verb, name, opts string }{
		{"Create", "Create" + entity, "idempotency_key: true"},
		{"Get", "Get" + entity, ""},
		{"Update", "Update" + entity, "idempotency_key: true"},
		{"Delete", "Delete" + entity, "idempotency_key: true"},
		{"List", "List" + plural, ""},
	} {
		fmt.Fprintf(&b, "  rpc %s(%sRequest) returns (%sResponse) {\n", op.name, op.name, op.name)
		b.WriteString("    option (forge.v1.method) = {\n")
		b.WriteString("      auth_required: true\n")
		if op.opts != "" {
			fmt.Fprintf(&b, "      %s\n", op.opts)
		}
		b.WriteString("    };\n")
		b.WriteString("  }\n")
	}
	return b.String()
}

func buildEntityCRUDMessages(entity string, fields []entityField, opts addEntityOpts) string {
	plural := naming.Pluralize(entity)
	var b strings.Builder

	// The entity wire message mirrors the table's columns at scaffold
	// time; it evolves independently afterwards (the API is the wire
	// truth, migrations are the schema truth).
	fmt.Fprintf(&b, "// %s wire message — scaffolded from the same field list as the\n", entity)
	fmt.Fprintf(&b, "// db/migrations create-table migration. Evolves independently of the\n")
	fmt.Fprintf(&b, "// schema from here on.\n")
	fmt.Fprintf(&b, "message %s {\n", entity)
	n := 1
	fmt.Fprintf(&b, "  string id = %d;\n", n)
	for _, f := range fields {
		n++
		fmt.Fprintf(&b, "  %s %s = %d;\n", f.Type.Proto, f.Name, n)
	}
	if !opts.NoTimestamps {
		n++
		fmt.Fprintf(&b, "  google.protobuf.Timestamp created_at = %d;\n", n)
		n++
		fmt.Fprintf(&b, "  google.protobuf.Timestamp updated_at = %d;\n", n)
	}
	if opts.SoftDelete {
		n++
		fmt.Fprintf(&b, "  google.protobuf.Timestamp deleted_at = %d;\n", n)
	}
	b.WriteString("}\n\n")

	// Create
	fmt.Fprintf(&b, "message Create%sRequest {\n", entity)
	for i, f := range fields {
		fmt.Fprintf(&b, "  %s %s = %d;\n", f.Type.Proto, f.Name, i+1)
	}
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "message Create%sResponse {\n  %s %s = 1;\n}\n\n", entity, entity, naming.ToSnakeCase(entity))

	// Get
	fmt.Fprintf(&b, "message Get%sRequest {\n  string id = 1;\n}\n\n", entity)
	fmt.Fprintf(&b, "message Get%sResponse {\n  %s %s = 1;\n}\n\n", entity, entity, naming.ToSnakeCase(entity))

	// Update (AIP-134)
	fmt.Fprintf(&b, "message Update%sRequest {\n", entity)
	fmt.Fprintf(&b, "  %s %s = 1;\n", entity, naming.ToSnakeCase(entity))
	b.WriteString("  google.protobuf.FieldMask update_mask = 2;\n")
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "message Update%sResponse {\n  %s %s = 1;\n}\n\n", entity, entity, naming.ToSnakeCase(entity))

	// Delete
	fmt.Fprintf(&b, "message Delete%sRequest {\n  string id = 1;\n  bool hard_delete = 2;\n}\n\n", entity)
	fmt.Fprintf(&b, "message Delete%sResponse {\n  string id = 1;\n  google.protobuf.Timestamp deleted_at = 2;\n}\n\n", entity)

	// List
	fmt.Fprintf(&b, "message List%sRequest {\n", plural)
	b.WriteString("  int32 page_size = 1;\n")
	b.WriteString("  string page_token = 2;\n")
	fn := 3
	hasText := false
	for _, f := range fields {
		if f.Decl == "string" {
			hasText = true
		}
	}
	if hasText {
		fmt.Fprintf(&b, "  optional string search = %d;\n", fn)
		fn++
	}
	for _, f := range fields {
		if f.Decl == "bool" {
			fmt.Fprintf(&b, "  optional bool %s = %d;\n", f.Name, fn)
			fn++
		}
	}
	fmt.Fprintf(&b, "  string order_by = %d;\n", fn)
	fn++
	fmt.Fprintf(&b, "  bool descending = %d;\n", fn)
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "message List%sResponse {\n", plural)
	fmt.Fprintf(&b, "  repeated %s %s = 1;\n", entity, naming.Pluralize(naming.ToSnakeCase(entity)))
	b.WriteString("  string next_page_token = 2;\n")
	b.WriteString("  int32 total_count = 3;\n")
	b.WriteString("}\n")

	return b.String()
}
