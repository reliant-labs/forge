package codegen

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// reliant's machine_pools, trimmed: an excluded-literal CHECK over a column
// whose DEFAULT is that literal, and a bounds CHECK that ANDs literal bounds
// with a guarded ordering. Every create-request factory whose FK parents
// reached the table was emitted as a t.Fatalf body:
//
//	seeding the foreign-key parents failed: ERROR: new row for relation
//	"machine_pools" violates check constraint "machine_pools_selector_present"
const machinePoolParentSchema = `
CREATE TABLE machine_pools (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    name text NOT NULL,
    selector jsonb NOT NULL DEFAULT '{}'::jsonb,
    min_members integer NOT NULL DEFAULT 0,
    max_members integer NOT NULL DEFAULT 0,
    max_runs_per_member integer NOT NULL DEFAULT 0,
    scale_up text NOT NULL DEFAULT 'never',
    CONSTRAINT machine_pools_user_name_key UNIQUE (user_id, name),
    CONSTRAINT machine_pools_name_present CHECK (length(trim(name)) > 0),
    CONSTRAINT machine_pools_selector_present CHECK (selector <> '{}'::jsonb),
    CONSTRAINT machine_pools_bounds CHECK (min_members >= 0 AND max_members >= 0 AND max_runs_per_member >= 0 AND (max_members = 0 OR min_members <= max_members)),
    CONSTRAINT machine_pools_scale_up CHECK (scale_up IN ('never'))
);
CREATE TABLE plans (
    id text PRIMARY KEY,
    machine_pool_id text NOT NULL REFERENCES machine_pools (id),
    title text NOT NULL
);
`

func TestCreateRequestFactory_SeedsParentWithExcludedDefaultAndBounds_PG(t *testing.T) {
	if testing.Short() {
		t.Skip("boots real postgres; skipped under -short")
	}
	projectDir := t.TempDir()
	migDir := filepath.Join(projectDir, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "00001_init.up.sql"), []byte(machinePoolParentSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	entity := EntityDef{
		Name: "Plan", TableName: "plans", PkField: "id", PkGoType: "string",
		Fields: []EntityField{
			{Name: "id", GoName: "Id", ProtoType: "string", GoType: "string", Kind: FieldKindScalar},
			{Name: "machine_pool_id", GoName: "MachinePoolId", ProtoType: "string", GoType: "string", Kind: FieldKindScalar},
			{Name: "title", GoName: "Title", ProtoType: "string", GoType: "string", Kind: FieldKindScalar},
		},
	}
	svc := ServiceDef{
		Name: "PlanService", Package: "plan.v1", GoPackage: "example.com/test/gen/proto/services/plan/v1",
		PkgName: "planv1", ModulePath: "example.com/test",
		Messages: map[string][]MessageFieldDef{
			"CreatePlanRequest": {
				{Name: "machine_pool_id", ProtoType: "string"},
				{Name: "title", ProtoType: "string"},
			},
		},
	}
	methods := []CRUDMethod{{
		Method: MethodTemplateData{Name: "CreatePlan", InputType: "CreatePlanRequest", OutputType: "CreatePlanResponse"},
		Entity: entity, Operation: "create",
	}}
	fx, err := loadCRUDTestFixtures(projectDir, methods)
	if err != nil || fx == nil {
		t.Fatalf("loadCRUDTestFixtures: fx=%v err=%v", fx, err)
	}
	defer fx.close()
	fx.ensurePlan("plans")
	specs := buildCreateRequestSpecs(context.Background(), svc, methods, fx)
	if len(specs) != 1 {
		t.Fatalf("specs = %+v", specs)
	}
	if specs[0].failure != "" {
		t.Fatalf("NewCreatePlanRequest was emitted as a failing body:\n%s", specs[0].failure)
	}
}
