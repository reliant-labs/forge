package generator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestZZDump(t *testing.T) {
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
				{Name: "tags", Type: "[]string", NotNull: true},
				{Name: "nums", Type: "[]int64"},
			},
		},
		{
			Name:       "Event",
			Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true},
				{Name: "name", Type: "string", NotNull: true},
			},
		},
	}
	if err := GeneratePlanORM(root, "github.com/test/myapp", "api", entities); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"project_orm.go", "record_orm.go", "event_orm.go"} {
		b, err := os.ReadFile(filepath.Join(root, "internal", "db", f))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join("/tmp/ormdump_"+f), b, 0o644)
	}
}
