package cli

import (
	"bufio"
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
)

// lint_wire_coverage.go — `forge lint --wire-coverage`.
//
// wire_gen emits `<field>: <zero>, // TODO: wire <field>` for any
// service / worker / operator Deps field it could not resolve to a
// conventional source or an *App field. These placeholders compile
// cleanly (the zero value satisfies the type) and validateDeps would
// catch any required field at startup — but until startup runs, the
// gap is silent. Without this lint, a developer can ship a project
// whose wire_gen.go has unresolved TODOs they never noticed.
//
// The lint reads pkg/app/wire_gen.go (or a project-relative override),
// scans for the canonical `// TODO: wire <field>` marker that
// wireExpressionFor emits, and reports each one as a warning. A count
// summary lands at the end. The lint never gates the build — projects
// in active development legitimately have unresolved TODOs while a
// new field is being threaded through *App + setup.go, and turning
// them into errors would block iteration on the very feature the
// scaffold is helping the user land.
//
// Why this lives in cli/ rather than internal/linter/forgeconv/:
//
//   The forgeconv linters are proto-aware analyzers operating against
//   user-authored proto and Go source. wire_gen.go is forge-emitted
//   Go that the user never edits — the lint here is more of a
//   "post-codegen completeness check" than a convention-enforcement
//   rule. Keeping it near runLint() makes the implementation cheap (no
//   new package, no test fixtures shared with forgeconv) without
//   hiding the rule from `forge lint` users.

// reWireTODO matches the canonical TODO marker emitted by
// wireExpressionFor in internal/codegen/wire_gen.go. Group 1 is the
// field name. The marker shape is intentionally narrow — anything
// else commented as `TODO` is left for golangci-lint's `godox` linter.
var reWireTODO = regexp.MustCompile(`//\s*TODO:\s*wire\s+(\S+)`)

// wireCoverageFinding mirrors forgeconv.Finding for the report shape
// without forcing a cross-package dependency. Wire coverage is its
// own thing — the user reads "1 unresolved field on service X" and
// goes to fix it; they don't need the full forgeconv-style remediation
// sentence.
type wireCoverageFinding struct {
	File  string
	Line  int
	Field string
	// Function is the wire_gen function the TODO appears in (e.g.
	// "wireBillingDeps", "wireOperatorWorkspaceControllerDeps").
	// Used for the "<N> unresolved deps across <M> components: ..."
	// summary so users can at-a-glance see which components are
	// affected without paging through the file.
	Function string
}

// runWireCoverageLint reads the project's pkg/app/wire_gen.go, scans
// for `// TODO: wire <field>` markers, and prints one warning per
// match plus a count summary. Also reads pkg/app/app_extras.go for
// `forge:placeholder` annotations and reports as ERRORS any field that
// is still typed `any` after the sibling lane was supposed to land its
// real type — those cases are the silent-worker-noop bug class the
// placeholder annotation exists to prevent.
//
// TODO findings are warnings (projects in active development
// legitimately have unresolved wires while threading a new field).
// Placeholder findings are errors — the user explicitly opted in to
// "this should be tightened" by writing the marker. The error is
// returned so `forge lint` (and the embedded `forge generate` check)
// fails the build.
//
// A missing wire_gen.go is a no-op success — projects that haven't
// run `forge generate` yet, or library projects with no services /
// workers / operators, just have nothing to lint.
func runWireCoverageLint(projectDir string) error {
	fmt.Println("Running wire-coverage lint...")
	path := filepath.Join(projectDir, "pkg", "app", "wire_gen.go")
	wireGenExists := true
	if _, err := os.Stat(path); os.IsNotExist(err) {
		wireGenExists = false
	}

	var findings []wireCoverageFinding
	if wireGenExists {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		got, err := scanWireGen(f, path, projectDir)
		f.Close()
		if err != nil {
			return fmt.Errorf("scan %s: %w", path, err)
		}
		findings = got
	}

	// Placeholder errors are a strict superset signal — they fire even
	// when wire_gen.go is missing (forge generate refused to write it).
	// Read app_extras directly to surface the same diagnosis the
	// generation step would have surfaced.
	placeholders, err := scanUnresolvedPlaceholders(projectDir)
	if err != nil {
		return fmt.Errorf("scan placeholders: %w", err)
	}

	formatWireCoverage(os.Stdout, findings)
	if !wireGenExists && len(placeholders) == 0 {
		fmt.Println("  no pkg/app/wire_gen.go — skipping (run `forge generate` first if you have services / workers / operators)")
		return nil
	}
	if len(placeholders) > 0 {
		formatPlaceholderErrors(os.Stdout, placeholders)
		return fmt.Errorf("%d unresolved forge:placeholder annotation(s) in pkg/app/app_extras.go — tighten field types", len(placeholders))
	}
	return nil
}

