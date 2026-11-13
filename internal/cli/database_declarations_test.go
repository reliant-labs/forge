package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// The render contract carries each database's schema, and the decoder reads
// it onto the entity that declares it: a managed one (hosted), a schema-only
// one (mixed). A database that declares no migrations decodes with none.
func TestRenderContract_DatabaseSchemasDecode(t *testing.T) {
	hosted, _ := loadContract(t, "hosted")
	if len(hosted.Databases) != 1 {
		t.Fatalf("hosted: %d databases, want 1", len(hosted.Databases))
	}
	db := hosted.Databases[0]
	want := DatabaseSchemaEntity{
		Dialect:    "postgres",
		Migrations: &DatabaseMigrationsEntity{Dir: "db/migrations", Driver: "forge"},
		ORM:        &DatabaseORMEntity{Package: "internal/db"},
	}
	if !reflect.DeepEqual(db.DatabaseSchemaEntity, want) {
		t.Errorf("hosted database schema = %+v, want %+v", db.DatabaseSchemaEntity, want)
	}

	mixed, _ := loadContract(t, "mixed")
	if len(mixed.SchemaOnlyDatabases) != 1 || mixed.SchemaOnlyDatabases[0].Name != "analytics" {
		t.Fatalf("mixed: schema_only_databases = %+v, want analytics", mixed.SchemaOnlyDatabases)
	}
	if got := mixed.SchemaOnlyDatabases[0].Migrations; got == nil || got.Driver != "goose" || got.Dir != "analytics/migrations" {
		t.Errorf("mixed: analytics migrations = %+v", got)
	}

	cluster, _ := loadContract(t, "cluster")
	for _, d := range cluster.Databases {
		if d.Migrations != nil || d.Dialect != "" || d.ORM != nil {
			t.Errorf("cluster: database %s declares no migrations but decoded a schema %+v", d.Name, d.DatabaseSchemaEntity)
		}
	}
}

func schemaEnv(dbs ...DatabaseEntity) *KCLEntities { return &KCLEntities{Databases: dbs} }

func schemaMigrations(dir, driver string) DatabaseSchemaEntity {
	return DatabaseSchemaEntity{Dialect: "postgres", Migrations: &DatabaseMigrationsEntity{Dir: dir, Driver: driver}}
}

