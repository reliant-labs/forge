package seeddata

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/schemadef"
)

// Column-ordering CHECK constraints — the shape a per-column model cannot see.
//
// Every constraint model in this package keys on ONE column (EnumPools,
// CheckBounds, LengthBounds), and the introspection feeding them drops
// anything whose conkey names two columns (singleCheckColumn returns "").
// A `CHECK (expires_at > issued_at)` is therefore structurally invisible:
// the seeder draws each column independently, timestampLiteral hands every
// non-managed time column in a row the SAME instant, and the INSERT dies on
// a constraint the applied schema states plainly.
//
// That is the same rule the rest of this package already lives by
// (constraints.go): values FORGE synthesizes are satisfied BY CONSTRUCTION,
// because forge is the one that put the constraint there. Skipping the
// table is not an option — a plan that seeds nothing and reports success is
// the false green this whole mechanism exists to prevent.
//
// # What is satisfied, and how
//
// The recognized shape is a binary comparison between two columns of one
// table — `a > b`, `a >= b`, `a < b`, `a <= b`. It is the overwhelmingly
// common multi-column CHECK: validity windows (expires_at > issued_at),
// date ranges (ends_on >= starts_on), bounded pairs (max_qty >= min_qty).
//
// The comparison rarely stands alone. Optional timestamps are ordinary, so
// the constraint an author actually writes over a pair of them is
// NULL-GUARDED:
//
//	CHECK (issued_at IS NULL OR expires_at IS NULL OR expires_at > issued_at)
//
// which is the SAME ordering requirement — the guard only exempts rows where
// one side is absent. forge reads it structurally rather than by spelling: an
// OR is TRUE as soon as ONE branch is, and an AND is TRUE when EVERY branch
// is, so the question "does forge's placement satisfy this expression" has a
// three-line answer that does not care how many guards precede the
// comparison, in which order they appear, or whether the comparison is a
// conjunction of several. Everything else — COALESCE, CASE, NOT, an
// arithmetic offset (`b > a + interval '1 day'`) — is left to the warning
// below, because forge's placement does not prove those.
//
// Each recognized comparison is an EDGE in a per-table DAG: lo -> hi, "hi must
// sit above lo". Columns are ranked by longest path from a root, and a column
// at rank r takes the component's root value plus r steps. The ordering then
// holds by arithmetic rather than by luck:
//
//	value(hi) = base + rank(hi)*step, base = max(natural values of the roots)
//	rank(hi) >= rank(lo) + 1  ⇒  value(hi) > value(lo)
//
// Non-strict (>=) constraints are satisfied by the strict assignment too, so
// strictness is not tracked. Root columns keep the value they would have had
// anyway, so a schema with no ordering constraint seeds exactly as before and
// no existing dataset shifts.
//
// # What is NOT satisfied, and why that is stated rather than hidden
//
// A constraint forge cannot place — a cycle, a comparison between columns of
// different types, one involving a key or UNIQUE column another mechanism
// already owns, or an expression whose truth its placement does not prove
// (num_nonnulls, CASE, COALESCE, NOT, an offset comparison) — is left alone
// and recorded as a plan warning naming the constraint. The seed then either
// happens to satisfy it or fails at the INSERT with the full explanation
// Apply now attaches. Guessing at an arbitrary predicate would write rows
// that contradict a rule the schema states, which is worse than not writing
// them.

// orderRel is one recognized ordering constraint, normalized so hi must sit
// above lo. It carries the constraint's name so a refusal can point at the
// exact line of SQL.
type orderRel struct {
	lo, hi     string
	constraint string
}

// orderSlot is one column's place in its table's ordering DAG.
type orderSlot struct {
	// rank is the longest path from a root; 0 means "a root", which keeps
	// its natural synthesized value.
	rank int
	// roots are the rank-0 columns of this column's connected component,
	// sorted. The base value is the MAX of their natural values, which is
	// what makes the arithmetic above a proof rather than a hope.
	roots []string
	// kind is the canonical type shared by every column of the component.
	kind schemadef.CanonicalType
}

