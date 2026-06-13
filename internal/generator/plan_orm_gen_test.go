package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestGeneratePlanORM_Basic(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Project",
			Timestamps: true,
			SoftDelete: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "org_id", Type: "string", TenantKey: true, NotNull: true},
				{Name: "name", Type: "string", NotNull: true},
				{Name: "description", Type: "string"},
				{Name: "active", Type: "bool", NotNull: true},
				{Name: "status", Type: "string", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "github.com/test/myapp", "api", entities, nil); err != nil {
		t.Fatalf("GeneratePlanORM() error = %v", err)
	}

	// Check shared file
	sharedContent, err := os.ReadFile(filepath.Join(root, "internal", "db", "orm_shared.go"))
	if err != nil {
		t.Fatalf("ReadFile orm_shared.go error = %v", err)
	}
	shared := string(sharedContent)
	if !strings.Contains(shared, "package db") {
		t.Error("orm_shared.go: missing package db")
	}
	if !strings.Contains(shared, `ormTracer = otel.Tracer("orm")`) {
		t.Error("orm_shared.go: missing ormTracer")
	}

	// Check entity file
	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "project_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile project_orm.go error = %v", err)
	}
	code := string(content)

	// Package
	if !strings.Contains(code, "package db") {
		t.Error("missing package db")
	}

	// The entity type is a REAL struct emitted here — a projection of the
	// applied schema. Nullable columns are pointers; timestamps are
	// time.Time-based, never timestamppb.
	if !strings.Contains(code, "type Project struct {") {
		t.Error("missing Project struct emission in _orm.go")
	}
	// Struct fields are gofmt-aligned; collapse whitespace before matching
	// the field declaration (name + Go type + bun tag).
	collapsedStruct := strings.Join(strings.Fields(code), " ")
	if !strings.Contains(collapsedStruct, "Id string `bun:\"id,pk\"`") {
		t.Error("Project struct should carry a bun-tagged string Id (pk) field")
	}
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "Description *string") {
		t.Error("nullable string column should be a *string struct field")
	}
	if !strings.Contains(code, "*time.Time `bun:\"created_at\"`") {
		t.Error("nullable timestamp column should be *time.Time on the struct")
	}

	// Should NOT have method-based patterns (type alias fix)
	if strings.Contains(code, "_ orm.Model") {
		t.Error("should not have orm.Model assertion (type alias incompatible)")
	}
	if strings.Contains(code, "_ orm.Scanner") {
		t.Error("should not have orm.Scanner assertion (type alias incompatible)")
	}
	if strings.Contains(code, "func (*Project) TableName()") {
		t.Error("should not have TableName method (type alias incompatible)")
	}
	if strings.Contains(code, "func (*Project) Schema()") {
		t.Error("should not have Schema method (type alias incompatible)")
	}
	if strings.Contains(code, "func (m *Project) PrimaryKey()") {
		t.Error("should not have PrimaryKey method (type alias incompatible)")
	}
	if strings.Contains(code, "func (m *Project) Values()") {
		t.Error("should not have Values method (type alias incompatible)")
	}
	if strings.Contains(code, "func (m *Project) Scan(") {
		t.Error("should not have Scan method (type alias incompatible)")
	}

	// Table name constant
	if !strings.Contains(code, `ProjectTableName = "projects"`) {
		t.Error("missing table name constant")
	}

	// Column list is EXPORTED — it doubles as the order-by/filter allowlist
	// handed to pkg/crud.
	if !strings.Contains(code, "var ProjectColumns = []string{") {
		t.Error("missing exported ProjectColumns var")
	}
	if strings.Contains(code, "var projectColumns") {
		t.Error("column list should be exported, found lowerCamel projectColumns")
	}

	// Bun scans rows directly into the struct via Model(&x).Scan(ctx) — no
	// standalone scan function, no NullTime temps, no database/sql import.
	if strings.Contains(code, "func scanProject(") {
		t.Error("Bun scans into the struct directly; should not emit scanProject")
	}
	if strings.Contains(code, "orm.NullTime") {
		t.Error("Bun scans timestamps directly; should not use orm.NullTime temps")
	}
	if strings.Contains(code, "sql.NullTime") {
		t.Error("Bun scans timestamps directly; should not use sql.NullTime")
	}
	if strings.Contains(code, `"database/sql"`) {
		t.Error("generated file should no longer import database/sql")
	}
	// No timestamppb anywhere in generated ORM code: time columns are
	// time.Time-based struct fields scanned directly by Bun.
	if strings.Contains(code, "timestamppb") {
		t.Error("generated ORM code must not reference timestamppb")
	}
	if strings.Contains(code, "dbCreatedAt.Valid") {
		t.Error("no NullTime valid-guard — Bun scans directly into the struct")
	}
	if strings.Contains(code, "entity.CreatedAt = &tCreatedAt") {
		t.Error("no NullTime temp assignment — Bun scans directly into the struct")
	}

	// CRUD functions (tenant-scoped)
	if !strings.Contains(code, "func CreateProject(ctx context.Context, db orm.Context, msg *Project, tenantID string) error {") {
		t.Error("missing CreateProject with tenantID")
	}
	if !strings.Contains(code, "func GetProjectByID(ctx context.Context, db orm.Context, id string, tenantID string) (*Project, error) {") {
		t.Error("missing GetProjectByID with tenantID")
	}
	if !strings.Contains(code, "func ListProject(ctx context.Context, db orm.Context, tenantID string, opts ...orm.QueryOption) ([]*Project, error) {") {
		t.Error("missing ListProject with tenantID")
	}
	if !strings.Contains(code, "func CountProject(ctx context.Context, db orm.Context, tenantID string, opts ...orm.QueryOption) (int64, error) {") {
		t.Error("missing CountProject with tenantID")
	}
	if !strings.Contains(code, "func UpdateProject(ctx context.Context, db orm.Context, msg *Project, tenantID string) error {") {
		t.Error("missing UpdateProject with tenantID")
	}
	if !strings.Contains(code, "func DeleteProject(ctx context.Context, db orm.Context, id string, tenantID string) error {") {
		t.Error("missing DeleteProject with tenantID")
	}

	// Tenant enforcement in Create
	if !strings.Contains(code, "msg.OrgId = tenantID") {
		t.Error("missing tenant enforcement in Create")
	}

	// Soft delete in List: Bun adds a deleted_at IS NULL WHERE clause.
	if !strings.Contains(code, `q.Where("\"deleted_at\" IS NULL")`) {
		t.Error("missing soft-delete filter in List")
	}

	// Soft delete in Delete
	if !strings.Contains(code, "soft-deletes a Project by setting deleted_at") {
		t.Error("missing soft-delete comment in Delete")
	}
	if !strings.Contains(code, "CURRENT_TIMESTAMP") {
		t.Error("missing CURRENT_TIMESTAMP in soft delete")
	}

	// ListAll (soft-delete bypass)
	if !strings.Contains(code, "func ListAllProject(") {
		t.Error("missing ListAllProject")
	}

	// Create is a plain INSERT — never an upsert. Bun builds the INSERT.
	if !strings.Contains(code, "db.Bun().NewInsert().Model(msg)") {
		t.Error("missing Bun NewInsert in Create")
	}
	if strings.Contains(code, "ON CONFLICT") {
		t.Error("Create must be a plain INSERT, found ON CONFLICT upsert")
	}

	// String-PK chokepoint: Create generates the ULID when the caller left
	// the id empty.
	if !strings.Contains(code, `"github.com/oklog/ulid/v2"`) {
		t.Error("missing ulid import for string PK generation")
	}
	if !strings.Contains(code, `if msg.Id == "" {`) || !strings.Contains(code, "msg.Id = ulid.Make().String()") {
		t.Error("missing ULID generation chokepoint in Create")
	}

	// Timestamps chokepoint: created_at/updated_at stamped in Create with
	// time.Now().UTC(). The auto-added columns are nullable → pointer
	// struct fields, so the guard is a nil check (IsZero through a nil
	// pointer would panic) and the stamp assigns through a local.
	if !strings.Contains(code, "now := time.Now().UTC()") {
		t.Error("missing timestamp stamping in Create")
	}
	if !strings.Contains(code, "if msg.CreatedAt == nil {") {
		t.Error("missing created_at zero-guard in Create")
	}
	if !strings.Contains(code, "msg.UpdatedAt = &stampUpdatedAt") {
		t.Error("missing updated_at stamping in Create")
	}

	// GetByID wraps the underlying error so callers (and forge/pkg/crud)
	// can map a miss to NotFound; the orm layer makes Bun's no-rows error
	// satisfy errors.Is(err, orm.ErrNoRows).
	if !strings.Contains(code, `fmt.Errorf("get projects by id: %w", err)`) {
		t.Error("GetProjectByID should wrap the query error")
	}

	// List/Get/Count build queries via Bun's NewSelect against the table.
	if !strings.Contains(code, `db.Bun().NewSelect().Model(&results)`) {
		t.Error("missing Bun NewSelect in List")
	}
	if strings.Contains(code, "orm.NewQueryBuilder(db,") {
		t.Error("List/Get should use Bun NewSelect, not orm.NewQueryBuilder")
	}
	if strings.Contains(code, "scanProject(rows)") {
		t.Error("Bun scans directly; should not call scanProject(rows)")
	}

	// Field constants (gofmt aligns the `=`, so collapse whitespace before
	// matching the const declaration).
	collapsed := strings.Join(strings.Fields(code), " ")
	if !strings.Contains(collapsed, `ProjectFieldId = "id"`) {
		t.Error("missing field constant for Id")
	}
	if !strings.Contains(collapsed, `ProjectFieldOrgId = "org_id"`) {
		t.Error("missing field constant for OrgId")
	}
}

