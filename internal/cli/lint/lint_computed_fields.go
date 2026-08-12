// File: internal/cli/lint/lint_computed_fields.go
//
// computed-fields — `forge lint --computed-fields`.
//
// `// forge:read-only` says a field is not CLIENT-writable. It says nothing
// about who writes it instead — and forge, correctly, writes nothing. So a
// read-only column that no app code populates takes its column DEFAULT, and
// for the money columns this happens to most (`amount_cents`,
// `subtotal_cents`, `total_cents`) that default is 0.
//
// Nothing catches it. No constraint is violated: 0 is a perfectly legal
// BIGINT. No test fails: the generated CRUD lifecycle test round-trips
// whatever the create wrote, so it asserts the zero it just stored. No error
// is logged. The single symptom is a human eventually reading a screen that
// says $0.00. The motivating case was an estimate line item whose proto
// comment promised `quantity_milli * unit_price_cents / 1000` and whose rows
// were all zero — found ninety minutes later, by looking.
//
// ── Why a marker, and not a heuristic over doc comments ───────────────────
//
// The tempting alternative is to warn when a read-only field's comment
// CLAIMS derivation — "computed", "derived", "sum of", "total of" — and
// nothing writes it. Measured against a real forge project (nine services,
// nine read-only fields) that rule is unusable:
//
//   - All nine carry derivation-claiming prose: "Totals in cents, maintained
//     by RecalculateEstimate", "Set by CompleteJob", "Derived from the
//     payment ledger by RecordPayment". So the prose half selects
//     essentially every read-only field, and the rule's precision rests
//     ENTIRELY on the "nothing writes it" half being exactly right.
//   - That half is a whole-program dataflow question. A value can be
//     populated by a handler hook, a service-layer method, a background
//     worker, a database trigger, or a GENERATED column — several of which
//     are not Go source at all. A checker that guesses it wrong reports a
//     correct field as broken.
//
// Guessing both halves at once is precisely how a rule cries wolf, and a
// lint rule that fires on correct code gets switched off, after which it
// protects nothing. So the mechanism is the EXPLICIT marker: the author
// declares the obligation with `// forge:computed`, and this check holds
// them to it. The question becomes definite — "you said this is computed;
// what computes it?" — and so does the fix.
//
// The marker is not inert: it implies read-only in both recognizers
// (codegen.ReadOnlyProtoMarkers), so adopting it changes the generated write
// envelopes exactly as read-only does. An author migrating a field from
// read-only to computed changes no behaviour and gains the check.
//
// ── What it checks ────────────────────────────────────────────────────────
//
// For each `// forge:computed` field, whether ANY non-generated, non-test Go
// file in the project assigns its Go field name (`amount_cents` →
// `AmountCents`) — `x.AmountCents = ...`, `x.AmountCents += ...`, or a
// `AmountCents:` key in a composite literal.
//
// Deliberately generous about WHERE the write lives: handler hook, service
// method, worker, anywhere. A write that exists is proof the obligation was
// met, and narrowing the search to one directory would report fields that
// are computed somewhere else — the exact false positive that discredits the
// rule.
//
// Generated files (`*_gen.go`) are excluded from the search, and that is the
// point rather than an oversight: the generated conversion assigns
// `e.AmountCents = m.AmountCents` for every field, so counting generated
// writes would mark every computed field as satisfied and the check would
// never fire at all. Tests are excluded for the same reason — a factory
// setting the field proves nothing about production.
//
// Severity is warning. The finding is high-confidence, but the fix is app
// logic only the author can write, and a project mid-migration (marker added
// before the hook) would otherwise be unable to run lint at all.

package lint

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

// computedFieldFinding is one `forge:computed` field that no app code
// writes. File/Line point at the PROTO declaration — the place the
// obligation was declared, and where the author decides whether to write
// the hook or drop the marker.
type computedFieldFinding struct {
	File   string
	Line   int
	Entity string
	Field  string
	// GoField is the protoc-gen-go spelling searched for ("AmountCents"),
	// carried so the fix hint can name the identifier the author must
	// actually assign rather than the proto spelling, which appears
	// nowhere in Go.
	GoField string
}

