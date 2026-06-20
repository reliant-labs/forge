package codegen

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

// interface_assertions_gen.go — emits pkg/app/interface_assertions_gen.go,
// a file of compile-time `var _ <Interface> = (<Concrete>)(nil)`
// assertions for every proven concrete→narrow-interface satisfaction in
// the wire graph.
//
// Why (FORGE_SHAPE_REDESIGN §6c): a fat shared repository commonly
// satisfies 10–12 narrow per-service interfaces purely by structural
// typing, with zero `var _ Iface = (*T)(nil)` assertions anywhere. The
// consequence is you cannot grep "what implements user.Repository" — the
// only proof that *db.PostgresRepository satisfies it is buried in a
// wire_gen assignment (or, worse, only discovered when it stops
// compiling). These assertions surface the relationship explicitly and
// cheaply: one greppable line per (interface, concrete) pair, in one
// file, regenerated every run.
//
// The source of truth is the SAME DepsAssignabilityMatcher wire_gen uses
// to resolve `app.<Field>` into narrow Deps interfaces, so an assertion
// exists exactly when wire_gen actually wires that concrete into that
// interface. No new discovery, no drift: if wire_gen proves the
// assignment, this file records it.
//
// pkg/app is the home because it already imports every component package
// and the concrete-type packages (db, clients, …) the App/AppExtras
// fields reference — the assertion compiles there with no new dependency
// edge.

// InterfaceAssertionComponent names one component to collect assertions
// for: its role root ("handlers"/"workers"/"operators") and the on-disk
// directory leaf under that root (the matcher loads packages by path).
type InterfaceAssertionComponent struct {
	RoleRoot string
	PkgDir   string
}

// GenerateInterfaceAssertions emits pkg/app/interface_assertions_gen.go.
// It consults the supplied matcher (shared with wire_gen so each package
// is loaded at most once per generate) for every component's proven
// concrete→interface pairs, dedupes them, and renders one assertion per
// pair.
//
// Best-effort by contract: a component whose universe didn't type-check
// yields no pairs (and no error). When the project as a whole produces no
// assertions, NO file is written and any stale prior file is removed via
// the checksum ledger's normal sweep — an empty assertions file is noise.
//
// matcher may be nil (assertions disabled — no file written). cs may be
// nil (tolerated; the file is then written untracked, matching the other
// emitters' nil-cs tolerance).
func GenerateInterfaceAssertions(components []InterfaceAssertionComponent, modulePath, projectDir string, matcher *DepsAssignabilityMatcher, cs *checksums.FileChecksums) error {
	if matcher == nil {
		return nil
	}

	// Collect across all components, dedupe by the full assertion line so
	// the same fat-repo→interface pair shared by N services emits once.
	type collected struct {
		InterfaceAssertion
		fromComponents []string
	}
	byLine := map[string]*collected{}
	mergedImports := map[string]string{}

	for _, c := range components {
		for _, a := range matcher.AssignablePairs(c.RoleRoot, c.PkgDir) {
			lineKey := a.Interface + " = " + a.Concrete
			comp := c.RoleRoot + "/" + c.PkgDir
			if existing, ok := byLine[lineKey]; ok {
				existing.fromComponents = append(existing.fromComponents, comp)
				continue
			}
			byLine[lineKey] = &collected{
				InterfaceAssertion: a,
				fromComponents:     []string{comp},
			}
			for path, name := range a.Imports {
				mergedImports[path] = name
			}
		}
	}

	if len(byLine) == 0 {
		// No proven pairs — write an empty (no-assertion) file so the
		// path stays certified rather than flapping in/out of the ledger,
		// but keep it minimal. (A project with services but no narrow
		// interface deps is legitimate.)
		return writeEmptyAssertionsFile(projectDir, cs)
	}

	// Detect package-name collisions: two distinct import paths whose
	// base name is identical would make the unqualified `pkg.Name`
	// selectors ambiguous. types.TypeString rendered both with the same
	// name, so we cannot safely alias after the fact — drop any assertion
	// that references a colliding name (best-effort: most projects have
	// no collision, and a dropped assertion only loses a greppability
	// aid, never correctness).
	nameToPaths := map[string]map[string]bool{}
	for path, name := range mergedImports {
		if nameToPaths[name] == nil {
			nameToPaths[name] = map[string]bool{}
		}
		nameToPaths[name][path] = true
	}
	collidingName := map[string]bool{}
	for name, paths := range nameToPaths {
		if len(paths) > 1 {
			collidingName[name] = true
		}
	}

	// Build the ordered assertion + import sets, skipping collisions.
	var assertions []assertionLine
	usedImports := map[string]string{}
	for _, c := range byLine {
		if assertionReferencesCollidingName(c.InterfaceAssertion, collidingName) {
			continue
		}
		for path, name := range c.Imports {
			usedImports[path] = name
		}
		sort.Strings(c.fromComponents)
		assertions = append(assertions, assertionLine{
			Interface: c.Interface,
			Concrete:  c.Concrete,
			Comment:   fmt.Sprintf("%s — wired into %s", c.DepsField, strings.Join(uniqueStrings(c.fromComponents), ", ")),
		})
	}
	if len(assertions) == 0 {
		return writeEmptyAssertionsFile(projectDir, cs)
	}
	sort.Slice(assertions, func(i, j int) bool {
		if assertions[i].Interface != assertions[j].Interface {
			return assertions[i].Interface < assertions[j].Interface
		}
		return assertions[i].Concrete < assertions[j].Concrete
	})

	imports := make([]string, 0, len(usedImports))
	for path := range usedImports {
		imports = append(imports, path)
	}
	sort.Strings(imports)

	content := renderInterfaceAssertions(imports, assertions)
	if err := writeForgeOwned(projectDir, filepath.Join("pkg", "app", "interface_assertions_gen.go"), []byte(content), cs); err != nil {
		return fmt.Errorf("write pkg/app/interface_assertions_gen.go: %w", err)
	}
	return nil
}