func TestGeneratePlanORM_NoTenant(t *testing.T) {
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

	if err := GeneratePlanORM(root, "github.com/test/myapp", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "tag_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// No tenantID parameter
	if !strings.Contains(code, "func CreateTag(ctx context.Context, db orm.Context, msg *Tag) error {") {
		t.Error("CreateTag should not have tenantID")
	}
	if !strings.Contains(code, "func GetTagByID(ctx context.Context, db orm.Context, id string) (*Tag, error) {") {
		t.Error("GetTagByID should not have tenantID")
	}
	if !strings.Contains(code, "func ListTag(ctx context.Context, db orm.Context, opts ...orm.QueryOption) ([]*Tag, error) {") {
		t.Error("ListTag should not have tenantID")
	}
	if !strings.Contains(code, "func CountTag(ctx context.Context, db orm.Context, opts ...orm.QueryOption) (int64, error) {") {
		t.Error("CountTag should not have tenantID")
	}
	if !strings.Contains(code, "func UpdateTag(ctx context.Context, db orm.Context, msg *Tag) error {") {
		t.Error("UpdateTag should not have tenantID")
	}
	if !strings.Contains(code, "func DeleteTag(ctx context.Context, db orm.Context, id string) error {") {
		t.Error("DeleteTag should not have tenantID")
	}

	// No soft-delete
	if strings.Contains(code, "ListAllTag") {
		t.Error("ListAllTag should not exist without soft-delete")
	}
	if strings.Contains(code, "CURRENT_TIMESTAMP") {
		t.Error("should not have soft-delete logic")
	}

	// Should not import database/sql (no timestamp fields)
	if strings.Contains(code, `"database/sql"`) {
		t.Error("should not import database/sql without timestamp fields")
	}

	// Should not import time/timestamppb (no timestamp fields)
	if strings.Contains(code, "timestamppb") {
		t.Error("should not import timestamppb without timestamp fields")
	}

	// Hard delete uses Bun NewDelete (no DELETE FROM literal anymore).
	if !strings.Contains(code, `db.Bun().NewDelete().Model((*Tag)(nil))`) {
		t.Error("simple delete should use Bun NewDelete")
	}
	if strings.Contains(code, "DELETE FROM") {
		t.Error("Bun builds the DELETE; should not emit a DELETE FROM literal")
	}

	// Update uses Bun NewUpdate with an explicit column list.
	if !strings.Contains(code, `db.Bun().NewUpdate().Model(msg)`) {
		t.Error("simple update should use Bun NewUpdate")
	}
	if !strings.Contains(code, `Column("label")`) {
		t.Error("Update should set the updatable column via .Column(...)")
	}

	// GetByID uses Bun NewSelect (no soft-delete/tenant filter).
	if !strings.Contains(code, `db.Bun().NewSelect().Model(entity)`) {
		t.Error("simple GetByID should use Bun NewSelect")
	}
	if strings.Contains(code, "orm.NewQueryBuilder(db,") {
		t.Error("GetByID should use Bun NewSelect, not orm.NewQueryBuilder")
	}
	if strings.Contains(code, "func scanTag(") {
		t.Error("Bun scans directly; should not emit a scanTag function")
	}
}

func TestGeneratePlanORM_SoftDeleteOnly(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Item",
			SoftDelete: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "name", Type: "string", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "github.com/test/myapp", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "item_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// Soft-delete filter in List: Bun adds a deleted_at IS NULL WHERE clause.
	if !strings.Contains(code, `q.Where("\"deleted_at\" IS NULL")`) {
		t.Error("missing soft-delete filter")
	}

	// ListAll should exist
	if !strings.Contains(code, "func ListAllItem(ctx context.Context, db orm.Context, opts ...orm.QueryOption)") {
		t.Error("missing ListAllItem without tenant")
	}

	// Delete should use CURRENT_TIMESTAMP
	if !strings.Contains(code, "CURRENT_TIMESTAMP") {
		t.Error("soft delete should use CURRENT_TIMESTAMP")
	}

	// GetByID applies the soft-delete filter so it never returns a tombstone.
	getIdx := strings.Index(code, "func GetItemByID(")
	if getIdx == -1 {
		t.Fatal("missing GetItemByID")
	}
	getCode := code[getIdx:]
	if end := strings.Index(getCode[1:], "\nfunc "); end >= 0 {
		getCode = getCode[:end+1]
	}
	if !strings.Contains(getCode, `Where("\"deleted_at\" IS NULL")`) {
		t.Error("GetByID should filter out soft-deleted rows")
	}

	// deleted_at field should be in columns
	if !strings.Contains(code, `"deleted_at"`) {
		t.Error("missing deleted_at in columns")
	}

	// deleted_at field constant
	if !strings.Contains(code, `ItemFieldDeletedAt = "deleted_at"`) {
		t.Error("missing deleted_at field constant")
	}
}

func TestGeneratePlanORM_TableNameOverride(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:      "UserProfile",
			TableName: "profiles",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "email", Type: "string", NotNull: true, Unique: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "user_profile_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	if !strings.Contains(code, `UserProfileTableName = "profiles"`) {
		t.Error("table name constant not using override")
	}

	// Table name used in queries
	if !strings.Contains(code, `"profiles"`) {
		t.Error("table name override not applied in queries")
	}
}

