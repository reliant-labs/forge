package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// projectService declares the CRUD quintet over projects, authenticated.
func projectService() codegen.ServiceDef {
	var ms []codegen.Method
	for _, n := range []string{"CreateProject", "GetProject", "ListProjects", "UpdateProject", "DeleteProject"} {
		ms = append(ms, codegen.Method{Name: n, AuthRequired: true})
	}
	return codegen.ServiceDef{Name: "ProjectService", Methods: ms}
}

const projectsBirthSQL = `CREATE TABLE projects (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL
);
`

// The per-table opt-out is a reasoned forge:unscoped / forge:public on the
// table: it answers the rule for that table alone, and is reported as a note
// carrying its reason.
func TestCRUDOwnership_OptOutCoversOnlyItsTable(t *testing.T) {
	for _, marker := range []string{"forge:unscoped", "forge:public"} {
		t.Run(marker, func(t *testing.T) {
			migrations := map[string]string{
				"0001_tasks.up.sql":    ownershipBirthSQL,
				"0002_projects.up.sql": projectsBirthSQL,
				"0003_optout.up.sql":   "COMMENT ON TABLE tasks IS '" + marker + ": shared by every team on purpose';\n",
			}
			fs := ownershipFindings(t, migrations, allGenerated, taskService(true), projectService())
			if len(fs) != 1 || fs[0].Table != "projects" || fs[0].severity() != lintSevError {
				t.Fatalf("the opt-out on tasks must leave projects gating, and only projects; findings = %+v", fs)
			}
			notes := ownershipOptOuts(t, migrations, allGenerated, taskService(true), projectService())
			if len(notes) != 1 || notes[0].Table != "tasks" || notes[0].Marker != marker || notes[0].Reason != "shared by every team on purpose" {
				t.Fatalf("want one opt-out note for tasks carrying its reason; notes = %+v", notes)
			}
			if notes[0].File != "db/migrations/0003_optout.up.sql" || notes[0].Line != 1 {
				t.Errorf("the note is located at %s:%d, want the COMMENT ON TABLE", notes[0].File, notes[0].Line)
			}
			for _, want := range []string{"crud-table-unowned: table tasks opts out with " + marker + ": shared by every team on purpose", "TaskService.GetTask"} {
				if !strings.Contains(notes[0].message(), want) {
					t.Errorf("note %q lacks %q", notes[0].message(), want)
				}
			}
		})
	}
}

// An opt-out with no reason records nothing, so it is an error, never an
// opt-out — on a hand-written table and on one forge just scaffolded.
func TestCRUDOwnership_OptOutWithoutAReasonIsAnError(t *testing.T) {
	for name, birth := range map[string]string{"hand-written": ownershipBirthSQL, "scaffolded": bornTasksSQL(t)} {
		for _, comment := range []string{"'forge:unscoped'", "'forge:unscoped:'", "'forge:public:   '"} {
			t.Run(name+" "+comment, func(t *testing.T) {
				migrations := map[string]string{
					"0001_tasks.up.sql":  birth,
					"0002_optout.up.sql": "COMMENT ON TABLE tasks IS " + comment + ";\n",
				}
				fs := ownershipFindings(t, migrations, allGenerated, taskService(true))
				if len(fs) != 1 || fs[0].Marker == "" || fs[0].severity() != lintSevError || !strings.Contains(fs[0].message(), "with no reason") {
					t.Fatalf("a reasonless opt-out must be an error; findings = %+v", fs)
				}
				if notes := ownershipOptOuts(t, migrations, allGenerated, taskService(true)); len(notes) != 0 {
					t.Fatalf("a reasonless opt-out must not be reported as one; notes = %+v", notes)
				}
			})
		}
	}
}

// The finding's fix names both ways out: the per-table opt-out (reason
// required) and the project-wide dial.
func TestCRUDOwnership_FixHintNamesTheOptOuts(t *testing.T) {
	fs := ownershipFindings(t, map[string]string{"0001_tasks.up.sql": ownershipBirthSQL}, allGenerated, taskService(true))
	if len(fs) != 1 {
		t.Fatalf("findings = %+v, want one", fs)
	}
	for _, want := range []string{
		"opt this table out, if it is intentionally shared",
		"COMMENT ON TABLE tasks IS 'forge:unscoped: <why>'",
		"the reason is required",
		"lint: {rules: {crud-table-unowned: warn}} (or off)",
	} {
		if !strings.Contains(fs[0].fixHint(), want) {
			t.Errorf("fix hint lacks %q: %s", want, fs[0].fixHint())
		}
	}
}

