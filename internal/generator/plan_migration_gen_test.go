package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestGeneratePlanMigrations_Basic(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Organization",
			Timestamps: true,
			SoftDelete: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "org_id", Type: "string", NotNull: true, TenantKey: true},
				{Name: "name", Type: "string", NotNull: true},
				{Name: "slug", Type: "string", Unique: true},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("GeneratePlanMigrations() error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile up error = %v", err)
	}
	up := string(upContent)

	// CREATE TABLE
	if !strings.Contains(up, "CREATE TABLE IF NOT EXISTS organizations") {
		t.Error("missing CREATE TABLE statement")
	}

	// Primary key
	if !strings.Contains(up, "id TEXT PRIMARY KEY") {
		t.Error("missing primary key constraint on id")
	}

	// NOT NULL
	if !strings.Contains(up, "org_id TEXT NOT NULL") {
		t.Error("missing NOT NULL on org_id")
	}
	if !strings.Contains(up, "name TEXT NOT NULL") {
		t.Error("missing NOT NULL on name")
	}

	// UNIQUE
	if !strings.Contains(up, "slug TEXT UNIQUE") {
		t.Error("missing UNIQUE on slug")
	}

	// Timestamps
	if !strings.Contains(up, "created_at TIMESTAMPTZ NOT NULL DEFAULT now()") {
		t.Error("missing created_at timestamp")
	}
	if !strings.Contains(up, "updated_at TIMESTAMPTZ NOT NULL DEFAULT now()") {
		t.Error("missing updated_at timestamp")
	}

	// Soft delete
	if !strings.Contains(up, "deleted_at TIMESTAMPTZ") {
		t.Error("missing deleted_at soft delete field")
	}

	// Tenant key index
	if !strings.Contains(up, "CREATE INDEX IF NOT EXISTS idx_organizations_org_id ON organizations(org_id)") {
		t.Error("missing tenant key index")
	}
}

func TestGeneratePlanMigrations_DownDropsInReverse(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:   "User",
			Fields: []config.PlanEntityField{{Name: "id", Type: "string", PrimaryKey: true}},
		},
		{
			Name:   "Post",
			Fields: []config.PlanEntityField{{Name: "id", Type: "string", PrimaryKey: true}},
		},
		{
			Name:   "Comment",
			Fields: []config.PlanEntityField{{Name: "id", Type: "string", PrimaryKey: true}},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	downContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.down.sql"))
	if err != nil {
		t.Fatalf("ReadFile down error = %v", err)
	}
	down := string(downContent)

	// Tables should be dropped in reverse order
	commentIdx := strings.Index(down, "DROP TABLE IF EXISTS comments")
	postIdx := strings.Index(down, "DROP TABLE IF EXISTS posts")
	userIdx := strings.Index(down, "DROP TABLE IF EXISTS users")

	if commentIdx == -1 || postIdx == -1 || userIdx == -1 {
		t.Fatalf("missing DROP TABLE statements in down migration:\n%s", down)
	}

	if commentIdx >= postIdx {
		t.Error("comments should be dropped before posts (reverse order)")
	}
	if postIdx >= userIdx {
		t.Error("posts should be dropped before users (reverse order)")
	}
}

func TestGeneratePlanMigrations_ForeignKeyReferences(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Comment",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "post_id", Type: "string", NotNull: true, References: "posts.id"},
				{Name: "user_id", Type: "string", NotNull: true, References: "users.id"},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	up := string(upContent)

	if !strings.Contains(up, "REFERENCES posts(id)") {
		t.Error("missing foreign key reference to posts.id")
	}
	if !strings.Contains(up, "REFERENCES users(id)") {
		t.Error("missing foreign key reference to users.id")
	}
}

func TestGeneratePlanMigrations_TableNameOverride(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:      "UserProfile",
			TableName: "profiles",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	up := string(upContent)

	if !strings.Contains(up, "CREATE TABLE IF NOT EXISTS profiles") {
		t.Error("table name override not applied")
	}
	if strings.Contains(up, "user_profiles") {
		t.Error("should use override, not derived table name")
	}
}