func TestGeneratePlanORM_FieldTypes(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Record",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "int64", PrimaryKey: true},
				{Name: "count", Type: "int32", NotNull: true},
				{Name: "enabled", Type: "bool", NotNull: true},
				{Name: "score", Type: "float"},
				{Name: "price", Type: "double"},
				{Name: "data", Type: "bytes"},
				{Name: "label", Type: "string"},
				{Name: "meta", Type: "json", NotNull: true},
				{Name: "tags", Type: "[]string", NotNull: true},
				{Name: "nums", Type: "[]int64"},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "record_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// PK type
	if !strings.Contains(code, "func GetRecordByID(ctx context.Context, db orm.Context, id int64)") {
		t.Error("GetByID should use int64 for PK type")
	}
	if !strings.Contains(code, "func DeleteRecord(ctx context.Context, db orm.Context, id int64)") {
		t.Error("Delete should use int64 for PK type")
	}

	// Nullable scalar columns are POINTER struct fields scanned directly —
	// no temp vars (the legacy *float32 scan temps are gone).
	if !strings.Contains(code, "type Record struct {") {
		t.Error("missing Record struct emission")
	}
	// Struct fields are gofmt-aligned; collapse whitespace before matching.
	collapsedStruct := strings.Join(strings.Fields(code), " ")
	if !strings.Contains(collapsedStruct, "Score *float32 `bun:\"score\"`") {
		t.Error("nullable float should be a *float32 struct field")
	}
	if !strings.Contains(collapsedStruct, "Price *float64 `bun:\"price\"`") {
		t.Error("nullable double should be a *float64 struct field")
	}
	if !strings.Contains(collapsedStruct, "Label *string `bun:\"label\"`") {
		t.Error("nullable string should be a *string struct field")
	}
	for _, temp := range []string{"dbScore", "dbPrice", "dbLabel"} {
		if strings.Contains(code, temp) {
			t.Errorf("Bun scans directly into the struct field; found temp %s", temp)
		}
	}
	// Bun scans nullable scalars directly into the pointer struct field; no
	// standalone scan function and no scan-temp aliases.
	if strings.Contains(code, "func scanRecord(") {
		t.Error("Bun scans directly; should not emit a scanRecord function")
	}

	// "json" maps to Go string on the struct.
	if !strings.Contains(collapsedStruct, "Meta string `bun:\"meta\"`") {
		t.Error("json column should be a string struct field")
	}

	// Array columns are native slices tagged ,array so Bun maps them to
	// the underlying SQL array column; no orm.StringArray/Int64Array temps,
	// no orm.ArrayValue encoder, no pq.StringArray.
	if !strings.Contains(collapsedStruct, "Tags []string `bun:\"tags,array\"`") || !strings.Contains(collapsedStruct, "Nums []int64 `bun:\"nums,array\"`") {
		t.Error("array columns should be native slices on the struct")
	}
	if !strings.Contains(code, "`bun:\"tags,array\"`") {
		t.Error("[]string column should carry the bun ,array tag")
	}
	if !strings.Contains(code, "`bun:\"nums,array\"`") {
		t.Error("[]int64 column should carry the bun ,array tag")
	}
	if strings.Contains(code, "orm.StringArray") || strings.Contains(code, "orm.Int64Array") {
		t.Error("Bun handles array columns; should not use orm.StringArray/Int64Array temps")
	}
	if strings.Contains(code, "orm.ArrayValue(") {
		t.Error("Bun encodes array columns; should not call orm.ArrayValue")
	}
	if strings.Contains(code, "pq.StringArray") {
		t.Error("array mapping must use bun ,array tags, not pq.StringArray")
	}
	// Create/Update normalize nil array slices to empty so they never
	// write a SQL NULL into a NOT NULL array column.
	if !strings.Contains(code, "msg.Tags = orm.EmptyIfNil(msg.Tags)") {
		t.Error("array Create/Update should normalize nil slices via orm.EmptyIfNil")
	}
}