// A later COMMENT ON TABLE … IS NULL removes the opt-out: the table gates
// again, and no note claims it is still opted out.
func TestCRUDOwnership_RemovedOptOutIsNotReported(t *testing.T) {
	migrations := map[string]string{
		"0001_tasks.up.sql":     ownershipBirthSQL,
		"0002_optout.up.sql":    "COMMENT ON TABLE tasks IS 'forge:unscoped: admin service only';\n",
		"0003_uncomment.up.sql": "COMMENT ON TABLE tasks IS NULL;\n",
	}
	fs := ownershipFindings(t, migrations, allGenerated, taskService(true))
	if len(fs) != 1 || fs[0].Table != "tasks" || fs[0].Marker != "" || fs[0].severity() != lintSevError {
		t.Fatalf("a removed opt-out no longer answers the rule; findings = %+v", fs)
	}
	if notes := ownershipOptOuts(t, migrations, allGenerated, taskService(true)); len(notes) != 0 {
		t.Fatalf("a removed opt-out is not reported; notes = %+v", notes)
	}
}

// Through the lane: the opt-out is an info note ("ran", never graded), so it
// gates at no pin and under no lint.rules entry, while lint.rules still dials
// the rule's own findings project-wide.
func TestCRUDOwnership_LaneReportsOptOutsAndHonoursLintRules(t *testing.T) {
	root := writeOwnershipLaneProject(t, map[string]string{
		"0001_tasks.up.sql":  ownershipBirthSQL,
		"0002_optout.up.sql": "COMMENT ON TABLE tasks IS 'forge:unscoped: admin service only';\n",
	})
	fs, gated, err := lintStepNamed(t, "crud-ownership lint").collect(&lintRunCtx{cwd: root, cfg: &config.ProjectConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || gated || fs[0].Rule != ruleRanNote || fs[0].Severity != lintSevInfo ||
		fs[0].File != "db/migrations/0002_optout.up.sql" || !strings.Contains(fs[0].Message, "forge:unscoped: admin service only") {
		t.Fatalf("got %+v gated=%v, want one ungated opt-out note carrying the reason", fs, gated)
	}
	for _, rules := range []map[string]string{nil, {ruleCRUDTableUnowned: "error"}} {
		if got, gated := gradeProjectLane(pinnedCfg("v1.12.0", rules), fs, false); gated || len(got) != 1 || got[0].Severity != lintSevInfo {
			t.Errorf("lint.rules %v: %+v gated=%v, want the note left an ungated info", rules, got, gated)
		}
	}

	// The rule's own finding, dialled project-wide.
	root = writeOwnershipLaneProject(t, map[string]string{"0001_tasks.up.sql": ownershipBirthSQL})
	fs, gated, err = lintStepNamed(t, "crud-ownership lint").collect(&lintRunCtx{cwd: root, cfg: &config.ProjectConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Severity != lintSevError || !gated {
		t.Fatalf("got %+v gated=%v, want one gating error", fs, gated)
	}
	if got, gated := gradeProjectLane(pinnedCfg("v1.12.0", map[string]string{ruleCRUDTableUnowned: "warn"}), fs, true); gated || len(got) != 1 || got[0].Severity != lintSevWarning {
		t.Errorf("lint.rules {%s: warn}: %+v gated=%v, want an ungated warning", ruleCRUDTableUnowned, got, gated)
	}
	if got, gated := gradeProjectLane(pinnedCfg("v1.12.0", map[string]string{ruleCRUDTableUnowned: "off"}), fs, true); gated || len(got) != 0 {
		t.Errorf("lint.rules {%s: off}: %+v gated=%v, want no finding and no gate", ruleCRUDTableUnowned, got, gated)
	}
}

// writeOwnershipLaneProject writes a project whose TaskService serves tasks
// through a generated, authenticated GetTask op, over migrations.
func writeOwnershipLaneProject(t *testing.T, migrations map[string]string) string {
	t.Helper()
	root := writeGrantsProject(t, migrations)
	desc := `{"Services":[{"Name":"TaskService","Package":"services.task.v1","GoPackage":"example.com/app/gen/services/task/v1",` +
		`"ProtoFile":"proto/services/task/v1/task.proto","Methods":[` +
		`{"Name":"GetTask","InputType":"GetTaskRequest","OutputType":"GetTaskResponse","AuthRequired":true}]}]}`
	for path, body := range map[string]string{
		"gen/forge_descriptor/fixture.json": desc,
		"go.mod":                            "module example.com/app\n\ngo 1.24\n",
		"internal/handlers/task/handlers_crud_ops_gen.go": "package task\n\n" +
			"func (s *Service) crudGetTaskOp() crud.GetOp[pb.GetTaskRequest, pb.GetTaskResponse, *db.Task] {\n\treturn nil\n}\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