// orderCompareRE matches ONE column-to-column comparison in postgres's
// canonical spelling, e.g. `expires_at > issued_at`. Both operands must be
// bare column references: a cast, a function call, or an arithmetic offset
// (`issued_at + '1 day'::interval`) deliberately does not match, because a
// placement forge cannot prove satisfies the expression is a placement it
// must not make.
var orderCompareRE = regexp.MustCompile(
	`^"?([a-z_][a-z0-9_]*)"?\s*(>=|>|<=|<)\s*"?([a-z_][a-z0-9_]*)"?$`)

// orderRelsFromCheck reads the ordering edges off an introspected CHECK that
// forge's placement is PROVEN to satisfy. ok is false for every expression
// whose truth the placement does not establish — those are named by the
// caller's warning rather than guessed at.
func orderRelsFromCheck(ck schemadef.CheckConstraint) ([]orderRel, bool) {
	body, ok := checkBody(ck.Def)
	if !ok {
		return nil, false
	}
	rels, ok := provableOrdering(body)
	if !ok || len(rels) == 0 {
		return nil, false
	}
	// Every parsed column must be one postgres says the constraint spans, or
	// the expression was not what the matcher thought it was.
	spans := make(map[string]bool, len(ck.Columns))
	for _, c := range ck.Columns {
		spans[c] = true
	}
	for i := range rels {
		if !spans[rels[i].lo] || !spans[rels[i].hi] {
			return nil, false
		}
		rels[i].constraint = ck.Name
	}
	return rels, true
}

// checkBody strips the `CHECK (...)` wrapper — and the `NOT VALID` suffix a
// constraint added over existing rows carries — off an introspected
// definition, leaving the bare expression. A NOT VALID constraint is still
// enforced on every INSERT, so it is satisfied exactly like a validated one.
func checkBody(def string) (string, bool) {
	s := strings.TrimSpace(def)
	s = strings.TrimSpace(strings.TrimSuffix(s, "NOT VALID"))
	if !strings.HasPrefix(s, "CHECK (") || !strings.HasSuffix(s, ")") {
		return "", false
	}
	return strings.TrimSpace(s[len("CHECK (") : len(s)-1]), true
}

// provableOrdering returns the ordering edges that make expr TRUE under
// forge's placement, which assigns each ranked column a value strictly above
// its lower bound. ok is false when the placement does not PROVE the
// expression — never when it merely might hold.
//
// The whole reading is three rules, and the soundness of each is the
// definition of the operator:
//
//   - OR: true as soon as ONE branch is, so the first placeable branch is
//     enough and the others (`issued_at IS NULL`, `expires_at IS NULL`, an
//     unrelated escape hatch) need no attention at all.
//   - AND: true only when EVERY branch is, so every branch must be placeable
//     and every edge it names is required.
//   - a comparison of two columns: the edge itself.
//
// A NULL guard is therefore not stripped, normalized, or special-cased — it
// is just a disjunct the OR rule steps over, which is why guard order,
// guard count, and one-sided guards all read the same.
func provableOrdering(expr string) ([]orderRel, bool) {
	e := unwrapParens(expr)
	if parts := splitTopLevel(e, "OR"); len(parts) > 1 {
		for _, part := range parts {
			if rels, ok := provableOrdering(part); ok {
				return rels, true
			}
		}
		return nil, false
	}
	if parts := splitTopLevel(e, "AND"); len(parts) > 1 {
		var all []orderRel
		for _, part := range parts {
			rels, ok := provableOrdering(part)
			if !ok {
				return nil, false
			}
			all = append(all, rels...)
		}
		return all, true
	}
	m := orderCompareRE.FindStringSubmatch(e)
	if m == nil {
		return nil, false
	}
	left, op, right := m[1], m[2], m[3]
	if left == right {
		return nil, false
	}
	if strings.HasPrefix(op, ">") {
		return []orderRel{{lo: right, hi: left}}, true
	}
	return []orderRel{{lo: left, hi: right}}, true
}

// sqlScan walks a canonical constraint expression byte by byte, tracking
// parenthesis depth and single-quoted literals. Both readers below need
// exactly this and nothing more: postgres parenthesizes every boolean
// operand, and a quoted literal may contain either a parenthesis or the
// word OR.
type sqlScan struct {
	depth int
	quote bool
}

