// cmd_tree_tier_test.go — the tier VERDICT for each cmd/<bin>/cmd file,
// asserted by rendering it against materially different user inputs.
//
// THE RULE BEING ENFORCED. Tier-1 (regenerated every run, behind a "do not
// edit" banner) is legitimate ONLY for a file re-derived from something the
// user wrote — a proto, a SQL migration, contract.go, forge.yaml. A generated
// file that comes out byte-identical regardless of user input is library code
// parked in the user's tree: regenerating it cannot keep it correct, because it
// was never a function of the inputs, and it carries a real cost — a user who
// edits it puts it in PERMANENT DRIFT, so every future forge improvement
// silently stops arriving.
//
// So the two directions are asserted separately, and both matter:
//
//   - A Tier-1 file must PROVE it responds to user input. root.go and
//     db_source.go each do, and the test names which input.
//   - A scaffold-once file must prove it does NOT vary, because it is written
//     once and never re-rendered: a conditional in it freezes one branch
//     forever with no later run able to correct it.
//
// These render the templates directly rather than driving a full project,
// which is what lets each case vary exactly ONE input. A whole-project
// differential (internal/tierguard) is the broader check, but a fixture pair
// that happens to hold an input constant reports "constant" for a file that
// genuinely tracks it — precisely the false positive this file rules out for
// the command tree.
package templates_test

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// cmdTreeData is the render payload the cmd-tree templates read. Every field
// corresponds to something the USER controls: the forge.yaml project name and
// module path, the config fields parsed out of their config proto, and whether
// db/migrations/ holds any SQL.
type cmdTreeData struct {
	Module        string
	Name          string
	ConfigFields  map[string]bool
	HasMigrations bool
}

func renderCmdTree(t *testing.T, tmpl string, d cmdTreeData) string {
	t.Helper()
	out, err := templates.ProjectTemplates().Render(tmpl, d)
	if err != nil {
		t.Fatalf("render %s: %v", tmpl, err)
	}
	return string(out)
}

// TestCmdTreeRootIsGenuinelyInputDerived is the evidence for leaving root.go
// Tier-1 while its neighbours were flipped to scaffold-once.
//
// Two independent inputs move its content, and a file that responds to user
// input is exactly what Tier-1 is for — re-deriving it on every run keeps it
// correct, which is a thing that can only be said of a file that is a function
// of the inputs.
func TestCmdTreeRootIsGenuinelyInputDerived(t *testing.T) {
	const tmpl = "cmd-tree-root.go.tmpl"

	base := cmdTreeData{
		Module:       "example.com/demo",
		Name:         "demo",
		ConfigFields: map[string]bool{"AutoMigrate": true},
	}

	// INPUT 1: the forge.yaml project name, which becomes the ServiceName
	// constant (this binary's OTel service.name identity) and the root
	// command's own Use string.
	renamed := base
	renamed.Name = "payments"
	if renderCmdTree(t, tmpl, base) == renderCmdTree(t, tmpl, renamed) {
		t.Error("cmd-tree-root.go.tmpl renders identically for project names \"demo\" and " +
			"\"payments\"; it is supposed to bake the forge.yaml name into ServiceName")
	}
	if got := renderCmdTree(t, tmpl, renamed); !strings.Contains(got, `const ServiceName = "payments"`) {
		t.Errorf("rendered root.go does not carry the project name as ServiceName:\n%s", got)
	}

	// INPUT 2: the config proto. Whether the user's config declares an
	// AutoMigrate field decides whether the root command wires up the `db`
	// subtree at all — wiring newDBCmd for a project that has no migration
	// config would reference a command that was never scaffolded.
	noMigrate := base
	noMigrate.ConfigFields = map[string]bool{}
	withDB, withoutDB := renderCmdTree(t, tmpl, base), renderCmdTree(t, tmpl, noMigrate)
	if withDB == withoutDB {
		t.Error("cmd-tree-root.go.tmpl renders identically with and without the AutoMigrate " +
			"config field; the db command tree is supposed to be gated on it")
	}
	if !strings.Contains(withDB, "newDBCmd(deps)") {
		t.Errorf("root.go omits newDBCmd even though the config declares AutoMigrate:\n%s", withDB)
	}
	if strings.Contains(withoutDB, "newDBCmd(deps)") {
		t.Errorf("root.go wires newDBCmd for a project whose config has no AutoMigrate field; "+
			"db.go is not scaffolded for such a project, so this would not compile:\n%s", withoutDB)
	}
}

