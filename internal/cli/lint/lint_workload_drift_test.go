package lint

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// scaffoldedWorkloadsProject writes the workloads.k a fresh project is born
// with — through the scaffolder itself, so the test tracks what forge really
// emits — and returns the project dir. hasFrontend adds the idp-provision job.
func scaffoldedWorkloadsProject(t *testing.T, name string, hasFrontend bool) string {
	t.Helper()
	dir := t.TempDir()
	if err := generator.ScaffoldWorkloadsKCL(dir, name, nil, hasFrontend); err != nil {
		t.Fatalf("ScaffoldWorkloadsKCL: %v", err)
	}
	return dir
}

func orphanNames(findings []componentDriftFinding) []string {
	var out []string
	for _, f := range findings {
		if !f.Undeclared {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// TestComponentDrift_ScaffoldedJobsAreNotOrphans: a freshly scaffolded
// project declares `migrate` (`db migrate up`) and, with a frontend,
// `idp-provision` (`auth idp-provision`). Both run built-in subcommands of
// the project binary, not components, so neither is drift. Before this, every
// `forge project new` linted with a warning about its own migrate job.
func TestComponentDrift_ScaffoldedJobsAreNotOrphans(t *testing.T) {
	for _, hasFrontend := range []bool{false, true} {
		dir := scaffoldedWorkloadsProject(t, "demo", hasFrontend)
		findings, err := collectComponentDrift(dir, &config.ProjectConfig{Name: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Errorf("hasFrontend=%v: fresh scaffold drift findings = %+v, want none", hasFrontend, findings)
		}
	}
}

// TestComponentDrift_GenuineOrphansStillWarn: the exemption is judged by what
// a workload RUNS, not its name. A project-binary workload selecting a
// component subcommand that no longer exists is stale; a workload named
// `migrate` that runs something else is not exempted by its name; and a
// built-in subcommand of a DIFFERENT binary is not the project's own command.
func TestComponentDrift_GenuineOrphansStillWarn(t *testing.T) {
	dir := scaffoldedWorkloadsProject(t, "demo", false)
	path := filepath.Join(dir, codegen.WorkloadsKCLRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	extra := `
billing = fw.Workload {
    name = "billing"
    kind = "service"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["billing"]
}

legacy_migrate = fw.Workload {
    name = "legacy-migrate"
    kind = "job"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["migrate-legacy"]
}

other_db = fw.Workload {
    name = "other-db"
    kind = "job"
    build = forge.GoBuild {cmd = "./cmd/other", output_name = "other"}
    args = ["db", "migrate", "up"]
}

all_in_one = fw.Workload {
    name = "all-in-one"
    kind = "service"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["server"]
}
`
	if err := os.WriteFile(path, append(raw, []byte(extra)...), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := collectComponentDrift(dir, &config.ProjectConfig{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(orphanNames(findings), ",")
	if want := "billing,legacy-migrate,other-db"; got != want {
		t.Errorf("orphan entries = %q, want %q (migrate and the all-in-one server run built-ins; the rest are drift)", got, want)
	}
}

// TestDeclaredWorkloads_ReadsBuildAndArgs pins the parser the exemption
// rests on: names, GoBuild cmd and args per literal, prose ignored.
func TestDeclaredWorkloads_ReadsBuildAndArgs(t *testing.T) {
	src := `# example = fw.Workload {name = "nats"}
migrate = fw.Workload {
    name = "migrate"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["db", "migrate", "up"]
    env = {A = "{not a brace}"}
}
nats = fw.Workload {
    name = "nats"
    image = "docker.io/library/nats:2.10"
}
`
	got := declaredWorkloads(src)
	if len(got) != 2 {
		t.Fatalf("parsed %d workloads, want 2: %+v", len(got), got)
	}
	m := got["migrate"]
	if m.buildCmd != "./cmd/demo" || strings.Join(m.args, " ") != "db migrate up" {
		t.Errorf("migrate = %+v", m)
	}
	if n := got["nats"]; n.buildCmd != "" || len(n.args) != 0 {
		t.Errorf("nats = %+v, want no build and no args", n)
	}
}