// step consumes expr[i] and returns the next index to read — i+2 across the
// doubled quote that escapes one inside a literal, i+1 otherwise.
func (s *sqlScan) step(expr string, i int) int {
	switch c := expr[i]; {
	case s.quote:
		if c == '\'' {
			if i+1 < len(expr) && expr[i+1] == '\'' {
				return i + 2
			}
			s.quote = false
		}
	case c == '\'':
		s.quote = true
	case c == '(':
		s.depth++
	case c == ')':
		s.depth--
	}
	return i + 1
}

// atTop reports whether the current position is ordinary expression text —
// not nested in parentheses and not inside a string literal.
func (s *sqlScan) atTop() bool { return s.depth == 0 && !s.quote }

// splitTopLevel splits expr on a boolean operator appearing at parenthesis
// depth 0 outside any literal, which is the only place postgres's canonical
// spelling puts one. A single-element result means the operator is absent.
func splitTopLevel(expr, op string) []string {
	pad := " " + op + " "
	var (
		parts []string
		sc    sqlScan
		start int
	)
	for i := 0; i < len(expr); {
		if sc.atTop() && strings.HasPrefix(expr[i:], pad) {
			parts = append(parts, expr[start:i])
			i += len(pad)
			start = i
			continue
		}
		i = sc.step(expr, i)
	}
	return append(parts, expr[start:])
}

// unwrapParens removes parentheses that wrap the WHOLE expression. A leading
// paren that closes before the end is a different pair — `(a) OR (b)` is not
// a wrapped expression — so depth reaching 0 early ends the unwrapping.
func unwrapParens(expr string) string {
	for {
		expr = strings.TrimSpace(expr)
		if len(expr) < 2 || expr[0] != '(' || expr[len(expr)-1] != ')' {
			return expr
		}
		var sc sqlScan
		for i := 0; i < len(expr); {
			next := sc.step(expr, i)
			if sc.depth == 0 && next < len(expr) {
				return expr
			}
			i = next
		}
		expr = expr[1 : len(expr)-1]
	}
}

// buildOrderChains resolves every table's ordering DAG into per-column
// slots, and returns the warnings for the constraints it could not place.
func buildOrderChains(tables []schemadef.Table) (map[string]map[string]orderSlot, []string) {
	chains := map[string]map[string]orderSlot{}
	var warns []string
	for _, t := range tables {
		slots, w := tableOrderChains(t)
		warns = append(warns, w...)
		if len(slots) > 0 {
			chains[t.Name] = slots
		}
	}
	sort.Strings(warns)
	return chains, warns
}

// orderEligible reports whether a column may be PLACED by the ordering pass:
// it must be a plain synthesized data column of a comparable type. Keys,
// foreign keys and single-column-UNIQUE columns are owned by other
// mechanisms (referential derivation, distinct-value assignment) whose
// values this pass must not overwrite.
func orderEligible(t schemadef.Table, name string) (schemadef.Column, bool) {
	var col schemadef.Column
	found := false
	for _, c := range t.Columns {
		if c.Name == name {
			col, found = c, true
			break
		}
	}
	if !found || col.IsPK || col.IsGenerated || col.IsArray {
		return col, false
	}
	switch col.Type {
	case schemadef.TypeTime, schemadef.TypeInt, schemadef.TypeFloat:
	default:
		return col, false
	}
	for _, fk := range t.ForeignKeys {
		if fk.Column == name {
			return col, false
		}
	}
	if uniqueSingleColumn(t, name) {
		return col, false
	}
	return col, true
}

