// Package schemadrift detects — and only ever PRINTS — the divergence
// between what a forge entity's proto declares today and what its BORN
// migration actually enforces in the database.
//
// # Why this exists
//
// forge projects proto→schema truth into a migration exactly once, at
// entity birth ("the line is the filesystem"): after that moment forge
// never writes or edits a migration, even when the proto changes. So an
// enum whose values were renamed, or a protovalidate bound that was
// tightened, silently drifts away from the CHECK constraints and columns
// the born migration froze. This package finds that drift and emits a
// NOTICE with a copy-pasteable suggested `ALTER TABLE …`. It NEVER writes
// a migration and NEVER fails the build — divergence is reported, the fix
// is the user's to apply in a new migration.
//
// # How the comparison is made robust
//
//   - APPLIED schema: schemadef.ApplyAndIntrospect(db/migrations) — the
//     same real-postgres shadow entity detection already relies on — read
//     back through postgres's own catalog. This is the source of truth for
//     what the database actually enforces.
//   - DESIRED schema: forge's birth-time projection for each entity today
//     (scaffold.RenderEntityMigrationFromProto, called in COMPUTE-ONLY mode
//     — the rendered SQL is introspected and discarded, never written),
//     applied to a second real-postgres shadow and read back through the
//     SAME catalog.
//
// Because both sides pass through postgres's own normalizer, two
// semantically-identical CHECKs read back byte-identical no matter how they
// were spelled in the source SQL (`col IN (...)` vs `col = ANY (ARRAY[...])`).
// Detection is a set diff over those normalized definitions — no bespoke SQL
// expression matcher.
//
// # Scope: forge-owned projections only (the false-positive guard)
//
// Drift is reported ONLY in the direction "the proto/forge projection
// declares X, the database does not enforce X", and ONLY for things forge
// itself projects at birth:
//
//   - an entity proto field with no matching column (an added field);
//   - an enum column whose CHECK vocabulary changed;
//   - a scalar column whose protovalidate CHECK changed.
//
// The opposite direction is deliberately silent: a column, index, CHECK or
// constraint present in the database but NOT in forge's projection is the
// developer's own — hand-added schema forge never projected — and is never
// flagged. Managed columns (id / created_at / updated_at / deleted_at) are
// excluded too. When the desired projection cannot be
// computed cleanly, the package says nothing: a false positive erodes trust
// worse than a missed one.

// forge:exclude-contract
// schemadrift is a pure comparison function plus its result record: the
// entry point is a package-level func, and Report's exported methods
// (Empty, String) are predicates/rendering ON that value. Nothing here
// holds a dependency or reaches outside the arguments it is given, so
// there is no seam to inject or fake.
package schemadrift

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jinzhu/inflection"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/scaffold"
	"github.com/reliant-labs/forge/internal/shadowdb"
	"github.com/reliant-labs/forge/pkg/schemadef"
)

// Drift is one forge-owned projection the applied schema no longer matches.
type Drift struct {
	// Table is the applied table name.
	Table string
	// Column is the drifted column.
	Column string
	// Kind is a short human label ("added proto field", "enum CHECK
	// vocabulary changed since birth", "protovalidate CHECK changed since
	// birth").
	Kind string
	// SuggestedSQL is the copy-pasteable ALTER statement(s) a user could
	// put in a NEW migration to reconcile the database. Never applied by
	// forge.
	SuggestedSQL []string
}

// Report is the full set of drifts found across a project's entities, plus
// the evidence for how much of the project the comparison actually covered.
//
// Compared/Inconclusive exist because "no drift" is a claim, and Detect has
// several paths that return zero drifts having compared NOTHING: no applied
// schema, no born entity matching a proto CRUD quintet, or a desired-side
// projection that would not apply to the shadow. Callers used to render all
// of those as "✅ No schema drift: every entity's applied schema matches" —
// vacuously true over an empty set, and indistinguishable from a real pass.
type Report struct {
	Drifts []Drift

	// Compared is the number of born entities whose applied table was
	// actually diffed against its current proto projection.
	Compared int

	// Inconclusive, when non-empty, says why the comparison could not be
	// completed. A report with Inconclusive set proves nothing: it is not
	// a pass and must never be rendered as one.
	Inconclusive string
}