func TestGeneratePlanORM_TimestampsOnly(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Event",
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "name", Type: "string", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "event_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// timestamppb is gone from generated ORM code entirely — time columns
	// are time.Time-based and need the time import; database/sql is gone
	// too (orm.NullTime replaced sql.NullTime in scan temps).
	if strings.Contains(code, `"database/sql"`) {
		t.Error("generated file should not import database/sql anymore")
	}
	if strings.Contains(code, "timestamppb") {
		t.Error("generated ORM code must not reference timestamppb")
	}
	if !strings.Contains(code, `"time"`) {
		t.Error("missing time import for timestamp fields")
	}
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "CreatedAt *time.Time") {
		t.Error("nullable created_at should be *time.Time on the struct")
	}

	// Created/updated_at in fields
	if !strings.Contains(code, `EventFieldCreatedAt = "created_at"`) {
		t.Error("missing created_at field constant")
	}
	if !strings.Contains(code, `EventFieldUpdatedAt = "updated_at"`) {
		t.Error("missing updated_at field constant")
	}

	// No deleted_at (no soft-delete)
	if strings.Contains(code, `EventFieldDeletedAt`) {
		t.Error("should not have deleted_at without soft-delete")
	}

	// Bun scans timestamps directly into the *time.Time struct field — no
	// NullTime temps of any flavor.
	if strings.Contains(code, "orm.NullTime") {
		t.Error("Bun scans timestamps directly; should not use orm.NullTime")
	}
	if strings.Contains(code, "sql.NullTime") {
		t.Error("Bun scans timestamps directly; should not use sql.NullTime")
	}

	// Create stamps managed timestamps with time.Now().UTC(). The
	// auto-added columns are nullable pointers, so the guard is a nil
	// check and the stamp assigns through an addressable local.
	if !strings.Contains(code, "now := time.Now().UTC()") {
		t.Error("missing timestamp stamping in Create")
	}
	if !strings.Contains(code, "if msg.CreatedAt == nil {") {
		t.Error("missing created_at zero-guard in Create")
	}
	if !strings.Contains(code, "msg.UpdatedAt = &stampUpdatedAt") {
		t.Error("missing updated_at stamping in Create")
	}

	// Update re-stamps updated_at (pointer-safe).
	if !strings.Contains(code, "stampUpdatedAt := time.Now().UTC()") {
		t.Error("UpdateEvent should stamp updated_at")
	}
}