// tableOrderChains ranks one table's ordering constraints.
func tableOrderChains(t schemadef.Table) (map[string]orderSlot, []string) {
	var (
		rels  []orderRel
		warns []string
	)
	refuse := func(name, why string) {
		warns = append(warns, fmt.Sprintf(
			"seed plan: %s constraint %q spans two columns and forge cannot place its values (%s) — seeded rows satisfy it only by chance",
			t.Name, name, why))
	}
	for _, ck := range t.Checks {
		if len(ck.Columns) < 2 {
			continue
		}
		found, ok := orderRelsFromCheck(ck)
		if !ok {
			refuse(ck.Name, "not a two-column ordering comparison")
			continue
		}
		// One constraint can name several edges (`b > a AND c > b`), and an
		// AND needs all of them: an edge forge may not place leaves the
		// WHOLE constraint unsatisfied, so the refusal is all-or-nothing.
		why := ""
		for _, rel := range found {
			lo, okLo := orderEligible(t, rel.lo)
			hi, okHi := orderEligible(t, rel.hi)
			switch {
			case !okLo || !okHi:
				why = "one side is a key, a reference, a UNIQUE column or a non-comparable type"
			case lo.Type != hi.Type:
				why = fmt.Sprintf("%s is %s but %s is %s", rel.lo, lo.Type, rel.hi, hi.Type)
			}
			if why != "" {
				break
			}
		}
		if why != "" {
			refuse(ck.Name, why)
			continue
		}
		rels = append(rels, found...)
	}
	if len(rels) == 0 {
		return nil, warns
	}

	ranks, ok := rankColumns(rels)
	if !ok {
		names := map[string]bool{}
		for _, r := range rels {
			names[r.constraint] = true
		}
		for name := range names {
			refuse(name, "the ordering constraints form a cycle, which no assignment satisfies")
		}
		return nil, warns
	}

	comps := components(rels)
	kindOf := map[string]schemadef.CanonicalType{}
	for _, r := range rels {
		c, _ := orderEligible(t, r.lo)
		kindOf[r.lo] = c.Type
		c, _ = orderEligible(t, r.hi)
		kindOf[r.hi] = c.Type
	}

	slots := map[string]orderSlot{}
	for _, comp := range comps {
		var roots []string
		for _, c := range comp {
			if ranks[c] == 0 {
				roots = append(roots, c)
			}
		}
		sort.Strings(roots)
		for _, c := range comp {
			slots[c] = orderSlot{rank: ranks[c], roots: roots, kind: kindOf[c]}
		}
	}
	return slots, warns
}

// rankColumns assigns each column the length of the longest path reaching
// it. ok is false when the edges contain a cycle.
func rankColumns(rels []orderRel) (map[string]int, bool) {
	indeg := map[string]int{}
	out := map[string][]string{}
	for _, r := range rels {
		if _, ok := indeg[r.lo]; !ok {
			indeg[r.lo] = 0
		}
		indeg[r.hi]++
		out[r.lo] = append(out[r.lo], r.hi)
	}
	rank := map[string]int{}
	var queue []string
	for c, d := range indeg {
		if d == 0 {
			queue = append(queue, c)
		}
	}
	sort.Strings(queue)
	seen := 0
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		seen++
		for _, next := range out[c] {
			if rank[c]+1 > rank[next] {
				rank[next] = rank[c] + 1
			}
			if indeg[next]--; indeg[next] == 0 {
				queue = append(queue, next)
				sort.Strings(queue)
			}
		}
	}
	if seen != len(indeg) {
		return nil, false
	}
	return rank, true
}

// components groups the columns into weakly-connected sets, each returned
// in sorted order.
func components(rels []orderRel) [][]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(c string) string {
		p, ok := parent[c]
		if !ok || p == c {
			parent[c] = c
			return c
		}
		root := find(p)
		parent[c] = root
		return root
	}
	for _, r := range rels {
		a, b := find(r.lo), find(r.hi)
		if a != b {
			parent[a] = b
		}
	}
	byRoot := map[string][]string{}
	for c := range parent {
		root := find(c)
		byRoot[root] = append(byRoot[root], c)
	}
	roots := make([]string, 0, len(byRoot))
	for r := range byRoot {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	out := make([][]string, 0, len(roots))
	for _, r := range roots {
		cols := byRoot[r]
		sort.Strings(cols)
		out = append(out, cols)
	}
	return out
}

// OrderStepDays is how far apart consecutive ranks of a TIME chain sit — a
// month, which reads as a plausible validity window rather than an
// artifact. Exported because the scaffolded lifecycle test's create fixtures
// place their timestamps the same way (see codegen's crudTestFixtures):
// there is ONE ordering model, so a born fixture and the seeded row it
// mirrors can never disagree.
const OrderStepDays = 30