// Empty reports whether no drift was found. It says nothing about whether
// anything was examined — check Compared and Inconclusive for that.
func (r Report) Empty() bool { return len(r.Drifts) == 0 }

// String renders the printed notice a user sees. Empty reports render "".
func (r Report) String() string {
	if r.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("ℹ️  Schema drift: some entity protos changed after birth, but forge never edits a\n")
	b.WriteString("    migration once it is written (\"the line is the filesystem\"). The applied schema\n")
	b.WriteString("    (db/migrations) no longer matches what these protos now declare. forge does NOT\n")
	b.WriteString("    auto-write a migration — review the suggested ALTERs and add a NEW migration if\n")
	b.WriteString("    you want the database to match:\n\n")
	for _, d := range r.Drifts {
		fmt.Fprintf(&b, "  • %s.%s — %s\n", d.Table, d.Column, d.Kind)
		for _, s := range d.SuggestedSQL {
			fmt.Fprintf(&b, "      %s\n", s)
		}
	}
	b.WriteString("\n    Review the DROP CONSTRAINT lines before applying — they target the CHECK\n")
	b.WriteString("    constraints forge originally generated for those columns; keep any you added by\n")
	b.WriteString("    hand. This notice is informational; generate did not fail and nothing was written.\n")
	return b.String()
}

// managedColumns are the convention columns forge's migration owns; drift
// is never reported for them (they are forge-managed but never proto-driven
// in a way this detector tracks).
var managedColumns = map[string]bool{
	"id":         true,
	"created_at": true,
	"updated_at": true,
	"deleted_at": true,
}

// Detect compares each forge-born entity's current proto projection against
// the applied schema and returns the drift report. It is non-fatal: a failure
// to introspect returns the underlying error, which callers may log but should
// not surface as a build failure.
//
// A zero-drift report is NOT automatically a pass. Report.Compared says how
// many entities were actually diffed, and Report.Inconclusive says why the
// comparison could not be completed (no applied schema, or a projection that
// produced no shadow tables). A project with no db/migrations, and a project
// whose desired projection would not apply, both come back Inconclusive.
func Detect(projectDir string, services []codegen.ServiceDef) (Report, error) {
	migDir := filepath.Join(projectDir, "db", "migrations")
	// Resolve the shadow server once from forge's config and use it for BOTH
	// the applied side and the desired-projection side, so the drift diff is
	// read back through the same postgres.
	shadowServer := shadowdb.Resolve(projectDir)
	applied, err := schemadef.ApplyAndIntrospectAt(migDir, shadowServer)
	if err != nil {
		return Report{}, err
	}
	if len(applied) == 0 {
		return Report{Inconclusive: "no applied schema to compare against (db/migrations is empty or applied to no tables)"}, nil
	}
	appliedByName := indexTables(applied)

	// Build forge's birth-time projection for every born entity, collecting
	// the rendered CREATE TABLE statements into a single desired-shadow DDL.
	type target struct {
		table  string
		fields []codegen.SchemaFieldDef
		upSQL  string
	}
	var (
		combined strings.Builder
		targets  []target
		seen     = map[string]bool{}
	)
	for _, svc := range services {
		for _, m := range svc.Methods {
			if m.ClientStreaming || m.ServerStreaming {
				continue
			}
			op, name := codegen.ParseCRUDOperation(m.Name)
			if op == "" || name == "" {
				continue
			}
			if op == "list" {
				name = inflection.Singular(name)
			}
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			table := naming.Pluralize(naming.ToSnakeCase(name))
			appliedT, ok := appliedByName[table]
			if !ok {
				continue // not born: no applied table to compare against
			}
			fields, ok := svc.Schemas[svc.Package+"."+name]
			if !ok || len(fields) == 0 {
				continue // no wire shape to project the desired schema from
			}
			seen[key] = true

			// Timestamps/SoftDelete are derived from the APPLIED table so the
			// desired render mirrors the same managed columns and they never
			// read as spurious drift (they are excluded from the diff anyway).
			spec := scaffold.EntityFromProtoSpec{
				Table:      table,
				MessageFQ:  svc.Package + "." + name,
				ProtoPkg:   svc.Package,
				Fields:     fields,
				Enums:      svc.Enums,
				SoftDelete: hasColumn(appliedT, schemadef.ColDeletedAt),
				Timestamps: hasColumn(appliedT, schemadef.ColCreatedAt) && hasColumn(appliedT, schemadef.ColUpdatedAt),
			}
			mig := scaffold.RenderEntityMigrationFromProto(spec)
			combined.WriteString(mig.UpSQL)
			combined.WriteString("\n")
			targets = append(targets, target{table: table, fields: fields, upSQL: mig.UpSQL})
		}
	}
	// Zero born entities is a legitimate "nothing to check", not a pass:
	// Compared stays 0 and the caller renders it as such.
	if len(targets) == 0 {
		return Report{}, nil
	}

	desiredByName, err := introspectDesired(combined.String(), shadowServer)
	if err != nil {
		return Report{}, err
	}
	// The projection could not be applied to a shadow (e.g. a construct the
	// renderer emitted that this ephemeral DB can't satisfy). Say so — the
	// previous empty-report-with-nil-error surfaced upstream as "no drift"
	// for entities that were never compared.
	if len(desiredByName) == 0 {
		return Report{Inconclusive: fmt.Sprintf(
			"forge's projection of %d entit%s produced no shadow tables, so nothing could be diffed",
			len(targets), plural(len(targets)))}, nil
	}

	var rep Report
	var skipped []string
	for _, tg := range targets {
		desiredT, ok := desiredByName[tg.table]
		if !ok {
			skipped = append(skipped, tg.table)
			continue
		}
		rep.Compared++
		rep.Drifts = append(rep.Drifts,
			diffEntity(tg.table, tg.fields, tg.upSQL, appliedByName[tg.table], desiredT)...)
	}
	if len(skipped) > 0 {
		rep.Inconclusive = fmt.Sprintf("no projected table for %s — those entities were not compared",
			strings.Join(skipped, ", "))
	}
	return rep, nil
}