// TestCmdTreeDBSourceIsGenuinelyInputDerived is the evidence for the one file
// this split ADDED to Tier-1.
//
// The input it tracks is the presence of SQL in db/migrations/, and the
// response is not cosmetic: with SQL the file imports the project's embedded db
// package, and without it that package does not exist, so importing it would
// not compile. That is the strongest possible form of input-derivation — the
// two renders are not merely different, only one of them builds.
func TestCmdTreeDBSourceIsGenuinelyInputDerived(t *testing.T) {
	const tmpl = "cmd-tree-db-source.go.tmpl"

	withSQL := cmdTreeData{Module: "example.com/demo", Name: "demo", HasMigrations: true}
	withoutSQL := withSQL
	withoutSQL.HasMigrations = false

	a, b := renderCmdTree(t, tmpl, withSQL), renderCmdTree(t, tmpl, withoutSQL)
	if a == b {
		t.Fatal("cmd-tree-db-source.go.tmpl renders identically whether or not the project has " +
			"migrations; its entire reason to be Tier-1 is that it tracks that fact")
	}

	// Asserted on the parsed IMPORT SET, not on the file text. The property
	// at stake is whether the compiler will try to resolve a package that
	// does not exist, and the file's own doc comment legitimately discusses
	// forgedb in both branches — a text search finds that comment and reports
	// a failure that is not there (or, worse, passes on a comment while the
	// real import is missing).
	if imports := dbFileImports(t, a); !containsString(imports, "example.com/demo/db") {
		t.Errorf("with migrations, db_source.go must import the embedded db package; imports = %v", imports)
	}
	for _, imp := range dbFileImports(t, b) {
		if strings.HasSuffix(imp, "/db") {
			t.Errorf("without migrations, db/embed.go does not exist, so db_source.go must not "+
				"import %q — the project would not compile", imp)
		}
	}
}

// TestScaffoldOnceCmdTreeFilesDoNotVaryWithInput is the other direction, and
// it is what makes handing these files to the user safe.
//
// A scaffold-once file is written ONCE, from whatever the inputs looked like at
// that moment, and never re-rendered. So input-sensitivity in one of them is
// worse than in a Tier-1 file: it freezes a single branch permanently and no
// later `forge generate` can correct it. (serve.go is excluded — it is
// deliberately ConfigFields-gated and is birthed by the codegen lane, which
// has the project's real field set; it predates this split and is not in
// scope here.)
//
// The comparison is over CODE, with comments stripped, and the distinction is
// the finding rather than a convenience. These files DO substitute the project
// name and module path into their prose — `demo server` in a doc comment, the
// `-X <module>/cmd/<bin>/cmd.version=` ldflags path in version.go. That is
// plain substitution: it fills a value into a sentence, it does not add or
// remove a statement, so there is no branch to freeze. What would be
// unacceptable is a difference the COMPILER sees, and that is exactly what
// this compares.
//
// The cost of the prose substitution is honest and small: rename the binary
// after scaffolding and those comments name the old one. That is a stale
// sentence in a file the user owns, not a stale decision — and it is the same
// trade every scaffold-once file in the tree already makes.
func TestScaffoldOnceCmdTreeFilesDoNotVaryWithInput(t *testing.T) {
	scaffoldOnce := []string{
		"cmd-tree-server.go.tmpl",
		"cmd-tree-version.go.tmpl",
		"cmd-tree-db.go.tmpl",
	}

	// Two payloads differing in every input EXCEPT the module path: a
	// different project name, a config with no fields at all, and no
	// migrations. If a scaffold-once file responds to any of these, it has a
	// branch that will freeze.
	a := cmdTreeData{
		Module:        "example.com/demo",
		Name:          "demo",
		ConfigFields:  map[string]bool{"AutoMigrate": true, "DatabaseUrl": true, "Port": true},
		HasMigrations: true,
	}
	b := cmdTreeData{
		Module:        "example.com/demo",
		Name:          "payments",
		ConfigFields:  map[string]bool{},
		HasMigrations: false,
	}

	for _, tmpl := range scaffoldOnce {
		want := goCodeOnly(renderCmdTree(t, tmpl, a))
		got := goCodeOnly(renderCmdTree(t, tmpl, b))
		if got == want {
			continue
		}
		// Report the first differing line rather than two whole files, so
		// the failure names the offending construct.
		t.Errorf("%s emits different CODE for different user inputs. It is scaffold-once — "+
			"written from the inputs at ONE moment and never re-rendered — so a conditional in "+
			"it freezes one branch forever with no later run able to correct it. Move the "+
			"input-dependent code to a Tier-1 file (as db.go does with db_source.go).\n"+
			"first difference:\n%s", tmpl, firstDiff(want, got))
	}
}

// goCodeOnly strips comment-only lines and blank lines, leaving what the
// compiler acts on.
//
// Deliberately line-based rather than AST-based: these templates render for
// several different payloads and a payload that produced unparseable Go should
// surface as a parse failure in the tests that check for that, not be silently
// reduced to an empty string here.
func goCodeOnly(src string) string {
	var code []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// firstDiff returns a short report of the first line where a and b differ.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "  line " + itoa(i+1) + ":\n    A: " + x + "\n    B: " + y
		}
	}
	return "  (no line differs — inputs differ only in trailing bytes)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