// OrderRanks returns the ordering rank of every column of t that a PLACEABLE
// ordering CHECK governs: 0 for a chain's root (which keeps its natural
// value) and r for a column that must sit r steps above it. A column absent
// from the map is governed by no ordering constraint forge can place, and
// takes its independent value.
//
// This is the same resolution the seed plan performs, exposed for the OTHER
// half of forge's generated data — the create-request fixtures in the born
// lifecycle test, which mint rows against the very same schema.
func OrderRanks(t schemadef.Table) map[string]int {
	slots, _ := tableOrderChains(t)
	if len(slots) == 0 {
		return nil
	}
	ranks := make(map[string]int, len(slots))
	for col, slot := range slots {
		ranks[col] = slot.rank
	}
	return ranks
}

// orderedLiteral returns the literal for a column placed by an ordering
// constraint. ok is false for root columns (which keep their natural value)
// and for any column whose placement would violate a bound the schema
// states elsewhere — the caller then falls back to independent synthesis and
// the INSERT reports the conflict rather than forge silently choosing which
// constraint to honor.
func (p *Plan) orderedLiteral(tp tablePlan, col schemadef.Column, i int) (string, bool) {
	slot, ok := p.orderChains[tp.table.Name][col.Name]
	if !ok || slot.rank == 0 {
		return "", false
	}
	base, ok := p.chainBase(tp, slot, i)
	if !ok {
		return "", false
	}
	switch slot.kind {
	case schemadef.TypeTime:
		at, perr := time.Parse(time.RFC3339, base)
		if perr != nil {
			return "", false
		}
		return sqlString(at.AddDate(0, 0, OrderStepDays*slot.rank).Format("2006-01-02T15:04:05Z")), true
	case schemadef.TypeInt:
		v, perr := strconv.ParseInt(base, 10, 64)
		if perr != nil {
			return "", false
		}
		want := v + int64(slot.rank)
		if b, has := p.bounds.get(tp.table.Name, col.Name); has && b.clamp(want) != want {
			return "", false // the range CHECK cannot hold the ordered value
		}
		return strconv.FormatInt(want, 10), true
	case schemadef.TypeFloat:
		v, perr := strconv.ParseFloat(base, 64)
		if perr != nil {
			return "", false
		}
		want := v + float64(slot.rank)
		if b, has := p.bounds.get(tp.table.Name, col.Name); has {
			if (b.Min != nil && want < float64(*b.Min)) || (b.Max != nil && want > float64(*b.Max)) {
				return "", false // the range CHECK cannot hold the ordered value
			}
		}
		return fmt.Sprintf("%.2f", want), true
	}
	return "", false
}

// chainBase returns the raw value the component's ranks are measured from:
// the LARGEST natural value among its root columns. Taking the max is what
// makes rank arithmetic a proof — every non-root sits above every root, not
// just above the one that happened to be picked.
func (p *Plan) chainBase(tp tablePlan, slot orderSlot, i int) (string, bool) {
	t := p.byName[tp.table.Name]
	best, have := "", false
	for _, root := range slot.roots {
		col, ok := orderEligible(t, root)
		if !ok {
			continue
		}
		raw, ok := decodeScalarLiteral(p.valueLiteral(tp.table.Name, col, i))
		if !ok {
			continue
		}
		if !have || orderGreater(slot.kind, raw, best) {
			best, have = raw, true
		}
	}
	return best, have
}

// orderGreater compares two raw values in the component's type. A value that
// does not parse loses, so an unparseable root can never become the base.
func orderGreater(kind schemadef.CanonicalType, a, b string) bool {
	switch kind {
	case schemadef.TypeTime:
		at, aerr := time.Parse(time.RFC3339, a)
		bt, berr := time.Parse(time.RFC3339, b)
		return aerr == nil && (berr != nil || at.After(bt))
	case schemadef.TypeInt:
		av, aerr := strconv.ParseInt(a, 10, 64)
		bv, berr := strconv.ParseInt(b, 10, 64)
		return aerr == nil && (berr != nil || av > bv)
	case schemadef.TypeFloat:
		av, aerr := strconv.ParseFloat(a, 64)
		bv, berr := strconv.ParseFloat(b, 64)
		return aerr == nil && (berr != nil || av > bv)
	}
	return false
}