func TestGeneratePlanORM_Empty(t *testing.T) {
	root := t.TempDir()

	if err := GeneratePlanORM(root, "example.com/app", "api", nil, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	// internal/db should not be created
	if _, err := os.Stat(filepath.Join(root, "internal", "db")); !os.IsNotExist(err) {
		t.Error("internal/db should not exist when no entities")
	}
}

func TestGeneratePlanORM_MultipleEntities(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:   "Org",
			Fields: []config.PlanEntityField{{Name: "id", Type: "string", PrimaryKey: true}},
		},
		{
			Name:   "User",
			Fields: []config.PlanEntityField{{Name: "id", Type: "string", PrimaryKey: true}},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	for _, name := range []string{"org_orm.go", "user_orm.go", "orm_shared.go"} {
		if _, err := os.Stat(filepath.Join(root, "internal", "db", name)); err != nil {
			t.Errorf("expected %s to exist", name)
		}
	}
}

func TestGeneratePlanORM_References(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Comment",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "post_id", Type: "string", NotNull: true, References: "posts.id"},
				{Name: "body", Type: "string", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "comment_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// Should have the projected struct and CRUD functions (Bun scans into
	// the struct directly — no standalone scan function).
	if !strings.Contains(code, "type Comment struct {") {
		t.Error("missing Comment struct emission")
	}
	if strings.Contains(code, "func scanComment(") {
		t.Error("Bun scans directly; should not emit a scanComment function")
	}
	if !strings.Contains(code, "func CreateComment(") {
		t.Error("missing CreateComment function")
	}
}

func TestGeneratePlanORM_TenantOnlyNoSoftDelete(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Setting",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "tenant_id", Type: "string", TenantKey: true, NotNull: true},
				{Name: "key", Type: "string", NotNull: true},
				{Name: "value", Type: "string"},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "setting_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// Tenant-scoped CRUD
	if !strings.Contains(code, "func CreateSetting(ctx context.Context, db orm.Context, msg *Setting, tenantID string) error {") {
		t.Error("missing tenant-scoped Create")
	}

	// Update with tenant but no soft-delete: NewUpdate + id WHERE + tenant
	// WHERE, and NO deleted_at clause.
	updateIdx := strings.Index(code, "func UpdateSetting(")
	if updateIdx == -1 {
		t.Fatal("missing UpdateSetting")
	}
	updateCode := code[updateIdx:]
	if end := strings.Index(updateCode[1:], "\nfunc "); end >= 0 {
		updateCode = updateCode[:end+1]
	}
	if !strings.Contains(updateCode, `db.Bun().NewUpdate().Model(msg)`) {
		t.Error("Update should use Bun NewUpdate")
	}
	if !strings.Contains(updateCode, `Where("\"id\" = ?", msg.Id)`) {
		t.Error("Update should filter by PK")
	}
	if !strings.Contains(updateCode, `q.Where("\"tenant_id\" = ?", tenantID)`) {
		t.Error("Update should have tenant WHERE")
	}
	if strings.Contains(updateCode, "deleted_at") {
		t.Error("Update should not have a soft-delete clause")
	}

	// Delete with tenant but no soft-delete (hard delete via NewDelete).
	deleteIdx := strings.Index(code, "func DeleteSetting(")
	if deleteIdx == -1 {
		t.Fatal("missing DeleteSetting")
	}
	deleteCode := code[deleteIdx:]
	if !strings.Contains(deleteCode, `db.Bun().NewDelete().Model((*Setting)(nil))`) {
		t.Error("Delete should be a hard delete via Bun NewDelete")
	}
	if !strings.Contains(deleteCode, `q.Where("\"tenant_id\" = ?", tenantID)`) {
		t.Error("Delete should isolate by tenant")
	}

	// No ListAll (no soft-delete)
	if strings.Contains(code, "ListAllSetting") {
		t.Error("ListAllSetting should not exist without soft-delete")
	}
}

func TestGeneratePlanORM_AutoIDWhenNoPK(t *testing.T) {
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "widget_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// id should be in field constants (gofmt aligns the `=`).
	collapsed := strings.Join(strings.Fields(code), " ")
	if !strings.Contains(collapsed, `WidgetFieldId = "id"`) {
		t.Error("missing auto-generated id field constant")
	}

	// id should be the first column in the ordered (exported) list
	colIdx := strings.Index(code, "var WidgetColumns = []string{")
	if colIdx == -1 {
		t.Fatal("missing exported WidgetColumns")
	}
	colSection := code[colIdx : colIdx+200]
	idIdx := strings.Index(colSection, `"id"`)
	nameIdx := strings.Index(colSection, `"name"`)
	if idIdx == -1 {
		t.Fatal("id not found in WidgetColumns")
	}
	if idIdx >= nameIdx {
		t.Error("id should appear before name in WidgetColumns")
	}

	// The struct carries the id field with a bun pk tag so Bun scans it.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "Id string `bun:\"id,pk\"`") {
		t.Error("Widget struct should carry a bun-tagged Id field")
	}

	// CRUD functions should use string PK type
	if !strings.Contains(code, "func GetWidgetByID(ctx context.Context, db orm.Context, id string)") {
		t.Error("GetWidgetByID should use string type for auto-generated id")
	}
	if !strings.Contains(code, "func DeleteWidget(ctx context.Context, db orm.Context, id string)") {
		t.Error("DeleteWidget should use string type for auto-generated id")
	}

	// Create is a plain INSERT; the auto-generated string id is filled in
	// at the Create chokepoint via ULID.
	if strings.Contains(code, "ON CONFLICT") {
		t.Error("Create must be a plain INSERT, found ON CONFLICT upsert")
	}
	if !strings.Contains(code, "msg.Id = ulid.Make().String()") {
		t.Error("Create should generate a ULID for the auto-added string id")
	}
}

func TestGeneratePlanORM_ExplicitPKNotDuplicated(t *testing.T) {
	root := t.TempDir()

	// Entity already has an explicit PK — no id should be auto-added.
	entities := []config.PlanEntity{
		{
			Name: "Counter",
			Fields: []config.PlanEntityField{
				{Name: "counter_id", Type: "int64", PrimaryKey: true},
				{Name: "value", Type: "int32", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "counter_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// Should NOT have an auto-generated id field constant
	if strings.Contains(code, `CounterFieldId = "id"`) {
		t.Error("should not auto-add id when explicit PK exists")
	}

	// Should use counter_id as the PK
	if !strings.Contains(code, `CounterFieldCounterId = "counter_id"`) {
		t.Error("missing counter_id field constant")
	}
	if !strings.Contains(code, "func GetCounterByID(ctx context.Context, db orm.Context, id int64)") {
		t.Error("GetCounterByID should use int64 PK type from explicit PK")
	}
}

func TestGeneratePlanORM_UpdateSetColumnsExcludeSpecial(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Task",
			SoftDelete: true,
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "org_id", Type: "string", TenantKey: true, NotNull: true},
				{Name: "title", Type: "string", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "task_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// The .Column(...) list should include title and updated_at but NOT
	// id, org_id, deleted_at, or the immutable created_at.
	updateIdx := strings.Index(code, "func UpdateTask(")
	if updateIdx == -1 {
		t.Fatal("missing UpdateTask function")
	}
	updateCode := code[updateIdx:]
	if end := strings.Index(updateCode[1:], "\nfunc "); end >= 0 {
		updateCode = updateCode[:end+1]
	}

	// Extract the Bun Column(...) argument list that builds the SET clause.
	// gofmt puts the chained method on its own line, so match "Column(".
	colCallIdx := strings.Index(updateCode, "Column(")
	if colCallIdx == -1 {
		t.Fatal("missing Column(...) in UpdateTask")
	}
	colCall := updateCode[colCallIdx:]
	colEnd := strings.Index(colCall, ")")
	if colEnd == -1 {
		t.Fatal("malformed Column(...) in UpdateTask")
	}
	colSection := colCall[:colEnd]

	// Should set title and updated_at.
	if !strings.Contains(colSection, `"title"`) {
		t.Error("Update column list should include title")
	}
	if !strings.Contains(colSection, `"updated_at"`) {
		t.Error("Update column list should include updated_at")
	}
	// Should NOT set the PK, the tenant key, deleted_at, or the immutable
	// created_at.
	if strings.Contains(colSection, `"id"`) {
		t.Error("Update column list should not include PK (id)")
	}
	if strings.Contains(colSection, `"deleted_at"`) {
		t.Error("Update column list should not include deleted_at")
	}
	if strings.Contains(colSection, `"org_id"`) {
		t.Error("Update column list should not include tenant key (org_id)")
	}
	if strings.Contains(colSection, `"created_at"`) {
		t.Error("Update column list should not include created_at (immutable)")
	}
	if !strings.Contains(updateCode, "stampUpdatedAt := time.Now().UTC()") ||
		!strings.Contains(updateCode, "msg.UpdatedAt = &stampUpdatedAt") {
		t.Error("UpdateTask should stamp updated_at (pointer-safe) before the query")
	}
}

func TestGeneratePlanORM_DeclaredTimestampsNotDuplicated(t *testing.T) {
	root := t.TempDir()

	// Entity DECLARES created_at/updated_at/deleted_at explicitly alongside
	// Timestamps/SoftDelete — resolveORMFields must not auto-add duplicates
	// (duplicate consts are a compile error; duplicate columns break SQL).
	entities := []config.PlanEntity{
		{
			Name:       "Doc",
			Timestamps: true,
			SoftDelete: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "title", Type: "string", NotNull: true},
				// Legacy proto-type alias stays mapped during the transition;
				// NOT NULL declared timestamps become bare time.Time fields.
				{Name: "created_at", Type: "google.protobuf.Timestamp", NotNull: true},
				{Name: "updated_at", Type: "timestamp", NotNull: true},
				{Name: "deleted_at", Type: "time"},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "doc_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	for _, constName := range []string{"DocFieldCreatedAt", "DocFieldUpdatedAt", "DocFieldDeletedAt"} {
		if got := strings.Count(code, constName+" ="); got != 1 {
			t.Errorf("%s declared %d times, want exactly 1", constName, got)
		}
	}
	for _, col := range []string{`"created_at",`, `"updated_at",`, `"deleted_at",`} {
		colIdx := strings.Index(code, "var DocColumns = []string{")
		if colIdx == -1 {
			t.Fatal("missing DocColumns")
		}
		end := strings.Index(code[colIdx:], "}")
		section := code[colIdx : colIdx+end]
		if got := strings.Count(section, col); got != 1 {
			t.Errorf("column %s appears %d times in DocColumns, want exactly 1", col, got)
		}
	}

	// Declared timestamps still get the managed-timestamp chokepoints.
	if !strings.Contains(code, "if msg.CreatedAt.IsZero() {") {
		t.Error("Create should stamp declared created_at")
	}
	if !strings.Contains(code, "msg.UpdatedAt = time.Now().UTC()") {
		t.Error("Update should stamp declared updated_at")
	}

	// NOT NULL declared timestamps are bare time.Time struct fields that
	// Bun scans directly; the nullable deleted_at stays a pointer.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "CreatedAt time.Time") {
		t.Error("NOT NULL created_at should be time.Time on the struct")
	}
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "DeletedAt *time.Time") {
		t.Error("nullable deleted_at should be *time.Time on the struct")
	}
	// Bun scans directly into the struct — no NullTime temp assignment.
	if strings.Contains(code, "dbCreatedAt") {
		t.Error("Bun scans directly; should not use a NullTime temp for created_at")
	}
}

// TestGeneratePlanORM_MaskedUpdate pins the Update<Entity>Masked sibling:
// an AIP-134 partial update that writes ONLY the fields named in the
// update_mask, validates paths against the updatable set, and stamps
// updated_at when timestamps are managed.
func TestGeneratePlanORM_MaskedUpdate(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Task",
			SoftDelete: true,
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "org_id", Type: "string", TenantKey: true, NotNull: true},
				{Name: "title", Type: "string", NotNull: true},
				{Name: "url", Type: "string"},
			},
		},
		{
			Name: "Note",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "body", Type: "string"},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	taskSrc, err := os.ReadFile(filepath.Join(root, "internal", "db", "task_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	task := string(taskSrc)

	// Tenant-scoped signature mirrors UpdateTask: fields slice, then tenantID.
	if !strings.Contains(task, "func UpdateTaskMasked(ctx context.Context, db orm.Context, msg *Task, fields []string, tenantID string) error {") {
		t.Error("missing tenant-scoped UpdateTaskMasked signature")
	}
	maskedIdx := strings.Index(task, "func UpdateTaskMasked(")
	if maskedIdx == -1 {
		t.Fatal("missing UpdateTaskMasked function")
	}
	masked := task[maskedIdx:]
	if end := strings.Index(masked[1:], "\nfunc "); end >= 0 {
		masked = masked[:end+1]
	}

	// Empty mask → do nothing (the crud layer never sends it, but be safe).
	if !strings.Contains(masked, "if len(fields) == 0 {") {
		t.Error("UpdateTaskMasked should no-op on an empty fields slice")
	}
	// Only requested columns reach SET — built from an updatable-column map.
	for _, col := range []string{`"title":`, `"url":`} {
		if !strings.Contains(masked, col) {
			t.Errorf("UpdateTaskMasked updatable set should include %s", col)
		}
	}
	// PK, tenant key, deleted_at, created_at must NOT be updatable via mask.
	for _, col := range []string{`"id":`, `"org_id":`, `"deleted_at":`, `"created_at":`} {
		if strings.Contains(masked, col) {
			t.Errorf("UpdateTaskMasked updatable set must exclude %s", col)
		}
	}
	// Unknown/immutable paths surface as the typed sentinel, not SQL errors.
	if !strings.Contains(masked, "orm.UnknownFieldError{Field:") {
		t.Error("UpdateTaskMasked should return orm.UnknownFieldError for unknown paths")
	}
	// Only the masked (and stamped) columns reach the SET clause, via Bun's
	// .Column(cols...).
	if !strings.Contains(masked, "Column(cols...)") {
		t.Error("UpdateTaskMasked should write only the masked columns via .Column(cols...)")
	}
	// updated_at is stamped on masked updates too (timestamps: true) —
	// pointer-safe, since the auto-added column is nullable.
	if !strings.Contains(masked, "stampUpdatedAt := time.Now().UTC()") ||
		!strings.Contains(masked, "msg.UpdatedAt = &stampUpdatedAt") {
		t.Error("UpdateTaskMasked should stamp updated_at")
	}
	// Tenant isolation + soft-delete filter carry over from UpdateTask as
	// chained Bun Where clauses.
	if !strings.Contains(masked, `q.Where("\"deleted_at\" IS NULL")`) {
		t.Error("UpdateTaskMasked should keep the soft-delete WHERE clause")
	}
	if !strings.Contains(masked, `q.Where("\"org_id\" = ?", tenantID)`) {
		t.Error("UpdateTaskMasked should keep the tenant WHERE clause")
	}

	noteSrc, err := os.ReadFile(filepath.Join(root, "internal", "db", "note_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	note := string(noteSrc)
	// Un-tenanted signature has no tenantID parameter.
	if !strings.Contains(note, "func UpdateNoteMasked(ctx context.Context, db orm.Context, msg *Note, fields []string) error {") {
		t.Error("missing un-tenanted UpdateNoteMasked signature")
	}
}

// ── M3: type-aware managed timestamps ──────────────────────────────────
//
// Kalshi fr-3fba9166ba: a legacy schema stores created_at/updated_at as
// TEXT. DetectConventions still reports timestamps:true (both columns
// exist), but the emitter stamped time.Now().UTC()/IsZero()
// unconditionally — `undefined: time` + `msg.CreatedAt.IsZero undefined
// (type string)`, so `forge generate` could NEVER produce compiling
// output for that schema. Stamping must branch on the projected Go type.

// TestGeneratePlanORM_TextTimestamps_Stamping pins the string branch:
// TEXT created_at/updated_at columns get RFC3339Nano string stamps, the
// time import is present (stamping needs it even with zero time.Time
// columns), and no time.Time-only constructs (IsZero) leak in.
func TestGeneratePlanORM_TextTimestamps_Stamping(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Trade",
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "ticker", Type: "string", NotNull: true},
				// Legacy schema: timestamps stored as TEXT.
				{Name: "created_at", Type: "string", NotNull: true},
				{Name: "updated_at", Type: "string", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	code := readGeneratedORM(t, root, "trade_orm.go")

	// The struct projects the applied schema: strings stay strings.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "CreatedAt string") {
		t.Error("TEXT created_at should project as a string struct field")
	}

	// The stamping needs the time package even though no column is
	// time.Time — `undefined: time` was the literal kalshi error.
	if !strings.Contains(code, "\t\"time\"\n") {
		t.Error("missing time import for string-timestamp stamping")
	}

	// String columns are stamped as RFC3339Nano text, guarded by the
	// string zero value — never IsZero (undefined on string).
	if strings.Contains(code, "IsZero()") {
		t.Error("string timestamp columns must not use time.Time's IsZero")
	}
	if !strings.Contains(code, `if msg.CreatedAt == "" {`) {
		t.Error("missing string zero-guard for created_at in Create")
	}
	if !strings.Contains(code, "msg.CreatedAt = now.Format(time.RFC3339Nano)") {
		t.Error("missing RFC3339Nano created_at stamp in Create")
	}
	if !strings.Contains(code, "msg.UpdatedAt = now.Format(time.RFC3339Nano)") {
		t.Error("missing RFC3339Nano updated_at stamp in Create")
	}

	// Update + masked update stamp updated_at in the column's type too.
	if !strings.Contains(code, "msg.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)") {
		t.Error("Update/UpdateMasked should stamp updated_at as RFC3339Nano text")
	}
	if strings.Contains(code, "msg.UpdatedAt = time.Now().UTC()\n") {
		t.Error("string updated_at must not be assigned a bare time.Time")
	}
}

// TestGeneratePlanORM_NullableTimestamps_PointerSafeStamping pins the
// pointer branches: nullable managed columns project as *time.Time /
// *string, and the stamping must assign through a pointer — the old
// emitter assigned a bare value to the pointer field (compile error)
// and called IsZero on a possibly-nil pointer (runtime panic).
func TestGeneratePlanORM_NullableTimestamps_PointerSafeStamping(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Audit",
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				// Nullable (no NotNull): pointer struct fields.
				{Name: "created_at", Type: "time"},
				{Name: "updated_at", Type: "time"},
			},
		},
		{
			Name:       "Legacy",
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "created_at", Type: "string"},
				{Name: "updated_at", Type: "string"},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	audit := readGeneratedORM(t, root, "audit_orm.go")
	if !strings.Contains(strings.Join(strings.Fields(audit), " "), "CreatedAt *time.Time") {
		t.Fatal("nullable created_at should be *time.Time — precondition for this test")
	}
	// nil-guard, not IsZero (nil pointer would panic).
	if !strings.Contains(audit, "if msg.CreatedAt == nil {") {
		t.Error("nullable created_at should be guarded with a nil check")
	}
	if strings.Contains(audit, "msg.CreatedAt.IsZero()") {
		t.Error("nullable created_at must not call IsZero through a possibly-nil pointer")
	}
	// Assignment goes through a local so we can take its address.
	if !strings.Contains(audit, "msg.CreatedAt = &") {
		t.Error("nullable created_at stamp should assign a pointer")
	}
	if !strings.Contains(audit, "msg.UpdatedAt = &") {
		t.Error("nullable updated_at stamp should assign a pointer")
	}
	// The bare-value assignment to a pointer field was the compile error.
	if strings.Contains(audit, "msg.UpdatedAt = now\n") {
		t.Error("nullable updated_at must not be assigned a bare time.Time")
	}

	legacy := readGeneratedORM(t, root, "legacy_orm.go")
	if !strings.Contains(strings.Join(strings.Fields(legacy), " "), "CreatedAt *string") {
		t.Fatal("nullable TEXT created_at should be *string — precondition for this test")
	}
	if !strings.Contains(legacy, "if msg.CreatedAt == nil {") {
		t.Error("nullable *string created_at should be guarded with a nil check")
	}
	if !strings.Contains(legacy, "msg.CreatedAt = &") {
		t.Error("nullable *string created_at stamp should assign a pointer")
	}
}

// TestGeneratePlanORM_UnstampableTimestampsSkipped pins the safety
// valve: a managed-timestamp column of a type the emitter can't stamp
// (e.g. an epoch INTEGER) is left entirely alone — no stamping code, no
// phantom time import, output still compiles.
func TestGeneratePlanORM_UnstampableTimestampsSkipped(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Epoch",
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "name", Type: "string", NotNull: true},
				{Name: "created_at", Type: "int64", NotNull: true},
				{Name: "updated_at", Type: "int64", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	code := readGeneratedORM(t, root, "epoch_orm.go")
	if strings.Contains(code, "time.Now()") {
		t.Error("unstampable timestamp types must not emit time.Now() stamping")
	}
	if strings.Contains(code, "\t\"time\"\n") {
		t.Error("no time import expected when nothing references the time package")
	}
	if strings.Contains(code, "IsZero()") {
		t.Error("int64 created_at must not call IsZero")
	}
}

// readGeneratedORM reads internal/db/<name> under root, failing the test
// on error.
func readGeneratedORM(t *testing.T, root, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, "internal", "db", name))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", name, err)
	}
	return string(content)
}