// introspectDesired applies forge's projected DDL to a throwaway real-
// postgres shadow (on the SAME resolved server the applied side uses) and
// returns the resulting tables keyed by name. The temp dir is removed
// before returning.
func introspectDesired(ddl, shadowServer string) (map[string]schemadef.Table, error) {
	dir, err := os.MkdirTemp("", "forge-drift-desired-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "00001_desired.up.sql"), []byte(ddl), 0o644); err != nil {
		return nil, err
	}
	tables, err := schemadef.ApplyAndIntrospectAt(dir, shadowServer)
	if err != nil {
		return nil, err
	}
	return indexTables(tables), nil
}

// diffEntity computes the scoped drift for one entity: added forge columns
// missing from the applied table, and CHECK drift on shared forge columns.
// Only the "forge projects X, database lacks X" direction is reported.
func diffEntity(table string, fields []codegen.SchemaFieldDef, upSQL string, applied, desired schemadef.Table) []Drift {
	var drifts []Drift
	appliedCols := columnSet(applied)

	// 1. Added proto field: a desired column with no applied column. The
	//    opposite (a column in the DB but not in the projection) is the
	//    developer's own and is never flagged.
	for _, dc := range desired.Columns {
		if managed(dc.Name) {
			continue
		}
		if _, ok := appliedCols[dc.Name]; ok {
			continue
		}
		d := Drift{Table: table, Column: dc.Name, Kind: "added proto field (no column in db/migrations)"}
		if line := columnLine(upSQL, dc.Name); line != "" {
			d.SuggestedSQL = []string{fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", table, line)}
		}
		drifts = append(drifts, d)
	}

	// 2. CHECK drift on a column present on BOTH sides. A forge check the
	//    database does not enforce is drift; extra hand-added checks in the
	//    database are left alone.
	appliedChecks := singleColumnChecks(applied)
	desiredChecks := singleColumnChecks(desired)
	for _, dc := range desired.Columns {
		col := dc.Name
		if managed(col) {
			continue
		}
		if _, ok := appliedCols[col]; !ok {
			continue // handled by the added-field pass
		}
		want := desiredChecks[col]
		if len(want) == 0 {
			continue // forge projects no check on this column
		}
		have := appliedChecks[col]
		haveNorm := normSet(have)
		// Compare bound CHECKs SEMANTICALLY, not structurally. A birth
		// migration that froze ONE combined predicate
		// `CHECK (col >= 1 AND col <= 12)` enforces exactly the same bounds as
		// the split single-bound CHECKs forge's protovalidate projection emits
		// (`CHECK (col >= 1)` + `CHECK (col <= 12)`). Fold both the desired and
		// the applied CHECK sets into canonical bound atoms so the two
		// spellings are equal and only a genuinely-changed bound reports drift.
		// Enum `IN (...)` / regex CHECKs decompose to nothing and stay matched
		// by their whole normalized definition, exactly as before.
		haveAtoms := canonicalCheckAtomSet(have)
		var missing bool
		for a := range canonicalCheckAtomSet(want) {
			if !haveAtoms[a] {
				missing = true
				break
			}
		}
		if !missing {
			continue // every forge bound/CHECK for this column is enforced
		}

		d := Drift{Table: table, Column: col, Kind: checkKind(fields, col)}
		// DROP the applied checks forge previously owned that no longer match.
		wantNorm := normSet(want)
		for _, c := range have {
			if !wantNorm[normDef(c.Def)] {
				d.SuggestedSQL = append(d.SuggestedSQL,
					fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s;", table, c.Name))
			}
		}
		// ADD the forge checks the database is missing, in forge's own idiom
		// (parsed from the rendered column line), so the suggestion is what
		// forge would have written.
		for _, raw := range missingRawChecks(want, columnCheckClauses(upSQL, col), haveNorm) {
			d.SuggestedSQL = append(d.SuggestedSQL, fmt.Sprintf("ALTER TABLE %s ADD %s;", table, raw))
		}
		drifts = append(drifts, d)
	}
	return drifts
}

// checkKind labels a check drift by the projecting field's shape.
func checkKind(fields []codegen.SchemaFieldDef, col string) string {
	for _, f := range fields {
		if f.Name != col {
			continue
		}
		switch {
		case f.Kind == "enum":
			return "enum CHECK vocabulary changed since birth"
		case f.Validate.HasAny():
			return "protovalidate CHECK changed since birth"
		}
		break
	}
	return "CHECK constraint changed since birth"
}

// missingRawChecks returns the forge-idiomatic CHECK clauses (raw, parsed
// from the rendered column line) whose normalized form the database does
// not already enforce. When the raw clauses cannot be paired 1:1 with the
// introspected desired checks it returns them all — over-suggesting an ADD
// is harmless next to the DROP lines and keeps the SQL valid.
func missingRawChecks(want []schemadef.CheckConstraint, raw []string, haveNorm map[string]bool) []string {
	sorted := append([]schemadef.CheckConstraint(nil), want...)
	sort.Slice(sorted, func(i, j int) bool { return checkOrder(sorted[i].Name) < checkOrder(sorted[j].Name) })
	if len(sorted) != len(raw) {
		return raw
	}
	var out []string
	for i, c := range sorted {
		if !haveNorm[normDef(c.Def)] {
			out = append(out, raw[i])
		}
	}
	return out
}

// checkOrder extracts the trailing integer postgres appends to auto-named
// CHECK constraints (`..._check`, `..._check1`, `..._check2`), giving their
// creation order — which matches the left-to-right order the clauses appear
// in the rendered column definition.
func checkOrder(name string) int {
	i := strings.LastIndex(name, "check")
	if i < 0 {
		return 0
	}
	if n, err := strconv.Atoi(name[i+len("check"):]); err == nil {
		return n
	}
	return 0
}

// managed reports whether a column is forge-managed, and thus excluded from
// drift.
func managed(name string) bool {
	return managedColumns[name]
}

func hasColumn(t schemadef.Table, name string) bool {
	for _, c := range t.Columns {
		if c.Name == name {
			return true
		}
	}
	return false
}

func indexTables(tables []schemadef.Table) map[string]schemadef.Table {
	m := make(map[string]schemadef.Table, len(tables))
	for _, t := range tables {
		m[t.Name] = t
	}
	return m
}

func columnSet(t schemadef.Table) map[string]schemadef.Column {
	m := make(map[string]schemadef.Column, len(t.Columns))
	for _, c := range t.Columns {
		m[c.Name] = c
	}
	return m
}

// singleColumnChecks groups a table's CHECK constraints by their single
// referenced column. Multi-column checks (rare; never forge-projected) are
// skipped — this detector only reasons about per-column forge projections.
func singleColumnChecks(t schemadef.Table) map[string][]schemadef.CheckConstraint {
	m := map[string][]schemadef.CheckConstraint{}
	for _, c := range t.Checks {
		if len(c.Columns) != 1 {
			continue
		}
		m[c.Columns[0]] = append(m[c.Columns[0]], c)
	}
	return m
}

// normDef collapses interior whitespace so two catalog-normalized check
// definitions compare equal. Both sides already come from postgres, so this
// is belt-and-suspenders.
func normDef(def string) string { return strings.Join(strings.Fields(def), " ") }

func normSet(checks []schemadef.CheckConstraint) map[string]bool {
	m := make(map[string]bool, len(checks))
	for _, c := range checks {
		m[normDef(c.Def)] = true
	}
	return m
}

// ── semantic CHECK equivalence: combined predicate ≡ split single-bound set ──
//
// forge's protovalidate projection emits a SEPARATE single-bound CHECK per
// bound (`CHECK (col >= 1)` and `CHECK (col <= 12)`), but a hand-written birth
// migration may freeze the SAME bounds as one combined predicate
// (`CHECK (col >= 1 AND col <= 12)`). Both pass through postgres, which
// normalizes them to `((col >= 1) AND (col <= 12))` vs `(col >= 1)` /
// `(col <= 12)` — structurally different, semantically identical. Comparing
// those definitions literally reports a false drift and demands the split
// form. To avoid that, both the desired (projected) and applied CHECK sets are
// folded into a CANONICAL SET OF BOUND ATOMS (`col op literal`) before the
// diff: a combined predicate decomposes into the same atoms its split
// equivalents produce, so the two spellings compare equal — while a genuinely
// changed bound (a different literal or operator) still yields a different atom
// and still reports drift.
//
// Only ordering-bound CHECKs (>=, >, <=, <, and BETWEEN, which expands to a
// >= / <= pair) are decomposed. Enum `IN (...)` / `= ANY(ARRAY[...])`, regex
// `~`, equality and every other predicate decompose to nothing and are matched
// by their whole normalized definition, exactly as before this change.

// canonicalCheckAtomSet folds a column's CHECK constraints into a canonical
// comparison set. Each ordering-bound CHECK contributes one atom per bound;
// every other CHECK contributes its whole normalized definition unchanged.
// Atoms and opaque definitions live in disjoint key namespaces so they never
// collide.
func canonicalCheckAtomSet(checks []schemadef.CheckConstraint) map[string]bool {
	out := make(map[string]bool, len(checks))
	for _, c := range checks {
		if atoms, ok := boundAtoms(c.Def); ok {
			for _, a := range atoms {
				out["atom:"+a] = true
			}
			continue
		}
		out["def:"+normDef(c.Def)] = true
	}
	return out
}

// boundAtoms decomposes a single CHECK definition into canonical bound atoms.
// It returns (atoms, true) ONLY when the ENTIRE predicate is a conjunction of
// ordering comparisons (>=, >, <=, <) — optionally spelled as BETWEEN, which
// this decomposer expands to a >= / <= pair the same way postgres does.
// Anything else (=, <>, IN, = ANY(...), regex ~, OR, an unparseable operand)
// yields (nil, false), leaving the caller to match it by whole definition.
func boundAtoms(def string) ([]string, bool) {
	inner, ok := checkInner(def)
	if !ok {
		return nil, false
	}
	inner = expandBetween(inner)
	inner = expandExactEquals(inner)
	conjuncts := splitTopLevelAnd(inner)
	if len(conjuncts) == 0 {
		return nil, false
	}
	atoms := make([]string, 0, len(conjuncts))
	for _, cj := range conjuncts {
		atom, ok := boundComparisonAtom(cj)
		if !ok {
			return nil, false // any non-bound conjunct → keep the whole CHECK opaque
		}
		atoms = append(atoms, atom)
	}
	return atoms, true
}

// checkInner returns the expression inside `CHECK (...)`, trimmed. It honors
// balanced parens (a nested predicate) and returns ok=false for anything that
// is not a CHECK definition.
func checkInner(def string) (string, bool) {
	s := strings.TrimSpace(def)
	if len(s) < len("CHECK") || !strings.EqualFold(s[:len("CHECK")], "CHECK") {
		return "", false
	}
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return "", false
	}
	end := matchParen(s, open)
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(s[open+1 : end]), true
}

// splitTopLevelAnd splits a boolean expression on ` AND ` at paren depth 0 and
// outside single-quoted string literals, after stripping any redundant
// enclosing parens. A predicate with a top-level OR (or no AND) comes back as a
// single element, which boundComparisonAtom then rejects if it is not a lone
// comparison.
func splitTopLevelAnd(expr string) []string {
	expr = stripOuterParens(expr)
	if expr == "" {
		return nil
	}
	var (
		parts   []string
		depth   int
		inQuote bool
		start   int
	)
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if inQuote {
			if c == '\'' {
				if i+1 < len(expr) && expr[i+1] == '\'' {
					i++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		case ' ':
			if depth == 0 && hasAndSep(expr, i) {
				parts = append(parts, strings.TrimSpace(expr[start:i]))
				i += len(" AND ") - 1
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(expr[start:]))
	return parts
}

// hasAndSep reports whether a case-insensitive ` AND ` separator begins at the
// space index i.
func hasAndSep(s string, i int) bool {
	const sep = " AND "
	if i+len(sep) > len(s) {
		return false
	}
	return strings.EqualFold(s[i:i+len(sep)], sep)
}

// boundComparisonAtom canonicalizes a single conjunct into a `col op literal`
// atom, returning ok=false unless it is exactly one ordering comparison. The
// operand order is normalized so the literal is always on the right
// (`1 <= col` ⇒ `col >= 1`), letting either hand-written spelling match forge's
// column-on-the-left idiom.
func boundComparisonAtom(cj string) (string, bool) {
	cj = stripOuterParens(cj)
	op, idx := findBoundOp(cj)
	if idx < 0 {
		return "", false
	}
	lhs := stripOuterParens(strings.TrimSpace(cj[:idx]))
	rhs := stripOuterParens(strings.TrimSpace(cj[idx+len(op):]))
	if lhs == "" || rhs == "" {
		return "", false
	}
	// A simple leaf holds exactly one comparison; a second operator means this
	// was not a plain bound (guard against malformed input).
	if _, extra := findBoundOp(rhs); extra >= 0 {
		return "", false
	}
	if isSQLLiteral(lhs) && !isSQLLiteral(rhs) {
		lhs, rhs, op = rhs, lhs, flipBoundOp(op)
	}
	return normWS(lhs) + " " + op + " " + normWS(rhs), true
}

// findBoundOp returns the first ordering operator (>=, <=, >, <) at paren
// depth 0 outside quotes, and its index. `<>` is skipped (inequality is not an
// ordering bound); `=` and everything else are never matched. idx is -1 when no
// ordering operator is present.
func findBoundOp(s string) (string, int) {
	depth := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		case '>':
			if depth == 0 {
				if i+1 < len(s) && s[i+1] == '=' {
					return ">=", i
				}
				return ">", i
			}
		case '<':
			if depth == 0 {
				if i+1 < len(s) && s[i+1] == '=' {
					return "<=", i
				}
				if i+1 < len(s) && s[i+1] == '>' {
					i++ // `<>` is inequality, not an ordering bound
					continue
				}
				return "<", i
			}
		}
	}
	return "", -1
}

// flipBoundOp reverses an ordering operator so its operands can be swapped.
func flipBoundOp(op string) string {
	switch op {
	case ">=":
		return "<="
	case "<=":
		return ">="
	case ">":
		return "<"
	case "<":
		return ">"
	}
	return op
}

// sqlLiteralRe matches a numeric literal (the only operand kind a
// protovalidate bound projects) — the "literal" side of a comparison, used to
// canonicalize operand order.
var sqlLiteralRe = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?$`)

// isSQLLiteral reports whether s is a comparison's literal operand: a numeric
// literal or a single-quoted string. Any other token (a bare column, a
// function call like char_length(col)) is treated as the value side.
func isSQLLiteral(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if s[0] == '\'' {
		return true
	}
	return sqlLiteralRe.MatchString(s)
}

// betweenRe expands `expr BETWEEN lo AND hi` into `expr >= lo AND expr <= hi`
// so a BETWEEN bound decomposes into the same atoms as the split form. postgres
// already rewrites BETWEEN in introspected definitions, so this is defensive
// (and covers directly-parsed spellings), matching a bare column or a
// single-argument function call against numeric or quoted bounds.
var betweenRe = regexp.MustCompile(`(?i)([A-Za-z_][A-Za-z0-9_]*(?:\s*\([^()]*\))?)\s+BETWEEN\s+('(?:[^']|'')*'|[+-]?[0-9.]+)\s+AND\s+('(?:[^']|'')*'|[+-]?[0-9.]+)`)

func expandBetween(s string) string {
	if !strings.Contains(strings.ToUpper(s), "BETWEEN") {
		return s
	}
	return betweenRe.ReplaceAllString(s, "$1 >= $2 AND $1 <= $3")
}

// exactEqualsRe expands `expr = <numeric>` (a bare column or single-argument
// function call equated to a numeric literal) into `expr >= N AND expr <= N`,
// so an exact-length / exact-value CHECK — `char_length(currency) = 3`,
// `exp_month = 6` — decomposes into the SAME bound atoms its BETWEEN and split
// (`>= N AND <= N`) equivalents produce. A fixed-length string field now
// projects to `= N` (fieldvalidate.SQLChecks), while a migration frozen at an
// earlier forge might carry `BETWEEN N AND N`; folding both to identical atoms
// keeps the two spellings from spuriously drifting. Only a NUMERIC right-hand
// side matches: `status = 'active'` (a string literal) and enum
// `col = ANY(ARRAY[...])` carry no `>=`/`<=` reading and stay opaque, matched by
// whole definition exactly as before. `>=`, `<=`, `<>`, `!=` never match — the
// captured left operand ends in a word char or `)`, so the operator directly
// after it is a lone `=`.
var exactEqualsRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*(?:\s*\([^()]*\))?)\s*=\s*([+-]?[0-9]+(?:\.[0-9]+)?)`)

