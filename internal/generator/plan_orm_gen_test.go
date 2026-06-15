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

	// The lifecycle now lives in the generic forge/pkg/crud.Repo. The
	// generated file constructs a per-entity repo whose Spec carries only
	// the conventions Bun can't read off the struct tags. Tenant
	// enforcement is the repo's job — driven by Spec.TenantColumn, NOT a
	// hand-rolled `msg.OrgId = tenantID` in Create.
	if strings.Contains(code, "msg.OrgId = tenantID") {
		t.Error("tenant enforcement now lives in pkg/crud; Create should not stamp the tenant inline")
	}
	if !strings.Contains(code, `var projectRepo = crud.NewRepo[Project](crud.Spec{`) {
		t.Error("missing per-entity crud.Repo var for Project")
	}
	// gofmt aligns the struct literal; collapse whitespace before matching
	// the Spec fields.
	collapsedAll := strings.Join(strings.Fields(code), " ")
	if !strings.Contains(collapsedAll, `TenantColumn: "org_id"`) {
		t.Error("Project repo Spec should set TenantColumn to the tenant column")
	}
	if !strings.Contains(collapsedAll, "Timestamps: true") {
		t.Error("Project repo Spec should set Timestamps: true (managed timestamps)")
	}
	// The generated file imports the generic crud library.
	if !strings.Contains(code, `"github.com/reliant-labs/forge/pkg/crud"`) {
		t.Error("generated file should import forge/pkg/crud")
	}
	// The delegates forward to the repo with the tenant arg.
	if !strings.Contains(code, "return projectRepo.Create(ctx, db, msg, tenantID)") {
		t.Error("CreateProject should delegate to projectRepo.Create with tenantID")
	}

	// Soft delete is Bun-native (proper TIMESTAMPTZ deleted_at): the
	// deleted_at field carries the ,soft_delete,nullzero tag (the library
	// reads it off the schema). The inline read-filter / CURRENT_TIMESTAMP
	// stamp / NewDelete body all moved into pkg/crud — only the struct tag
	// is asserted here.
	if !strings.Contains(collapsedStruct, "`bun:\"deleted_at,soft_delete,nullzero\"`") {
		t.Error("deleted_at should carry the Bun ,soft_delete,nullzero tag")
	}
	if strings.Contains(code, "CURRENT_TIMESTAMP") {
		t.Error("soft-delete stamping lives in pkg/crud; no CURRENT_TIMESTAMP in generated output")
	}

	// ListAll (soft-delete bypass) is emitted iff SoftDelete and forwards
	// to the repo.
	if !strings.Contains(code, "func ListAllProject(") {
		t.Error("missing ListAllProject")
	}
	if !strings.Contains(code, "return projectRepo.ListAll(ctx, db, tenantID, opts...)") {
		t.Error("ListAllProject should delegate to projectRepo.ListAll")
	}

	// The inline Bun query bodies (NewInsert/NewSelect/NewDelete), the ULID
	// chokepoint, and the timestamp stamping all moved into pkg/crud. The
	// generated file must NOT carry any of them anymore.
	if strings.Contains(code, "db.Bun().NewInsert()") {
		t.Error("Create body moved to pkg/crud; no inline NewInsert")
	}
	if strings.Contains(code, "ON CONFLICT") {
		t.Error("Create must be a plain INSERT, found ON CONFLICT upsert")
	}
	if strings.Contains(code, `"github.com/oklog/ulid/v2"`) {
		t.Error("ULID generation lives in pkg/crud; generated file should not import ulid")
	}
	if strings.Contains(code, "ulid.Make()") {
		t.Error("ULID generation lives in pkg/crud; no inline ulid.Make()")
	}
	if strings.Contains(code, "now := time.Now().UTC()") {
		t.Error("timestamp stamping lives in pkg/crud; no inline time.Now().UTC()")
	}
	if strings.Contains(code, "fmt.Errorf(") {
		t.Error("error wrapping lives in pkg/crud; generated file no longer imports fmt")
	}
	if strings.Contains(code, "db.Bun().NewSelect()") {
		t.Error("read bodies moved to pkg/crud; no inline NewSelect")
	}
	if strings.Contains(code, "orm.NewQueryBuilder(db,") {
		t.Error("reads go through pkg/crud's Bun NewSelect, not orm.NewQueryBuilder")
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

	// The lifecycle lives in the generic crud.Repo. A no-tenant entity
	// emits a repo with an EMPTY Spec (no TenantColumn, no Timestamps) and
	// every delegate forwards the empty-string tenant arg.
	if !strings.Contains(code, `"github.com/reliant-labs/forge/pkg/crud"`) {
		t.Error("generated file should import forge/pkg/crud")
	}
	if !strings.Contains(code, "var tagRepo = crud.NewRepo[Tag](crud.Spec{") {
		t.Error("missing per-entity crud.Repo var for Tag")
	}
	if strings.Contains(code, "TenantColumn:") {
		t.Error("no-tenant entity's repo Spec must not set TenantColumn")
	}
	if !strings.Contains(code, `return tagRepo.Create(ctx, db, msg, "")`) {
		t.Error("CreateTag should delegate to tagRepo.Create with an empty tenant arg")
	}
	if !strings.Contains(code, `return tagRepo.Delete(ctx, db, id, "")`) {
		t.Error("DeleteTag should delegate to tagRepo.Delete with an empty tenant arg")
	}
	if !strings.Contains(code, `return tagRepo.Update(ctx, db, msg, "")`) {
		t.Error("UpdateTag should delegate to tagRepo.Update with an empty tenant arg")
	}

	// The inline Bun query bodies all moved into pkg/crud.
	if strings.Contains(code, "DELETE FROM") {
		t.Error("Bun builds the DELETE in pkg/crud; should not emit a DELETE FROM literal")
	}
	if strings.Contains(code, "db.Bun().NewUpdate()") {
		t.Error("Update body moved to pkg/crud; no inline NewUpdate")
	}
	if strings.Contains(code, "db.Bun().NewSelect()") {
		t.Error("GetByID body moved to pkg/crud; no inline NewSelect")
	}
	if strings.Contains(code, "orm.NewQueryBuilder(db,") {
		t.Error("reads go through pkg/crud, not orm.NewQueryBuilder")
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

	// Soft delete is Bun-native: the deleted_at column carries the
	// ,soft_delete,nullzero tag (the library reads it off the schema). The
	// read exclusion / delete stamp / WhereAllWithDeleted bodies moved into
	// pkg/crud — only the struct tag is asserted here.
	collapsed := strings.Join(strings.Fields(code), " ")
	if !strings.Contains(collapsed, "`bun:\"deleted_at,soft_delete,nullzero\"`") {
		t.Error("deleted_at should carry the Bun ,soft_delete,nullzero tag")
	}
	if strings.Contains(code, "CURRENT_TIMESTAMP") {
		t.Error("soft-delete stamping lives in pkg/crud; no CURRENT_TIMESTAMP")
	}

	// ListAll is emitted iff SoftDelete and forwards to the repo (the
	// WhereAllWithDeleted opt-out now lives in pkg/crud.ListAll).
	if !strings.Contains(code, "func ListAllItem(ctx context.Context, db orm.Context, opts ...orm.QueryOption)") {
		t.Error("missing ListAllItem without tenant")
	}
	if !strings.Contains(code, `return itemRepo.ListAll(ctx, db, "", opts...)`) {
		t.Error("ListAllItem should delegate to itemRepo.ListAll")
	}

	// The inline Bun NewDelete body moved into pkg/crud.
	if strings.Contains(code, "db.Bun().NewDelete()") {
		t.Error("Delete body moved to pkg/crud; no inline NewDelete")
	}
	if !strings.Contains(code, `return itemRepo.Delete(ctx, db, id, "")`) {
		t.Error("DeleteItem should delegate to itemRepo.Delete")
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

	// "json" maps to Go string on the struct; NOT NULL projects ,notnull.
	if !strings.Contains(collapsedStruct, "Meta string `bun:\"meta,notnull\"`") {
		t.Error("json NOT NULL column should be a string struct field tagged ,notnull")
	}

	// Array columns are native slices tagged ,array so Bun maps them to
	// the underlying SQL array column; no orm.StringArray/Int64Array temps,
	// no orm.ArrayValue encoder, no pq.StringArray. NOT NULL also projects
	// ,notnull (tags is NOT NULL; nums is nullable).
	if !strings.Contains(collapsedStruct, "Tags []string `bun:\"tags,array,notnull\"`") || !strings.Contains(collapsedStruct, "Nums []int64 `bun:\"nums,array\"`") {
		t.Error("array columns should be native slices on the struct with ,array (+ ,notnull when NOT NULL)")
	}
	if !strings.Contains(code, "`bun:\"tags,array,notnull\"`") {
		t.Error("[]string NOT NULL column should carry the bun ,array,notnull tags")
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
	// The nil-array-to-empty normalization moved into pkg/crud (it runs off
	// the ,array tag there); the generated file no longer inlines it.
	if strings.Contains(code, "orm.EmptyIfNil(") {
		t.Error("array nil-normalization lives in pkg/crud; no inline orm.EmptyIfNil")
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

	// Managed-timestamp stamping (created_at on insert, updated_at re-stamp
	// on update) moved into pkg/crud. The generated file no longer inlines
	// time.Now().UTC(); instead the repo Spec sets Timestamps: true, which
	// drives the library. The time import survives because the *time.Time
	// struct field needs it.
	if strings.Contains(code, "time.Now().UTC()") {
		t.Error("timestamp stamping lives in pkg/crud; no inline time.Now().UTC()")
	}
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "Timestamps: true") {
		t.Error("Event repo Spec should set Timestamps: true (managed timestamps)")
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

	// Tenant isolation is driven by the repo Spec's TenantColumn (the
	// inline id/tenant WHERE clauses moved into pkg/crud). With no soft
	// delete, the Spec carries no LegacyTextDeletedAt flag and the struct
	// has no soft_delete tag.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), `TenantColumn: "tenant_id"`) {
		t.Error("Setting repo Spec should set TenantColumn to tenant_id")
	}
	if strings.Contains(code, "db.Bun().NewUpdate()") {
		t.Error("Update body moved to pkg/crud; no inline NewUpdate")
	}
	if strings.Contains(code, "db.Bun().NewDelete()") {
		t.Error("Delete body moved to pkg/crud; no inline NewDelete")
	}
	if strings.Contains(code, "soft_delete") {
		t.Error("no soft-delete tag expected without SoftDelete")
	}

	// Delegates forward to the repo with the tenant arg.
	if !strings.Contains(code, "return settingRepo.Update(ctx, db, msg, tenantID)") {
		t.Error("UpdateSetting should delegate to settingRepo.Update with tenantID")
	}
	if !strings.Contains(code, "return settingRepo.Delete(ctx, db, id, tenantID)") {
		t.Error("DeleteSetting should delegate to settingRepo.Delete with tenantID")
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
	// at the Create chokepoint via ULID — but that chokepoint now lives in
	// pkg/crud (string-PK detection off Bun's schema), so the generated
	// file no longer inlines ulid.Make() and forwards to the repo instead.
	if strings.Contains(code, "ON CONFLICT") {
		t.Error("Create must be a plain INSERT, found ON CONFLICT upsert")
	}
	if strings.Contains(code, "ulid.Make()") {
		t.Error("ULID generation lives in pkg/crud; no inline ulid.Make()")
	}
	if !strings.Contains(code, `return widgetRepo.Create(ctx, db, msg, "")`) {
		t.Error("CreateWidget should delegate to widgetRepo.Create")
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

	// The SET-clause column selection (exclude PK/tenant/created_at/
	// deleted_at, re-stamp updated_at) now lives in pkg/crud, tested there
	// against real postgres. The generator's contract is to emit the repo
	// Spec that drives it and a thin Update delegate that forwards.
	collapsed := strings.Join(strings.Fields(code), " ")
	if !strings.Contains(collapsed, `TenantColumn: "org_id"`) {
		t.Error("Task repo Spec should set TenantColumn to org_id")
	}
	if !strings.Contains(collapsed, "Timestamps: true") {
		t.Error("Task repo Spec should set Timestamps: true")
	}
	if !strings.Contains(code, "return taskRepo.Update(ctx, db, msg, tenantID)") {
		t.Error("UpdateTask should delegate to taskRepo.Update")
	}
	// The Columns var still lists every declared column (it doubles as the
	// order_by/filter allowlist handed to pkg/crud).
	for _, col := range []string{`"id"`, `"org_id"`, `"title"`, `"created_at"`, `"updated_at"`, `"deleted_at"`} {
		colIdx := strings.Index(code, "var TaskColumns = []string{")
		if colIdx == -1 {
			t.Fatal("missing TaskColumns")
		}
		end := strings.Index(code[colIdx:], "}")
		if !strings.Contains(code[colIdx:colIdx+end], col) {
			t.Errorf("TaskColumns should list %s", col)
		}
	}
	// The inline SET-column selection and pointer-safe stamp moved to the
	// library — the generated file no longer inlines them.
	if strings.Contains(code, "stampUpdatedAt") {
		t.Error("updated_at stamping lives in pkg/crud; no inline stampUpdatedAt")
	}
	if strings.Contains(code, "db.Bun().NewUpdate()") {
		t.Error("Update body moved to pkg/crud; no inline NewUpdate")
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

	// Declared timestamps still get the managed-timestamp chokepoints, but
	// those now live in pkg/crud — driven by the repo Spec's Timestamps
	// flag. The generated file no longer inlines IsZero/time.Now() stamps.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "Timestamps: true") {
		t.Error("Doc repo Spec should set Timestamps: true (declared managed timestamps)")
	}
	if strings.Contains(code, "IsZero()") {
		t.Error("timestamp stamping lives in pkg/crud; no inline IsZero")
	}
	if strings.Contains(code, "time.Now().UTC()") {
		t.Error("timestamp stamping lives in pkg/crud; no inline time.Now().UTC()")
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

	// The masked-update mechanics (empty-mask no-op, updatable-column
	// allowlist, UnknownFieldError sentinel, .Column(cols...) SET clause,
	// updated_at stamp, tenant + soft-delete WHERE) all moved into
	// pkg/crud.UpdateMasked, tested there against real postgres. The
	// generator's contract is the delegate that forwards to it.
	if !strings.Contains(masked, "return taskRepo.UpdateMasked(ctx, db, msg, fields, tenantID)") {
		t.Error("UpdateTaskMasked should delegate to taskRepo.UpdateMasked with the fields slice and tenantID")
	}
	// None of the old inline machinery should survive in the generated file.
	for _, gone := range []string{"orm.UnknownFieldError", "Column(cols...)", "stampUpdatedAt", `deleted_at\" IS NULL`} {
		if strings.Contains(masked, gone) {
			t.Errorf("UpdateTaskMasked inline %q moved to pkg/crud; should not appear in generated output", gone)
		}
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

	// The struct projects the applied schema: TEXT timestamps stay strings.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "CreatedAt string") {
		t.Error("TEXT created_at should project as a string struct field")
	}

	// The type-aware stamping (RFC3339Nano text for TEXT columns, branching
	// on the projected Go type — kalshi fr-3fba9166ba) moved into
	// pkg/crud, driven by the repo Spec's Timestamps flag.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "Timestamps: true") {
		t.Error("Trade repo Spec should set Timestamps: true (managed TEXT timestamps)")
	}

	// With stamping in the library and no time.Time columns, the generated
	// file references nothing from the time package — and must therefore
	// NOT import it (an unused "time" import would not compile).
	if strings.Contains(code, "\t\"time\"\n") {
		t.Error("no time.Time column and stamping in pkg/crud — generated file must not import time")
	}
	// None of the old inline string-stamping machinery should survive.
	if strings.Contains(code, "IsZero()") {
		t.Error("string timestamp columns must not use time.Time's IsZero")
	}
	if strings.Contains(code, "RFC3339Nano") {
		t.Error("RFC3339Nano stamping lives in pkg/crud; no inline stamp in generated output")
	}
	if strings.Contains(code, "time.Now()") {
		t.Error("timestamp stamping lives in pkg/crud; no inline time.Now()")
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

	// The pointer-safe stamping (nil-guard, address-of-local assignment for
	// nullable *time.Time / *string managed columns) moved into pkg/crud.
	// The generator's contract is the projected pointer struct field types
	// plus the repo Spec's Timestamps flag that drives the library.
	audit := readGeneratedORM(t, root, "audit_orm.go")
	if !strings.Contains(strings.Join(strings.Fields(audit), " "), "CreatedAt *time.Time") {
		t.Fatal("nullable created_at should be *time.Time — precondition for this test")
	}
	if !strings.Contains(strings.Join(strings.Fields(audit), " "), "Timestamps: true") {
		t.Error("Audit repo Spec should set Timestamps: true")
	}
	// No inline stamping survives — neither IsZero (panic on nil) nor a
	// bare-value assignment to a pointer field (the old compile error).
	if strings.Contains(audit, "IsZero()") {
		t.Error("pointer-safe stamping lives in pkg/crud; no inline IsZero")
	}
	if strings.Contains(audit, "time.Now()") {
		t.Error("timestamp stamping lives in pkg/crud; no inline time.Now()")
	}

	legacy := readGeneratedORM(t, root, "legacy_orm.go")
	if !strings.Contains(strings.Join(strings.Fields(legacy), " "), "CreatedAt *string") {
		t.Fatal("nullable TEXT created_at should be *string — precondition for this test")
	}
	if !strings.Contains(strings.Join(strings.Fields(legacy), " "), "Timestamps: true") {
		t.Error("Legacy repo Spec should set Timestamps: true")
	}
	// A *string TEXT-timestamp entity has no time.Time column and no inline
	// stamping, so the generated file must not import time.
	if strings.Contains(legacy, "\t\"time\"\n") {
		t.Error("nullable *string timestamps need no time import (stamping in pkg/crud)")
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

	// The server-allocated-PK behaviour (exclude id from the INSERT, read
	// the DB-assigned value back via RETURNING) moved into pkg/crud, which
	// detects it off Bun's schema. The generator's contract is the struct
	// tag that tells Bun the PK is autoincrement: ,pk,autoincrement.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "Id int64 `bun:\"id,pk,autoincrement\"`") {
		t.Error("server-allocated int64 PK should carry the bun ,pk,autoincrement tag")
	}

	// None of the old inline server-allocation machinery survives.
	for _, gone := range []string{`q.ExcludeColumn("id")`, `Returning(`, "q.Exec(ctx, &msg.Id)", "SupportsReturning()", "LastInsertId()"} {
		if strings.Contains(code, gone) {
			t.Errorf("server-allocation inline %q moved to pkg/crud; should not appear in generated output", gone)
		}
	}
	// No ULID machinery for integer PKs.
	if strings.Contains(code, "ulid.") {
		t.Error("integer-PK entity must not import/use ulid")
	}

	// Create delegates to the repo.
	if !strings.Contains(code, `return hypothesisRepo.Create(ctx, db, msg, "")`) {
		t.Error("CreateHypothesis should delegate to hypothesisRepo.Create")
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

	// Like int64, an int32 SERIAL PK is server-allocated. The exclude/
	// RETURNING/scan behaviour moved into pkg/crud (detected off Bun's
	// schema); the generator's contract is the ,pk,autoincrement struct tag.
	if !strings.Contains(strings.Join(strings.Fields(code), " "), "Id int32 `bun:\"id,pk,autoincrement\"`") {
		t.Error("server-allocated int32 PK should carry the bun ,pk,autoincrement tag")
	}
	// None of the old inline server-allocation machinery survives.
	for _, gone := range []string{`q.ExcludeColumn("id")`, `Returning(`, "q.Exec(ctx, &msg.Id)", "msg.Id = int32(", "LastInsertId()"} {
		if strings.Contains(code, gone) {
			t.Errorf("server-allocation inline %q moved to pkg/crud; should not appear in generated output", gone)
		}
	}

	// Get/Delete keep the int32 PK signature.
	if !strings.Contains(code, "func GetTickByID(ctx context.Context, db orm.Context, id int32)") {
		t.Error("GetTickByID should take an int32 id")
	}
}

// TestBunTag_GeneratedColumnScanOnly proves a GENERATED ALWAYS column gets
// the ,scanonly Bun tag (Bun reads it, never writes it — postgres rejects
// writes to a generated column), and that a normal column does not.
func TestBunTag_GeneratedColumnScanOnly(t *testing.T) {
	gen := bunTag(ormField{columnName: "search_vector", isGenerated: true, notNull: true})
	if !strings.Contains(gen, "scanonly") {
		t.Errorf("generated column should carry ,scanonly; got %s", gen)
	}

	plain := bunTag(ormField{columnName: "title", notNull: true})
	if strings.Contains(plain, "scanonly") {
		t.Errorf("non-generated column must not carry ,scanonly; got %s", plain)
	}

	// A generated array column keeps ,array (for scanning) alongside ,scanonly.
	genArr := bunTag(ormField{columnName: "tags_lc", isGenerated: true, isArray: true})
	if !strings.Contains(genArr, "array") || !strings.Contains(genArr, "scanonly") {
		t.Errorf("generated array column should carry both ,array and ,scanonly; got %s", genArr)
	}
}