// ── M3: server-allocated integer primary keys ──────────────────────────
//
// Kalshi fr-fd061aed2b: Create<X> INSERTed the id column from msg.Id —
// always 0 for BIGSERIAL rows — so every writer routed around the ORM.
// Integer PKs are server-allocated: Create omits the id column and
// scans the database-assigned value back into msg (RETURNING where the
// dialect supports it, LastInsertId otherwise), mirroring the string-PK
// ULID chokepoint.
func TestGeneratePlanORM_IntegerPKCreate_ServerAllocated(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name:       "Hypothesis",
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "int64", PrimaryKey: true, NotNull: true},
				{Name: "title", Type: "string", NotNull: true},
				{Name: "created_at", Type: "time", NotNull: true},
				{Name: "updated_at", Type: "time", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	code := readGeneratedORM(t, root, "hypothesis_orm.go")

	createIdx := strings.Index(code, "func CreateHypothesis(")
	if createIdx == -1 {
		t.Fatal("missing CreateHypothesis")
	}
	create := code[createIdx:]
	if end := strings.Index(create[1:], "\nfunc "); end >= 0 {
		create = create[:end+1]
	}

	// The id column is excluded from the INSERT (server-allocated), and the
	// caller's msg.Id is never passed as an insert value.
	if !strings.Contains(create, `q.ExcludeColumn("id")`) {
		t.Error("CreateHypothesis must exclude the server-allocated id column from the INSERT")
	}
	if strings.Contains(create, "msg.Id,") {
		t.Error("CreateHypothesis must not pass msg.Id as an INSERT value")
	}

	// The database-assigned id is read back via RETURNING into msg.Id.
	// (postgres-only: always RETURNING, Bun handles the scan into Exec's
	// dest — there is no SupportsReturning/LastInsertId branch anymore.)
	if !strings.Contains(create, `Returning("\"id\"")`) {
		t.Error("CreateHypothesis should scan the allocated id back via RETURNING")
	}
	if !strings.Contains(create, "q.Exec(ctx, &msg.Id)") {
		t.Error("CreateHypothesis should scan the allocated id into msg.Id")
	}
	if strings.Contains(create, "dialect.SupportsReturning()") {
		t.Error("server-allocated Create is postgres-only RETURNING; no SupportsReturning branch")
	}
	if strings.Contains(create, "LastInsertId()") {
		t.Error("server-allocated Create is postgres-only RETURNING; no LastInsertId fallback")
	}

	// No ULID machinery for integer PKs.
	if strings.Contains(code, "ulid.") {
		t.Error("integer-PK entity must not import/use ulid")
	}

	// Get/Delete keep the int64 PK signature.
	if !strings.Contains(code, "func GetHypothesisByID(ctx context.Context, db orm.Context, id int64)") {
		t.Error("GetHypothesisByID should take an int64 id")
	}
}