// The union reads every env: a database one env declares is the project's,
// envs that agree (in any spelling) merge, and a database that declares no
// migrations is not a schema declaration.
func TestUnionDeclaredDatabases_MergesAgreeingEnvs(t *testing.T) {
	got, err := unionDeclaredDatabases(map[string]*KCLEntities{
		"dev":  schemaEnv(DatabaseEntity{Name: "default", DatabaseSchemaEntity: schemaMigrations("./db/migrations/", "forge")}),
		"prod": schemaEnv(DatabaseEntity{Name: "default", DatabaseSchemaEntity: schemaMigrations("db/migrations", "")}, DatabaseEntity{Name: "cache"}),
		"e2e": {SchemaOnlyDatabases: []SchemaOnlyDatabaseEntity{{
			Name: "analytics",
			DatabaseSchemaEntity: DatabaseSchemaEntity{
				Migrations: &DatabaseMigrationsEntity{Dir: "analytics", Driver: "goose"},
				ORM:        &DatabaseORMEntity{Package: "internal/analyticsdb/"},
			},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d databases, want analytics and default: %+v", len(got), got)
	}
	a, d := got[0], got[1]
	if a.Name != "analytics" || a.Migrations.Tool != "goose" || a.ORM.Package != "internal/analyticsdb" || a.Dialect != "postgres" {
		t.Errorf("analytics = %+v", a)
	}
	if d.Name != "default" || d.MigrationsPath() != "db/migrations" || d.Migrations.Tool != "forge" {
		t.Errorf("default = %+v", d)
	}
	if !reflect.DeepEqual(d.Envs, []string{"dev", "prod"}) || d.Source != config.DatabaseSourceKCL {
		t.Errorf("default envs/source = %v/%s", d.Envs, d.Source)
	}
}

func TestUnionDeclaredDatabases_RefusesDisagreeingEnvs(t *testing.T) {
	_, err := unionDeclaredDatabases(map[string]*KCLEntities{
		"dev":  schemaEnv(DatabaseEntity{Name: "app", DatabaseSchemaEntity: schemaMigrations("db/migrations", "forge")}),
		"prod": schemaEnv(DatabaseEntity{Name: "app", DatabaseSchemaEntity: schemaMigrations("migrations", "goose")}),
	})
	if err == nil || !strings.Contains(err.Error(), `database "app" declares a different schema in env prod than in env dev`) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnionDeclaredDatabases_RefusesSharedDirAcrossEnvs(t *testing.T) {
	_, err := unionDeclaredDatabases(map[string]*KCLEntities{
		"dev":  schemaEnv(DatabaseEntity{Name: "a", DatabaseSchemaEntity: schemaMigrations("db/migrations", "forge")}),
		"prod": schemaEnv(DatabaseEntity{Name: "b", DatabaseSchemaEntity: schemaMigrations("./db/migrations", "forge")}),
	})
	if err == nil || !strings.Contains(err.Error(), "both declare migrations dir db/migrations") {
		t.Fatalf("err = %v", err)
	}
}

// writeProject lays out a project with forge.yaml body yaml and, per env, a
// deploy/kcl/<env>/main.k holding kcl.
func writeDBProject(t *testing.T, yaml string, kcl map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if yaml != "" {
		if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for env, body := range kcl {
		p := filepath.Join(dir, "deploy", "kcl", env, "main.k")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// stubDeclarer installs a declarer that answers dbs for every project.
func stubDeclarer(t *testing.T, dbs []config.DeclaredDatabase, err error) {
	t.Helper()
	prev := config.RegisterDatabaseDeclarer(func(string) ([]config.DeclaredDatabase, error) { return dbs, err })
	t.Cleanup(func() { config.RegisterDatabaseDeclarer(prev) })
}

// Precedence: with no declaration the project is its forge.yaml; with one,
// the declaration wins and forge.yaml's orm fills the primary; with
// migrations in both, it is an error.
func TestProjectDatabases_Precedence(t *testing.T) {
	kclDefault := config.DeclaredDatabase{Name: "default", Dialect: "postgres", Source: config.DatabaseSourceKCL,
		Migrations: config.MigrationsConfig{Dir: "sql", Tool: "goose"}}

	t.Run("legacy only", func(t *testing.T) {
		stubDeclarer(t, nil, nil)
		dir := writeDBProject(t, "name: x\ndatabase:\n  migrations_dir: sql/migrations\n", nil)
		dbs, err := config.ProjectDatabases(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(dbs) != 1 || dbs[0].Name != "default" || dbs[0].MigrationsPath() != "sql/migrations" || dbs[0].Source != config.DatabaseSourceForgeYAML {
			t.Fatalf("dbs = %+v", dbs)
		}
		if got := config.MigrationsDirIn(dir); got != "sql/migrations" {
			t.Errorf("MigrationsDirIn = %s", got)
		}
	})

	t.Run("kcl wins, forge.yaml orm fills the primary", func(t *testing.T) {
		stubDeclarer(t, []config.DeclaredDatabase{kclDefault}, nil)
		dir := writeDBProject(t, "name: x\ndatabase:\n  orm:\n    package: internal/store\n", nil)
		dbs, err := config.ProjectDatabases(dir)
		if err != nil {
			t.Fatal(err)
		}
		if dbs[0].ORM.Package != "internal/store" {
			t.Errorf("forge.yaml orm did not fill the primary: %+v", dbs[0])
		}
		if got := config.MigrationsDirIn(dir); got != "sql" {
			t.Errorf("MigrationsDirIn = %s, want the declaration's sql", got)
		}
		if got := config.MigrationToolIn(dir); got != "goose" {
			t.Errorf("MigrationToolIn = %s, want goose", got)
		}
	})

	t.Run("both declare migrations", func(t *testing.T) {
		stubDeclarer(t, []config.DeclaredDatabase{kclDefault}, nil)
		dir := writeDBProject(t, "name: x\ndatabase:\n  migrations:\n    dir: db/migrations\n", nil)
		_, err := config.ProjectDatabases(dir)
		if !errors.Is(err, config.ErrDatabaseDeclaredTwice) {
			t.Fatalf("err = %v, want ErrDatabaseDeclaredTwice", err)
		}
		// A leaf reader cannot fail: it keeps forge.yaml's answer, and the
		// command that reads databases reports the error.
		if got := config.MigrationsDirIn(dir); got != "db/migrations" {
			t.Errorf("MigrationsDirIn = %s", got)
		}
	})
}

func TestSelectDatabase(t *testing.T) {
	a := config.DeclaredDatabase{Name: "analytics"}
	b := config.DeclaredDatabase{Name: "ledger"}
	def := config.DeclaredDatabase{Name: "default"}
	if d, err := config.SelectDatabase([]config.DeclaredDatabase{a}, ""); err != nil || d.Name != "analytics" {
		t.Errorf("the only database is the primary: %v %v", d, err)
	}
	if d, err := config.SelectDatabase([]config.DeclaredDatabase{a, def}, ""); err != nil || d.Name != "default" {
		t.Errorf("default is the primary: %v %v", d, err)
	}
	if _, err := config.SelectDatabase([]config.DeclaredDatabase{a, b}, ""); err == nil || !strings.Contains(err.Error(), "pass --db <name>") {
		t.Errorf("ambiguous: %v", err)
	}
	if d, err := config.SelectDatabase([]config.DeclaredDatabase{a, b}, "ledger"); err != nil || d.Name != "ledger" {
		t.Errorf("named: %v %v", d, err)
	}
	if _, err := config.SelectDatabase([]config.DeclaredDatabase{a, b}, "nope"); err == nil || !strings.Contains(err.Error(), "analytics, ledger") {
		t.Errorf("unknown: %v", err)
	}
}

// The pre-check finds a declaration wherever the project's KCL spells one,
// and finds none in a project that predates them — which therefore
// evaluates nothing.
func TestKCLMayDeclareDatabaseSchemas(t *testing.T) {
	none := writeDBProject(t, "", map[string]string{"dev": "import forge\n# migrations = later\noutput = forge.render(forge.Bundle {project = \"x\", databases = [forge.ManagedDatabase {name = \"db\"}]})\n"})
	if kclMayDeclareDatabaseSchemas(none) {
		t.Error("a project with no schema declaration (a commented one included) matched")
	}
	some := writeDBProject(t, "", map[string]string{"prod": "_db = forge.ManagedDatabase {\n    name = \"db\"\n    migrations = forge.DatabaseMigrations {}\n}\n"})
	if !kclMayDeclareDatabaseSchemas(some) {
		t.Error("a declaration was missed")
	}
	if kclMayDeclareDatabaseSchemas(t.TempDir()) {
		t.Error("a project with no deploy/kcl matched")
	}
	// With no deploy/kcl the declarer reads nothing: the project is its
	// forge.yaml.
	if dbs, err := declaredDatabasesOfEnvs(none); err != nil || dbs != nil {
		t.Errorf("declarer on a non-declaring project = %v, %v", dbs, err)
	}
}
