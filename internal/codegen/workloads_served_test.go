package codegen

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestDeclaredWorkloads_ReadsBuildArgsAndServes pins the parser both
// `forge lint` and `forge scaffold` read workloads.k through: names, GoBuild
// cmd (inline, or bound to a top-level variable) and args per literal,
// `serves` with comments and brackets in it, prose ignored.
func TestDeclaredWorkloads_ReadsBuildArgsAndServes(t *testing.T) {
	src := `# example = fw.Workload {name = "nats"}
_demo_build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}

migrate = fw.Workload {
    name = "migrate"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["db", "migrate", "up"]
    env = {A = "{not a brace}"}
}
admin_api = fw.Workload {
    name = "admin-api"
    build = _demo_build
    args = ["admin-api"]
    serves = [
        "billing-admin",  # [legacy] name, kept
        # "retired-admin",
        "user-admin"
    ]
}
nats = fw.Workload {
    name = "nats"
    image = "docker.io/library/nats:2.10"
}
`
	got := DeclaredWorkloads(src)
	if len(got) != 3 {
		t.Fatalf("parsed %d workloads, want 3: %+v", len(got), got)
	}
	if m := got["migrate"]; m.BuildCmd != "./cmd/demo" || strings.Join(m.Args, " ") != "db migrate up" || m.Serves != nil {
		t.Errorf("migrate = %+v", m)
	}
	a := got["admin-api"]
	if a.BuildCmd != "./cmd/demo" {
		t.Errorf("admin-api build = %q, want the bound _demo_build's cmd", a.BuildCmd)
	}
	if strings.Join(a.Serves, ",") != "billing-admin,user-admin" {
		t.Errorf("admin-api serves = %v, want [billing-admin user-admin] (the commented-out entry is not one)", a.Serves)
	}
	if n := got["nats"]; n.BuildCmd != "" || len(n.Args) != 0 {
		t.Errorf("nats = %+v, want no build and no args", n)
	}
}

// TestServingWorkload: a component is run by another workload when that
// workload's `serves` names it (in either spelling) or when the project binary
// runs its all-in-one `server` — but never a secondary binary, which is its
// own program, and never a `server` of some OTHER binary.
func TestServingWorkload(t *testing.T) {
	const src = `
admin_api = fw.Workload {
    name = "admin-api"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["admin-api"]
    serves = ["overview-admin"]
}
other = fw.Workload {
    name = "other"
    build = forge.GoBuild {cmd = "./cmd/other", output_name = "other"}
    args = ["server"]
}
`
	declared := DeclaredWorkloads(src)
	for _, tc := range []struct {
		comp   config.ComponentConfig
		wantBy string
	}{
		{config.ComponentConfig{Name: "overview-admin", Kind: config.ComponentKindServer}, "admin-api"},
		{config.ComponentConfig{Name: "overview_admin", Kind: config.ComponentKindServer}, "admin-api"},
		{config.ComponentConfig{Name: "billing", Kind: config.ComponentKindServer}, ""},
	} {
		by, ok := ServingWorkload(declared, "demo", tc.comp)
		if by != tc.wantBy || ok != (tc.wantBy != "") {
			t.Errorf("ServingWorkload(%s) = %q, %v; want %q", tc.comp.Name, by, ok, tc.wantBy)
		}
	}

	allInOne := DeclaredWorkloads(src + `
app = fw.Workload {
    name = "app"
    build = forge.GoBuild {cmd = "./cmd/demo", output_name = "demo"}
    args = ["server"]
}
`)
	for _, kind := range []string{config.ComponentKindServer, config.ComponentKindWorker, config.ComponentKindOperator} {
		if by, _ := ServingWorkload(allInOne, "demo", config.ComponentConfig{Name: "reaper", Kind: kind}); by != "app" {
			t.Errorf("%s under an all-in-one server: served by %q, want app", kind, by)
		}
	}
	if by, ok := ServingWorkload(allInOne, "demo", config.ComponentConfig{Name: "tool", Kind: config.ComponentKindBinary}); ok {
		t.Errorf("a secondary binary is its own program, but was reported served by %q", by)
	}
}

// TestDeclaresServes: the project's own statement that it groups components
// — any `serves`, even an empty one — and nothing else counts as one.
func TestDeclaresServes(t *testing.T) {
	plain := "billing = fw.Workload {\n    name = \"billing\"\n    args = [\"billing\"]\n}\n"
	if DeclaresServes(DeclaredWorkloads(plain)) {
		t.Error("a file with no serves reported as grouping")
	}
	if DeclaresServes(DeclaredWorkloads("# serves = [\"x\"]\n" + plain)) {
		t.Error("a serves in a comment reported as grouping")
	}
	grouped := "api = fw.Workload {\n    name = \"api\"\n    args = [\"api\"]\n    serves = []\n}\n"
	if !DeclaresServes(DeclaredWorkloads(plain + grouped)) {
		t.Error("an explicit serves = [] not reported as grouping")
	}
}