func expandExactEquals(s string) string {
	if !strings.Contains(s, "=") {
		return s
	}
	return exactEqualsRe.ReplaceAllString(s, "$1 >= $2 AND $1 <= $2")
}

// stripOuterParens removes redundant parentheses that enclose the WHOLE
// expression (`((x))` → `x`), preserving inner grouping.
func stripOuterParens(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 && s[0] == '(' {
		if matchParen(s, 0) != len(s)-1 {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// normWS collapses interior whitespace to single spaces.
func normWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// columnLine returns the trimmed CREATE TABLE line whose first token is the
// given column name, with any trailing comma removed. "" when not found.
func columnLine(upSQL, col string) string {
	for _, ln := range strings.Split(upSQL, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		first := t
		if i := strings.IndexAny(t, " \t"); i >= 0 {
			first = t[:i]
		}
		if first == col {
			return strings.TrimRight(t, " \t,")
		}
	}
	return ""
}

// columnCheckClauses extracts the full `CHECK (...)` clause(s) from a
// column's rendered line, honoring single-quoted string literals when
// balancing parentheses (a protovalidate pattern may contain them).
func columnCheckClauses(upSQL, col string) []string {
	line := columnLine(upSQL, col)
	if line == "" {
		return nil
	}
	const marker = "CHECK ("
	var out []string
	for i := 0; i < len(line); {
		j := strings.Index(line[i:], marker)
		if j < 0 {
			break
		}
		start := i + j
		open := start + len(marker) - 1 // index of '('
		end := matchParen(line, open)
		if end < 0 {
			break
		}
		out = append(out, strings.TrimSpace(line[start:end+1]))
		i = end + 1
	}
	return out
}

// matchParen returns the index of the ')' matching the '(' at open, or -1.
// Content inside single-quoted string literals (” escapes a quote) is
// skipped so parens within a value or pattern don't confuse the balance.
func matchParen(s string, open int) int {
	depth := 0
	inQuote := false
	for i := open; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// plural returns the English plural suffix for n ("y"/"ies" callers append).
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