// assertionLine is one rendered `var _ I = (*T)(nil)` row plus its
// explanatory comment.
type assertionLine struct {
	Interface string
	Concrete  string
	Comment   string
}

func assertionReferencesCollidingName(a InterfaceAssertion, colliding map[string]bool) bool {
	for _, name := range a.Imports {
		if colliding[name] {
			return true
		}
	}
	return false
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// renderInterfaceAssertions builds the file body. Done with a string
// builder rather than a template so the emitter stays dependency-light
// and the header can carry the forge:hash marker writeForgeOwned stamps.
func renderInterfaceAssertions(imports []string, assertions []assertionLine) string {
	var sb strings.Builder
	sb.WriteString("// Code generated by forge. DO NOT EDIT.\n")
	sb.WriteString("//\n")
	sb.WriteString("// Interface-satisfaction assertions: each line proves a concrete type\n")
	sb.WriteString("// satisfies a narrow consumer interface it is wired into. Grep an\n")
	sb.WriteString("// interface name here to find every concrete that implements it (the\n")
	sb.WriteString("// fat-repo greppability gap — see the forge architecture skill).\n")
	sb.WriteString("package app\n\n")
	if len(imports) > 0 {
		sb.WriteString("import (\n")
		for _, p := range imports {
			fmt.Fprintf(&sb, "\t%q\n", p)
		}
		sb.WriteString(")\n\n")
	}
	for _, a := range assertions {
		if a.Comment != "" {
			fmt.Fprintf(&sb, "// %s\n", a.Comment)
		}
		fmt.Fprintf(&sb, "var _ %s = (%s)(nil)\n", a.Interface, a.Concrete)
	}
	return sb.String()
}

// writeEmptyAssertionsFile emits a minimal, valid file so the generated
// path is consistent across regenerates even when there is nothing to
// assert (a project with no narrow interface deps).
func writeEmptyAssertionsFile(projectDir string, cs *checksums.FileChecksums) error {
	content := "// Code generated by forge. DO NOT EDIT.\n" +
		"//\n" +
		"// Interface-satisfaction assertions. No concrete→narrow-interface\n" +
		"// pairs were proven in the wire graph (no services declare narrow\n" +
		"// collaborator interfaces, or the project did not type-check at\n" +
		"// generate time). The file regenerates with assertions once such\n" +
		"// pairs exist — see the forge architecture skill (§6c).\n" +
		"package app\n"
	if err := writeForgeOwned(projectDir, filepath.Join("pkg", "app", "interface_assertions_gen.go"), []byte(content), cs); err != nil {
		return fmt.Errorf("write pkg/app/interface_assertions_gen.go: %w", err)
	}
	return nil
}