// TestGeneratePlanORM_Int32PKCreate_ServerAllocated pins the non-int64
// integer PK: like int64, an int32 BIGSERIAL/SERIAL PK is server-allocated
// — the column is excluded from the INSERT and the database-assigned value
// is read back via RETURNING into msg.Id. Bun handles the int64→int32
// conversion at the scan boundary, so no explicit int32(...) conversion is
// emitted (and there is no LastInsertId fallback — postgres-only RETURNING).
func TestGeneratePlanORM_Int32PKCreate_ServerAllocated(t *testing.T) {
	root := t.TempDir()

	entities := []config.PlanEntity{
		{
			Name: "Tick",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "int32", PrimaryKey: true, NotNull: true},
				{Name: "label", Type: "string", NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities, nil); err != nil {
		t.Fatalf("error = %v", err)
	}

	code := readGeneratedORM(t, root, "tick_orm.go")

	createIdx := strings.Index(code, "func CreateTick(")
	if createIdx == -1 {
		t.Fatal("missing CreateTick")
	}
	create := code[createIdx:]
	if end := strings.Index(create[1:], "\nfunc "); end >= 0 {
		create = create[:end+1]
	}

	// Server-allocated: exclude the id column, scan it back via RETURNING.
	if !strings.Contains(create, `q.ExcludeColumn("id")`) {
		t.Error("int32 PK Create must exclude the server-allocated id column")
	}
	if !strings.Contains(create, `Returning("\"id\"")`) {
		t.Error("int32 PK Create should scan the allocated id back via RETURNING")
	}
	if !strings.Contains(create, "q.Exec(ctx, &msg.Id)") {
		t.Error("int32 PK Create should scan the allocated id into msg.Id")
	}
	// Bun converts at the scan boundary — no explicit int32(...) conversion,
	// no LastInsertId fallback.
	if strings.Contains(create, "msg.Id = int32(") {
		t.Error("int32 PK Create should let Bun convert at the scan boundary, not int32(...)")
	}
	if strings.Contains(create, "LastInsertId()") {
		t.Error("int32 PK Create is postgres-only RETURNING; no LastInsertId fallback")
	}

	// Get/Delete keep the int32 PK signature.
	if !strings.Contains(code, "func GetTickByID(ctx context.Context, db orm.Context, id int32)") {
		t.Error("GetTickByID should take an int32 id")
	}
}