// scanUnresolvedPlaceholders reads pkg/app/app_extras.go and returns
// every AppExtras field that carries a `forge:placeholder: <Type>`
// annotation but is still typed `any`. Empty list means either no
// placeholders are declared, or every placeholder has been tightened
// to its target type.
//
// Sharing codegen.ParseAppFields keeps the recognition logic in one
// place — same reader the wire_gen generator uses, same exact-type
// comparison semantics.
func scanUnresolvedPlaceholders(projectDir string) ([]codegen.UnresolvedPlaceholder, error) {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if _, err := os.Stat(appDir); os.IsNotExist(err) {
		return nil, nil
	}
	fields, err := codegen.ParseAppFields(appDir)
	if err != nil {
		return nil, err
	}
	var out []codegen.UnresolvedPlaceholder
	for _, f := range fields {
		if f.Placeholder == "" {
			continue
		}
		t := strings.TrimSpace(f.Type)
		if t == "any" || t == "interface{}" {
			out = append(out, codegen.UnresolvedPlaceholder{
				FieldName:   f.Name,
				CurrentType: f.Type,
				TargetType:  f.Placeholder,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].FieldName < out[j].FieldName
	})
	return out, nil
}

// formatPlaceholderErrors writes one error per unresolved placeholder
// in the canonical `forge lint` shape. Severity is error — the build
// is gated by the caller returning a non-nil error.
func formatPlaceholderErrors(w io.Writer, placeholders []codegen.UnresolvedPlaceholder) {
	if len(placeholders) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, p := range placeholders {
		fmt.Fprintf(w, "  ✗ [forge-wire-coverage] pkg/app/app_extras.go\n")
		fmt.Fprintf(w, "      %s carries `forge:placeholder: %s` but is still typed `%s`\n", p.FieldName, p.TargetType, p.CurrentType)
		fmt.Fprintf(w, "      → tighten the declaration in app_extras.go from `%s %s` to `%s %s`, then re-run `forge generate`\n", p.FieldName, p.CurrentType, p.FieldName, p.TargetType)
	}
	fmt.Fprintf(w, "\n%d unresolved forge:placeholder annotation(s) in pkg/app/app_extras.go.\n", len(placeholders))
	fmt.Fprintln(w, "(errors — failing the build; the placeholder marker promised the field would be tightened)")
}

// scanWireGen extracts all wire-TODO findings from one wire_gen.go.
// Exposed so the unit test can feed in a strings.Reader directly
// without a temp file.
//
// The scan is line-based against reWireTODO, and we run a quick AST
// parse to attribute each line to its enclosing wire*Deps function.
// The function name lets the summary group findings by component
// (one component may have multiple unresolved fields) and surface
// "X unresolved deps across Y components" cleanly.
func scanWireGen(r io.Reader, path, projectDir string) ([]wireCoverageFinding, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	// Build a [funcName, startLine, endLine] table from the AST so
	// each TODO can be attributed to its enclosing function. We don't
	// require the parse to succeed for line-scan to work — a broken
	// wire_gen.go is rare, and a TODO finding with empty Function is
	// still actionable.
	var funcs []wireFuncSpan
	fset := token.NewFileSet()
	if file, parseErr := parser.ParseFile(fset, path, src, 0); parseErr == nil {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if !strings.HasPrefix(fn.Name.Name, "wire") {
				continue
			}
			start := fset.Position(fn.Pos()).Line
			end := fset.Position(fn.End()).Line
			funcs = append(funcs, wireFuncSpan{Name: fn.Name.Name, Start: start, End: end})
		}
	}

	relPath, relErr := filepath.Rel(projectDir, path)
	if relErr != nil {
		relPath = path
	}

	var findings []wireCoverageFinding
	scanner := bufio.NewScanner(strings.NewReader(string(src)))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		m := reWireTODO.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		findings = append(findings, wireCoverageFinding{
			File:     relPath,
			Line:     lineNum,
			Field:    m[1],
			Function: enclosingFunc(funcs, lineNum),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return findings, nil
}

// wireFuncSpan associates a wire*Deps function name with the line
// range it spans in wire_gen.go.
type wireFuncSpan struct {
	Name  string
	Start int
	End   int
}

// enclosingFunc returns the wire*Deps function whose [Start, End]
// span contains line. Empty string when no match — happens when the
// AST parse failed or when the TODO is somehow above the first
// function (header comment), which is fine to leave unattributed.
func enclosingFunc(funcs []wireFuncSpan, line int) string {
	for _, f := range funcs {
		if line >= f.Start && line <= f.End {
			return f.Name
		}
	}
	return ""
}

// formatWireCoverage prints findings to w in the canonical
// `forge lint` shape: one `⚠ [rule] file:line` line per finding plus
// a summary tail. Empty findings print a single success line.
func formatWireCoverage(w io.Writer, findings []wireCoverageFinding) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "  wire coverage clean — no unresolved Deps fields")
		return
	}
	// Stable order: by file, then line.
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	for _, f := range findings {
		fmt.Fprintf(w, "  ⚠ [forge-wire-coverage] %s:%d\n", f.File, f.Line)
		if f.Function != "" {
			fmt.Fprintf(w, "      %s in %s is unresolved — wire_gen emitted a typed-zero placeholder\n", f.Field, f.Function)
		} else {
			fmt.Fprintf(w, "      %s is unresolved — wire_gen emitted a typed-zero placeholder\n", f.Field)
		}
		fmt.Fprintf(w, "      → add `%s <Type>` to AppExtras in pkg/app/app_extras.go and assign in setup.go, OR mark the field `// forge:optional-dep` if it's intentionally optional\n", f.Field)
	}

	// Summary: "<N> unresolved deps across <M> components: <c1>, <c2>, ..."
	componentSet := map[string]bool{}
	for _, f := range findings {
		if f.Function != "" {
			componentSet[f.Function] = true
		}
	}
	components := make([]string, 0, len(componentSet))
	for c := range componentSet {
		components = append(components, c)
	}
	sort.Strings(components)

	if len(components) == 0 {
		fmt.Fprintf(w, "\n%d unresolved Deps field(s) in wire_gen.go.\n", len(findings))
	} else {
		fmt.Fprintf(w, "\n%d unresolved Deps field(s) across %d component(s): %s\n",
			len(findings), len(components), strings.Join(components, ", "))
	}
	fmt.Fprintln(w, "(warnings only — not failing the build)")
}
