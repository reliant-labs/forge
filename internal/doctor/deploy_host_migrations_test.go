package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// CheckDeployMigrations asks "does this environment have SOME way to apply its
// migrations", and answers by reading the rendered k8s manifests: a migration
// Job, an initContainer, a migrate command, or AUTO_MIGRATE=true.
//
// That misses the app entirely in a HOST-mode environment. A host env
// (forge.Service with a `host` block) runs its services as processes on the
// developer's machine and gives the cluster only the pieces that must be in
// it — an operator that needs a projected SA token, a proxy sidecar. Those
// in-cluster pieces are real containers sharing the same database, so the env
// genuinely has a schema to keep current; the thing that KEEPS it current is
// the host service's AUTO_MIGRATE=true, which never becomes a container.
//
// The result was a check reporting an environment that migrates on every boot
// as one where "a schema-changing release deploys new code against the old
// schema", with a fix instructing the author to add a migration Job to an
// environment whose app does not run in the cluster.
//
// A host service is visible in the `output` JSON contract — the same document
// `forge env deploy` consumes — so the fix is to read it, and to judge it by
// the same two mechanisms the manifest path accepts.
func TestCheckDeployMigrations_HostServiceWithAutoMigrateCounts(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir)

	// A host-mode render: an in-cluster operator (a real container, no
	// migration step) plus the host-deployed app that carries AUTO_MIGRATE.
	body := `{
	  "manifests":[
	    {"apiVersion":"apps/v1","kind":"Deployment",
	     "metadata":{"name":"controller","namespace":"dev"},
	     "spec":{"template":{"spec":{"containers":[{"name":"controller","image":"c:1"}]}}}}
	  ],
	  "output":{"services":[
	    {"name":"admin-server","command":["server"],
	     "deploy":{"type":"host"},
	     "env_vars":[{"name":"AUTO_MIGRATE","value":"true"}]}
	  ]}
	}`

	env := envWithRender([]envRender{renderFromJSON(t, "dev", body)})
	env.ProjectDir = dir

	got := CheckDeployMigrations(context.Background(), env)
	if got.Status == StatusFail {
		t.Fatalf("a HOST service with AUTO_MIGRATE=true applies migrations on every "+
			"boot — reporting the env unmigrated sends someone to add a Job to an "+
			"environment whose app does not run in the cluster.\nmessage: %s\nevidence: %s",
			got.Message, got.Evidence)
	}
}

// The shape a REAL render actually emits, and the one a first cut of this fix
// missed: a host service's environment is composed onto its DEPLOY block, the
// way a cluster service's env lands on the container. Its own top-level
// `env_vars` is empty. Reading only the outer list finds nothing on every host
// service in every project, so the fix would have looked right and changed
// nothing.
func TestCheckDeployMigrations_HostEnvLivesOnTheDeployBlock(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir)

	body := `{
	  "manifests":[
	    {"apiVersion":"apps/v1","kind":"Deployment",
	     "metadata":{"name":"controller","namespace":"dev"},
	     "spec":{"template":{"spec":{"containers":[{"name":"controller","image":"c:1"}]}}}}
	  ],
	  "output":{"services":[
	    {"name":"admin-server","command":["./control-plane","server"],"env_vars":[],
	     "deploy":{"type":"host","runner":"air",
	       "env_vars":[{"name":"PORT","value":"8090"},
	                   {"name":"AUTO_MIGRATE","value":"true"}]}}
	  ]}
	}`

	env := envWithRender([]envRender{renderFromJSON(t, "dev", body)})
	env.ProjectDir = dir

	if got := CheckDeployMigrations(context.Background(), env); got.Status == StatusFail {
		t.Fatalf("AUTO_MIGRATE on the host deploy block is where a real render puts "+
			"it — this is the shape that must count.\nevidence: %s", got.Evidence)
	}
}

// A host service whose COMMAND is the migrate step counts too — the same
// mechanism the manifest path accepts via a container command.
func TestCheckDeployMigrations_HostMigrateCommandCounts(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir)

	body := `{
	  "manifests":[
	    {"apiVersion":"apps/v1","kind":"Deployment",
	     "metadata":{"name":"controller","namespace":"dev"},
	     "spec":{"template":{"spec":{"containers":[{"name":"controller","image":"c:1"}]}}}}
	  ],
	  "output":{"services":[
	    {"name":"schema","command":["/app/cp","db","migrate","up"],
	     "deploy":{"type":"host"},"env_vars":[]}
	  ]}
	}`

	env := envWithRender([]envRender{renderFromJSON(t, "dev", body)})
	env.ProjectDir = dir

	if got := CheckDeployMigrations(context.Background(), env); got.Status == StatusFail {
		t.Fatalf("a host service running `db migrate up` is a migration step: %s", got.Evidence)
	}
}

// The guard on the other side. Widening must not blunt the check:
//
//   - a host service with AUTO_MIGRATE explicitly FALSE is not a migration step
//   - a CLUSTER-deployed service is judged by its manifest, not by the contract,
//     so putting AUTO_MIGRATE on a non-host entry must not excuse the env
func TestCheckDeployMigrations_StillFailsWithoutARealStep(t *testing.T) {
	dir := t.TempDir()
	writeMigrationFile(t, dir)

	clusterDeployment := `{"apiVersion":"apps/v1","kind":"Deployment",
	     "metadata":{"name":"api","namespace":"prod"},
	     "spec":{"template":{"spec":{"containers":[{"name":"api","image":"api:1"}]}}}}`

	for _, tt := range []struct {
		name string
		body string
	}{
		{
			name: "host service with AUTO_MIGRATE=false",
			body: `{"manifests":[` + clusterDeployment + `],"output":{"services":[` +
				`{"name":"app","command":["server"],"deploy":{"type":"host"},` +
				`"env_vars":[{"name":"AUTO_MIGRATE","value":"false"}]}]}}`,
		},
		{
			name: "AUTO_MIGRATE on a cluster service in the contract",
			body: `{"manifests":[` + clusterDeployment + `],"output":{"services":[` +
				`{"name":"api","command":["server"],"deploy":{"type":"cluster"},` +
				`"env_vars":[{"name":"AUTO_MIGRATE","value":"true"}]}]}}`,
		},
		{
			name: "no contract at all",
			body: `{"manifests":[` + clusterDeployment + `]}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := envWithRender([]envRender{renderFromJSON(t, "prod", tt.body)})
			env.ProjectDir = dir
			got := CheckDeployMigrations(context.Background(), env)
			if got.Status != StatusFail {
				t.Fatalf("an env that runs its app in-cluster with no migration step must "+
					"still fail — that is a release deploying new code against the old "+
					"schema. status = %q (%s)", got.Status, got.Message)
			}
		})
	}
}

func writeMigrationFile(t *testing.T, projectDir string) {
	t.Helper()
	migDir := filepath.Join(projectDir, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "0001_init.up.sql"),
		[]byte("CREATE TABLE t (id int);"), 0o644); err != nil {
		t.Fatal(err)
	}
}