// computedFieldFixHint renders the remediation. It states both legitimate
// resolutions, because "nothing writes it" has two honest answers: write
// the thing, or stop claiming it is written.
func computedFieldFixHint(f computedFieldFinding) string {
	return fmt.Sprintf(
		"%s.%s is marked `%s` but no non-generated Go file assigns %s. The field is omitted "+
			"from Create/Update (as read-only), so nothing populates it and the insert takes the "+
			"column default — for a money column that ships as $0.00 with no error anywhere. "+
			"Either derive it (override the generated op's Entity hook in "+
			"internal/handlers/<svc>/handlers_crud.go and set row.%s before returning), or drop "+
			"the marker to `%s` if the value is genuinely written elsewhere (a trigger, a "+
			"GENERATED column, a service this check cannot see).",
		f.Entity, f.Field, codegen.ProtoMarkerComputed, f.GoField, f.GoField, codegen.ProtoMarkerReadOnly)
}

// runComputedFieldsLint is the text-mode entry point.
func runComputedFieldsLint(projectDir string) error {
	fmt.Println("Running computed-fields lint...")
	findings, err := collectComputedFieldFindings(projectDir)
	if err != nil {
		return err
	}
	formatComputedFields(os.Stdout, findings)
	return nil
}

// formatComputedFields writes the human report.
func formatComputedFields(w io.Writer, findings []computedFieldFinding) {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  computed-fields clean — every forge:computed field is written by app code")
		return
	}
	for _, f := range findings {
		_, _ = fmt.Fprintf(w, "  ⚠ [forgeconv-computed-field-unwritten] %s:%d\n", f.File, f.Line)
		_, _ = fmt.Fprintf(w, "      → %s\n", computedFieldFixHint(f))
	}
	_, _ = fmt.Fprintf(w, "\n%d computed field(s) that nothing populates.\n", len(findings))
	_, _ = fmt.Fprintln(w, "(warnings only — not failing the build)")
}