func TestGeneratePlanMigrations_TypeMapping(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "TypeTest",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "count", Type: "int32"},
				{Name: "big_count", Type: "int64"},
				{Name: "active", Type: "bool"},
				{Name: "score", Type: "float"},
				{Name: "precise", Type: "double"},
				{Name: "data", Type: "bytes"},
				{Name: "event_time", Type: "google.protobuf.Timestamp"},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	up := string(upContent)

	checks := map[string]string{
		"id TEXT":                  "string -> TEXT",
		"count INTEGER":           "int32 -> INTEGER",
		"big_count BIGINT":        "int64 -> BIGINT",
		"active BOOLEAN":          "bool -> BOOLEAN",
		"score REAL":              "float -> REAL",
		"precise DOUBLE PRECISION": "double -> DOUBLE PRECISION",
		"data BYTEA":              "bytes -> BYTEA",
		"event_time TIMESTAMPTZ":  "Timestamp -> TIMESTAMPTZ",
	}

	for substr, desc := range checks {
		if !strings.Contains(up, substr) {
			t.Errorf("type mapping %s: expected %q in output", desc, substr)
		}
	}
}

func TestGeneratePlanMigrations_DefaultValue(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Setting",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "enabled", Type: "bool", Default: "false"},
				{Name: "priority", Type: "int32", Default: "0"},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	up := string(upContent)

	// Boolean keywords are emitted unquoted (proper SQL).
	if !strings.Contains(up, "DEFAULT false") {
		t.Error("missing DEFAULT for enabled field")
	}
	if !strings.Contains(up, "DEFAULT '0'") {
		t.Error("missing DEFAULT for priority field")
	}
}

func TestGeneratePlanMigrations_NoTimestampsNoSoftDelete(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Tag",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "label", Type: "string", NotNull: true},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	up := string(upContent)

	if strings.Contains(up, "created_at") {
		t.Error("should not contain created_at when timestamps=false")
	}
	if strings.Contains(up, "updated_at") {
		t.Error("should not contain updated_at when timestamps=false")
	}
	if strings.Contains(up, "deleted_at") {
		t.Error("should not contain deleted_at when soft_delete=false")
	}
}

func TestGeneratePlanMigrations_AutoIDWhenNoPK(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Widget",
			Fields: []config.PlanEntityField{
				{Name: "name", Type: "string", NotNull: true},
				{Name: "color", Type: "string"},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	up := string(upContent)

	// Should have auto-generated UUID id column with PRIMARY KEY
	if !strings.Contains(up, "id UUID PRIMARY KEY DEFAULT gen_random_uuid()") {
		t.Error("missing auto-generated UUID id PRIMARY KEY column")
	}

	// id should appear before name
	idIdx := strings.Index(up, "id UUID PRIMARY KEY")
	nameIdx := strings.Index(up, "name TEXT")
	if idIdx >= nameIdx {
		t.Error("id column should appear before name column")
	}
}

func TestGeneratePlanMigrations_ExplicitPKNotDuplicated(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Counter",
			Fields: []config.PlanEntityField{
				{Name: "counter_id", Type: "int64", PrimaryKey: true},
				{Name: "value", Type: "int32", NotNull: true},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	up := string(upContent)

	// Should NOT have auto-generated id column
	if strings.Contains(up, "gen_random_uuid") {
		t.Error("should not auto-add id when explicit PK exists")
	}

	// Should have the explicit PK
	if !strings.Contains(up, "counter_id BIGINT PRIMARY KEY") {
		t.Error("missing explicit counter_id PRIMARY KEY")
	}
}

func TestGeneratePlanMigrations_Empty(t *testing.T) {
	root := t.TempDir()

	if err := GeneratePlanMigrations(root, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	// db/migrations should not be created
	if _, err := os.Stat(filepath.Join(root, "db", "migrations")); !os.IsNotExist(err) {
		t.Error("db/migrations should not exist when no entities")
	}
}

func TestGeneratePlanMigrations_TenantKeyAddsNotNull(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Item",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "org_id", Type: "string", TenantKey: true},
			},
		},
	}

	if err := GeneratePlanMigrations(root, entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	upContent, err := os.ReadFile(filepath.Join(root, "db", "migrations", "00001_init.up.sql"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	up := string(upContent)

	// TenantKey should imply NOT NULL
	if !strings.Contains(up, "org_id TEXT NOT NULL") {
		t.Error("tenant key field should have NOT NULL")
	}

	// TenantKey should create an index
	if !strings.Contains(up, "CREATE INDEX IF NOT EXISTS idx_items_org_id ON items(org_id)") {
		t.Error("missing tenant key index")
	}
}