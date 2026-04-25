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

	if err := GeneratePlanORM(root, "github.com/test/myapp", "api", entities); err != nil {
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

	// Column list
	if !strings.Contains(code, "var projectColumns = []string{") {
		t.Error("missing projectColumns var")
	}

	// Scan function (standalone)
	if !strings.Contains(code, "func scanProject(scanner interface{ Scan(...interface{}) error }) (*Project, error) {") {
		t.Error("missing scanProject function")
	}
	if !strings.Contains(code, "sql.NullTime") {
		t.Error("missing sql.NullTime in scan function for timestamps")
	}
	if !strings.Contains(code, "timestamppb.New(") {
		t.Error("missing timestamppb.New in scan function")
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

	// Soft delete in List
	if !strings.Contains(code, `orm.WhereIsNull("deleted_at")`) {
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

	// Create uses inline SQL with upsert
	if !strings.Contains(code, "INSERT INTO") {
		t.Error("missing INSERT INTO in Create")
	}
	if !strings.Contains(code, "ON CONFLICT") {
		t.Error("missing ON CONFLICT in Create")
	}

	// List uses QueryBuilder
	if !strings.Contains(code, "orm.NewQueryBuilder(db,") {
		t.Error("missing orm.NewQueryBuilder in List/Get")
	}
	if !strings.Contains(code, "scanProject(rows)") {
		t.Error("missing scanProject(rows) in List")
	}

	// Field constants
	if !strings.Contains(code, `ProjectFieldId = "id"`) {
		t.Error("missing field constant for Id")
	}
	if !strings.Contains(code, `ProjectFieldOrgId = "org_id"`) {
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

	if err := GeneratePlanORM(root, "github.com/test/myapp", "api", entities); err != nil {
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

	// Simple delete uses inline SQL DELETE
	if !strings.Contains(code, "DELETE FROM") {
		t.Error("simple delete should use DELETE FROM")
	}

	// Simple update uses inline SQL UPDATE
	if !strings.Contains(code, "UPDATE %s SET %s WHERE") {
		t.Error("simple update should use inline UPDATE")
	}

	// GetByID uses QueryBuilder (no soft-delete/tenant)
	if !strings.Contains(code, "orm.NewQueryBuilder(db,") {
		t.Error("simple GetByID should use orm.NewQueryBuilder")
	}
	if !strings.Contains(code, "scanTag(row)") {
		t.Error("simple GetByID should use scanTag(row)")
	}

	// Standalone scan function
	if !strings.Contains(code, "func scanTag(scanner interface{ Scan(...interface{}) error }) (*Tag, error) {") {
		t.Error("missing scanTag function")
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

	if err := GeneratePlanORM(root, "github.com/test/myapp", "api", entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "item_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// Soft-delete filter in List
	if !strings.Contains(code, `orm.WhereIsNull("deleted_at")`) {
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

	// GetByID should use List (because soft-delete)
	if !strings.Contains(code, "ListItem(ctx, db,") {
		t.Error("GetByID should delegate to List for soft-delete")
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
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
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
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

	// Scan function with nullable fields
	if !strings.Contains(code, "dbScore *float32") {
		t.Error("nullable float should scan as *float32")
	}
	if !strings.Contains(code, "dbPrice *float64") {
		t.Error("nullable double should scan as *float64")
	}
	if !strings.Contains(code, "dbLabel *string") {
		t.Error("nullable string should scan as *string")
	}

	// scanRecord function exists
	if !strings.Contains(code, "func scanRecord(scanner") {
		t.Error("missing scanRecord function")
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "event_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// Should have timestamp imports
	if !strings.Contains(code, `"database/sql"`) {
		t.Error("missing database/sql import for timestamps")
	}
	if !strings.Contains(code, "timestamppb") {
		t.Error("missing timestamppb import")
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

	// sql.NullTime used for timestamps in scan
	if !strings.Contains(code, "sql.NullTime") {
		t.Error("missing sql.NullTime for timestamp scanning")
	}
}

func TestGeneratePlanORM_Empty(t *testing.T) {
	root := t.TempDir()

	if err := GeneratePlanORM(root, "example.com/app", "api", nil); err != nil {
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "comment_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// Should have scan function, CRUD functions, etc.
	if !strings.Contains(code, "func scanComment(") {
		t.Error("missing scanComment function")
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
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

	// Update with tenant but no soft-delete
	if !strings.Contains(code, `"UPDATE %s SET %s WHERE %s = %s AND %s = %s"`) {
		t.Error("Update should have tenant WHERE but no soft-delete clause")
	}

	// Delete with tenant but no soft-delete (hard delete)
	if !strings.Contains(code, `"DELETE FROM %s WHERE %s = %s AND %s = %s"`) {
		t.Error("Delete should be hard delete with tenant isolation")
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "widget_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// id should be in field constants
	if !strings.Contains(code, `WidgetFieldId = "id"`) {
		t.Error("missing auto-generated id field constant")
	}

	// id should be the first column in the ordered list
	colIdx := strings.Index(code, "var widgetColumns = []string{")
	if colIdx == -1 {
		t.Fatal("missing widgetColumns")
	}
	colSection := code[colIdx : colIdx+200]
	idIdx := strings.Index(colSection, `"id"`)
	nameIdx := strings.Index(colSection, `"name"`)
	if idIdx == -1 {
		t.Fatal("id not found in widgetColumns")
	}
	if idIdx >= nameIdx {
		t.Error("id should appear before name in widgetColumns")
	}

	// scan function should include id
	if !strings.Contains(code, "&entity.Id,") {
		t.Error("scanWidget should scan entity.Id")
	}

	// CRUD functions should use string PK type
	if !strings.Contains(code, "func GetWidgetByID(ctx context.Context, db orm.Context, id string)") {
		t.Error("GetWidgetByID should use string type for auto-generated id")
	}
	if !strings.Contains(code, "func DeleteWidget(ctx context.Context, db orm.Context, id string)") {
		t.Error("DeleteWidget should use string type for auto-generated id")
	}

	// ON CONFLICT (id) should work
	if !strings.Contains(code, "ON CONFLICT") {
		t.Error("Create should have ON CONFLICT for upsert")
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
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

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
		t.Fatalf("error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "internal", "db", "task_orm.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	code := string(content)

	// The SET clause should include title, created_at, updated_at but NOT id, org_id, deleted_at
	updateIdx := strings.Index(code, "func UpdateTask(")
	if updateIdx == -1 {
		t.Fatal("missing UpdateTask function")
	}
	updateCode := code[updateIdx:]

	// Should set title
	if !strings.Contains(updateCode, `QuoteIdentifier("title")`) {
		t.Error("Update SET should include title")
	}

	// Should NOT set id in SET (it's the PK)
	setPartsIdx := strings.Index(updateCode, "setParts := []string{")
	if setPartsIdx == -1 {
		t.Fatal("missing setParts in Update")
	}
	setPartsEnd := strings.Index(updateCode[setPartsIdx:], "}")
	setPartsSection := updateCode[setPartsIdx : setPartsIdx+setPartsEnd]

	if strings.Contains(setPartsSection, `"id"`) {
		t.Error("Update SET should not include PK (id)")
	}
	if strings.Contains(setPartsSection, `"deleted_at"`) {
		t.Error("Update SET should not include deleted_at")
	}
	if strings.Contains(setPartsSection, `"org_id"`) {
		t.Error("Update SET should not include tenant key (org_id)")
	}
}