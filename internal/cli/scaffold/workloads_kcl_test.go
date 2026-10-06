package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// devMainK is a dev env's main.k in the scaffolded `_workloads` shape, which
// is what bindWorkloadInEnvs appends a binding to.
const devMainK = "import workloads as wl\n\n_workloads = [\n    _on_host_job(wl.migrate)\n]\n"

// declareProject writes a project with one worker, `reaper`, the given
// workloads.k, and a dev env, then runs the scaffold's declaration step for
// reaper and returns workloads.k and dev/main.k as they are left.
func declareProject(t *testing.T, workloadsK string) (string, string) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range map[string]string{
		"internal/workers/reaper/worker.go": "package reaper\n",
		codegen.WorkloadsKCLRelPath:         workloadsK,
		"deploy/kcl/dev/main.k":             devMainK,
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declareWorkloadInKCL(root, &config.ProjectConfig{Name: "demo", ModulePath: "github.com/acme/demo"},
		componentSpec{name: "reaper", ctxLabel: "forge scaffold worker reaper"})
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	return read(codegen.WorkloadsKCLRelPath), read("deploy/kcl/dev/main.k")
}

const migrateDecl = `import forge
import forge.workloads as fw

migrate = fw.Workload {
    name = "migrate"
    kind = "job"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["db", "migrate", "up"]
}
`

// TestDeclareWorkload_PlainProjectDeclaresAndBinds is the default path: no
// grouping, so the worker gets its own declaration, its ALL entry, and a
// binding in every env.
func TestDeclareWorkload_PlainProjectDeclaresAndBinds(t *testing.T) {
	workloads, dev := declareProject(t, migrateDecl+"\nALL: [fw.Workload] = [\n    migrate,\n]\n")
	if !strings.Contains(workloads, "reaper = fw.Workload {") || !strings.Contains(workloads, "    migrate,\n    reaper,\n]") {
		t.Errorf("workloads.k not declared + listed:\n%s", workloads)
	}
	if !strings.Contains(dev, "wl.reaper") {
		t.Errorf("dev/main.k not bound:\n%s", dev)
	}
}

// TestDeclareWorkload_ServedComponentIsNotDeclared: a worker another
// workload already runs (`serves`) gets no workload of its own — one would
// deploy it twice — and no env binding to a workload that does not exist.
func TestDeclareWorkload_ServedComponentIsNotDeclared(t *testing.T) {
	in := migrateDecl + `
background = fw.Workload {
    name = "background"
    kind = "worker"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["workers"]
    serves = ["reaper"]
}

ALL: [fw.Workload] = [migrate, background]
`
	workloads, dev := declareProject(t, in)
	if workloads != in {
		t.Errorf("workloads.k changed for a served component:\n%s", workloads)
	}
	if dev != devMainK {
		t.Errorf("dev/main.k changed for a served component:\n%s", dev)
	}
}

// TestDeclareWorkload_GroupingProjectShowsTheChoice: in a project that groups
// components with `serves`, a component in no group may still belong to one
// — the mount set lives in Go — so nothing is written; the choice is printed.
func TestDeclareWorkload_GroupingProjectShowsTheChoice(t *testing.T) {
	in := migrateDecl + `
admin_api = fw.Workload {
    name = "admin-api"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["admin-api"]
    serves = ["billing-admin"]
}

ALL: [fw.Workload] = [migrate, admin_api]
`
	workloads, dev := declareProject(t, in)
	if workloads != in || dev != devMainK {
		t.Errorf("files changed in a grouping project:\n%s\n---\n%s", workloads, dev)
	}
}

// TestDeclareWorkload_UneditableListBindsNothing: when ALL cannot be edited
// the stanza is printed, not written — so no env may be bound to it. Binding
// `wl.reaper` there would leave every env's main.k naming a workload that
// does not exist.
func TestDeclareWorkload_UneditableListBindsNothing(t *testing.T) {
	in := migrateDecl + "\nALL: [fw.Workload] = [migrate]\nALL: [fw.Workload] = [migrate]\n"
	workloads, dev := declareProject(t, in)
	if workloads != in {
		t.Errorf("workloads.k changed despite an ambiguous ALL:\n%s", workloads)
	}
	if dev != devMainK {
		t.Errorf("dev/main.k bound a workload that was never declared:\n%s", dev)
	}
}