// collectComputedFieldFindings is the shared engine behind text mode and
// `forge lint --json`. A project with no proto tree yields nothing.
func collectComputedFieldFindings(projectDir string) ([]computedFieldFinding, error) {
	protoRoot := filepath.Join(projectDir, protoDirDefault)
	if _, err := os.Stat(protoRoot); os.IsNotExist(err) {
		return nil, nil
	}
	dirs, err := protoSubdirsWithFiles(protoRoot)
	if err != nil {
		return nil, err
	}

	// Collect the declared obligations first, so the (more expensive) Go
	// scan is skipped entirely when there are none — the common case for
	// a project that has not adopted the marker.
	type computedField struct {
		entity, field, goField, file string
		line                         int
	}
	var declared []computedField
	for _, dir := range dirs {
		scan, scanErr := codegen.ScanRawProtoDir(dir)
		if scanErr != nil {
			continue // buf lint / generate report a malformed proto far better
		}
		for _, msg := range scan.Messages {
			for _, name := range computedFieldNames(msg) {
				declared = append(declared, computedField{
					entity:  msg.Name,
					field:   name,
					goField: naming.ToProtoPascalCase(name),
					file:    msg.File,
					line:    fieldLineIn(msg, name),
				})
			}
		}
	}
	if len(declared) == 0 {
		return nil, nil
	}

	written, err := assignedGoFields(projectDir)
	if err != nil {
		return nil, err
	}

	var findings []computedFieldFinding
	for _, d := range declared {
		if written[d.goField] {
			continue
		}
		findings = append(findings, computedFieldFinding{
			// Project-relative, matching every sibling lint: an absolute
			// path under a temp dir or a CI checkout is not clickable in
			// the reader's editor and differs between machines.
			File: relToProject(projectDir, d.file), Line: d.line,
			Entity: d.entity, Field: d.field, GoField: d.goField,
		})
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// relToProject renders an absolute scan path as project-relative, falling
// back to the original when it lies outside the project (which nothing in
// the normal walk produces, but a symlinked proto tree could).
func relToProject(projectDir, path string) string {
	rel, err := filepath.Rel(projectDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// computedFieldNames returns the fields of msg carrying `forge:computed`.
//
// The raw scan records `forge:computed` as ReadOnly (the marker implies it)
// and keeps no separate bit, so membership is re-read from the source text
// here. Re-reading is the right call rather than a workaround: adding a
// Computed bit to SchemaFieldDef would put a lint-only concern into the
// vocabulary every generator consumes, and the two markers are deliberately
// identical everywhere EXCEPT this check.
func computedFieldNames(msg codegen.RawProtoMessage) []string {
	data, err := os.ReadFile(msg.File)
	if err != nil {
		return nil
	}
	content := string(data)
	if msg.BodyOpen < 0 || msg.BodyClose > len(content) || msg.BodyOpen >= msg.BodyClose {
		return nil
	}
	declared := make(map[string]bool, len(msg.Fields))
	for _, f := range msg.Fields {
		declared[f.Name] = true
	}

	computedRE := codegen.ProtoMarkerAnyLineRE([]string{codegen.ProtoMarkerComputed})
	var out []string
	// Both accepted marker positions, matching the scanner's own rule: a
	// trailing comment binds to the field on that line, a full-line one
	// binds to the next field declared.
	pending := false
	for _, line := range strings.Split(content[msg.BodyOpen:msg.BodyClose], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue // blank line — a pending marker survives
		}
		if strings.HasPrefix(trimmed, "//") {
			if computedRE.MatchString(trimmed) {
				pending = true
			}
			continue
		}
		name, ok := protoFieldNameOnLine(line)
		if !ok || !declared[name] {
			continue
		}
		if pending || computedRE.MatchString(line) {
			out = append(out, name)
		}
		pending = false
	}
	return out
}

// protoFieldNameOnLine extracts the field name from a proto field
// declaration line, or ok=false when the line declares no field.
func protoFieldNameOnLine(line string) (string, bool) {
	m := protoFieldDeclRE.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// protoFieldDeclRE captures the NAME of a proto field declaration.
// Anchored to the line's start (modulo indentation) so a type or default
// value that happens to contain a `=` cannot be read as a declaration.
var protoFieldDeclRE = regexp.MustCompile(`^\s*(?:optional\s+|repeated\s+)?[\w.]+\s+(\w+)\s*=\s*\d+`)

// assignedGoFields returns the set of struct-field identifiers assigned
// anywhere in the project's non-generated, non-test Go source.
//
// Selector-based rather than type-resolving on purpose. Resolving types
// would mean type-checking the whole project, which needs it to COMPILE —
// and a lint that only works on a building project cannot report on the
// half-finished state where this defect actually lives. The trade is that
// two different types with the same field name are conflated; that
// direction is safe here, because it can only SUPPRESS a finding, never
// invent one. A rule whose failure mode is silence is the right shape for
// something this close to the author's own code.
func assignedGoFields(projectDir string) (map[string]bool, error) {
	written := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(projectDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipGoScanDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") ||
			strings.HasSuffix(name, "_gen.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return nil // the Go toolchain reports parse errors far better
		}
		collectAssignedFields(file, written)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", projectDir, err)
	}
	return written, nil
}

// skipGoScanDir names the trees that cannot contain a hand-written
// derivation: forge's own generated output, vendored dependencies, and
// build artifacts.
func skipGoScanDir(name string) bool {
	switch name {
	case "gen", "vendor", "node_modules", ".git", "dist", "build":
		return true
	}
	return false
}

// collectAssignedFields records every struct-field identifier that appears
// on the left of an assignment, in a compound assignment, or as a key in a
// composite literal.
func collectAssignedFields(file *ast.File, out map[string]bool) {
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			// `row.AmountCents = ...` and `row.AmountCents += ...`
			for _, lhs := range node.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					out[sel.Sel.Name] = true
				}
			}
		case *ast.IncDecStmt:
			if sel, ok := node.X.(*ast.SelectorExpr); ok {
				out[sel.Sel.Name] = true
			}
		case *ast.CompositeLit:
			// `db.LineItem{AmountCents: total}` — a constructor is as
			// legitimate a derivation site as a field assignment.
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok {
					out[key.Name] = true
				}
			}
		}
		return true
	})
}
