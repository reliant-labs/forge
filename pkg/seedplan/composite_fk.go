package seedplan

import (
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// Composite foreign keys: one parent row, every column.
//
// `FOREIGN KEY (tenant_id, strategy_id) REFERENCES strategies (tenant_id, id)`
// is the standard way to make "a child references only a parent in its own
// tenant" a database fact. It says more than its columns do: strategy_id must
// be some strategy, tenant_id must be some strategy's tenant, AND both must be
// read off the SAME strategy row. Seeding each column on its own satisfies the
// first two and violates the third on nearly every row.
//
// So a composite is placed whole. The planner picks ONE parent row for it —
// for its anchor, the member that references the parent's primary key when
// one does — and every column the composite places takes the value that row
// holds in the referenced column. A single-column foreign key beside it
// (hypotheses.tenant_id -> tenants.id, born with the table) is then satisfied
// by the parent's own reference: the strategy's tenant_id is a real tenant.
//
// # Two composites sharing a column
//
// content_items (tenant_id, hypothesis_id) -> hypotheses and (tenant_id,
// channel_id) -> channels share tenant_id, which can hold one value. The
// first composite (constraint-name order, so the plan is the same on every
// run) places it. The second picks its parent row among the rows that AGREE
// on every column an earlier composite already placed — channels of that
// tenant — and places only the columns nothing placed before it. When no
// parent row agrees, the second composite degrades to its own pick and
// postgres reports the violation by name; forge does not invent a parent.
//
// A composite settles its columns outright: it is the schema's own statement
// of which values go together, so a `forge:ref` declaration on one of those
// columns cannot move it (see findDiamonds), and the diamond it would
// otherwise describe is not reported.

// compositeFK is one multi-column foreign-key constraint on a table.
type compositeFK struct {
	name     string
	refTable string
	members  []schemadef.ForeignKey // one per column, key order
	// anchor indexes the member the parent row is picked for: the one that
	// references the parent's single-column primary key, else the first.
	anchor int
	// owns[k] reports whether this composite places members[k]: no composite
	// earlier in the table's list contains that column.
	owns []bool
}

func (c compositeFK) column(k int) string { return c.members[k].Column }

// buildCompositeFKs groups a table's composite constraints, in constraint-name
// order, and resolves which one places each member column. owner maps a column
// to the index of the composite that places it.
func buildCompositeFKs(t schemadef.Table, byName map[string]schemadef.Table) (list []compositeFK, owner map[string]int) {
	byConstraint := map[string][]schemadef.ForeignKey{}
	for _, fk := range t.ForeignKeys {
		if len(fk.KeyColumns) > 1 && fk.Name != "" {
			byConstraint[fk.Name] = append(byConstraint[fk.Name], fk)
		}
	}
	if len(byConstraint) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(byConstraint))
	for n := range byConstraint {
		names = append(names, n)
	}
	sort.Strings(names)

	owner = map[string]int{}
	for _, n := range names {
		members := byConstraint[n]
		if members[0].RefTable == t.Name {
			continue // a self-referencing composite resolves per row; not modelled
		}
		c := compositeFK{name: n, refTable: members[0].RefTable, members: members, owns: make([]bool, len(members))}
		ref := byName[c.refTable]
		for k, m := range members {
			if len(ref.PKCols) == 1 && ref.PKCols[0] == m.RefColumn {
				c.anchor = k
				break
			}
		}
		placesAny := false
		for k, m := range members {
			if _, taken := owner[m.Column]; !taken {
				c.owns[k] = true
				placesAny = true
			}
		}
		if !placesAny {
			continue // every column already placed: nothing left to decide
		}
		for k, m := range members {
			if c.owns[k] {
				owner[m.Column] = len(list)
			}
		}
		list = append(list, c)
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list, owner
}

// compositeOwnerOf returns the composite that places (table, column).
func (p *Plan) compositeOwnerOf(table, column string) (int, bool) {
	ci, ok := p.compositeOwner[table][column]
	return ci, ok
}

// compositeMemberRow is fkParentRow for a column a composite places: the
// composite's parent row. Only the anchor can decline the reference (a NULL
// anchor turns the constraint off under MATCH SIMPLE); a NOT NULL partner
// still takes a real parent's value, so it stays a valid reference on its own.
func (p *Plan) compositeMemberRow(tp tablePlan, ci int, column string, i, depth int) (int, bool) {
	row, anchorNull := p.compositeRow(tp, ci, i, depth)
	c := p.composites[tp.table.Name][ci]
	if anchorNull && c.column(c.anchor) == column {
		return 0, false
	}
	return row, true
}

// compositeRow resolves the parent row composite ci points at on row i.
func (p *Plan) compositeRow(tp tablePlan, ci, i, depth int) (row int, anchorNull bool) {
	table := tp.table.Name
	c := p.composites[table][ci]

	// The anchor's pick, as a single-column reference would make it — the
	// row-0 spine, the optional-edge coin, the 1-1 deal — but against the
	// composite's member, so it lands on the composite's parent table.
	anchorCP := columnPlan{}
	if _, cp, ok := p.colPlan(table, c.column(c.anchor)); ok {
		anchorCP = cp
	}
	anchorCP.fk = &c.members[c.anchor]
	anchorCP.selfRef = false
	base, ok := p.tupleParentRow(table, c.column(c.anchor), i)
	if !ok {
		base, ok = p.fkParentRowIndependent(tp, anchorCP, i)
	}
	if !ok {
		// Declined. The partners still need a parent for their own sake — one
		// the composites they pin can agree with.
		if cands := p.compositeCandidates(tp, ci, depth); len(cands) > 0 {
			return cands[pick(p.cfg.EffectiveSalt(), table, c.name, i, len(cands))], true
		}
		return pick(p.cfg.EffectiveSalt(), table, c.name, i, p.rowsOf[c.refTable]), true
	}

	// Columns an earlier composite placed pin this one's parent to rows that
	// agree with them.
	var positions []int
	var want []string
	for k := range c.members {
		if c.owns[k] || depth >= maxFKDepth {
			continue
		}
		oi, owned := p.compositeOwnerOf(table, c.column(k))
		if !owned {
			continue
		}
		o := p.composites[table][oi]
		orow, _ := p.compositeRow(tp, oi, i, depth+1)
		for _, m := range o.members {
			if m.Column == c.column(k) {
				positions = append(positions, k)
				want = append(want, p.referencedValue(o.refTable, m.RefColumn, orow, depth+1))
				break
			}
		}
	}
	if len(positions) == 0 {
		// Not pinned by an earlier composite — but a later one may be pinned
		// by THIS one, and a parent row whose shared value no later parent
		// carries leaves that composite nothing to agree with. Draw from the
		// rows every partner can match. A dealt anchor (1-1, composite
		// UNIQUE) is a bijection and is never narrowed.
		if anchorCP.uniqueFK || p.tupleOwns(table, c.column(c.anchor)) {
			return base, false
		}
		cands := p.compositeCandidates(tp, ci, depth)
		if len(cands) == 0 {
			return base, false // no partner, or none satisfiable: degrade
		}
		if i == 0 {
			return cands[0], false
		}
		return cands[pick(p.cfg.EffectiveSalt(), table, c.name, i, len(cands))], false
	}
	matches := p.compositeMatches(table, c, positions, depth)[strings.Join(want, "\x1f")]
	if len(matches) == 0 {
		return base, false // nothing agrees: degrade rather than invent a parent
	}
	if i == 0 {
		return matches[0], false
	}
	return matches[pick(p.cfg.EffectiveSalt(), table, c.name, i, len(matches))], false
}

// compositeCandidates returns the parent rows of composite ci that every later
// composite pinned only by ci can agree with, or nil when no later composite
// is pinned by ci alone. Memoized per finalize.
func (p *Plan) compositeCandidates(tp tablePlan, ci, depth int) []int {
	table := tp.table.Name
	key := table + "\x00" + strconv.Itoa(ci)
	if cands, ok := p.compositeCands[key]; ok {
		return cands
	}
	comps := p.composites[table]
	c := comps[ci]
	refColOf := map[string]string{}
	for _, m := range c.members {
		refColOf[m.Column] = m.RefColumn
	}

	type partner struct {
		c         compositeFK
		positions []int
		cols      []string
	}
	var partners []partner
	for j := ci + 1; j < len(comps); j++ {
		pc := partner{c: comps[j]}
		solely := true
		for k := range comps[j].members {
			if comps[j].owns[k] {
				continue
			}
			if oi, _ := p.compositeOwnerOf(table, comps[j].column(k)); oi != ci {
				solely = false
				break
			}
			pc.positions = append(pc.positions, k)
			pc.cols = append(pc.cols, comps[j].column(k))
		}
		if solely && len(pc.positions) > 0 {
			partners = append(partners, pc)
		}
	}

	var cands []int
	if len(partners) > 0 {
		cands = []int{}
		for r := 0; r < p.rowsOf[c.refTable]; r++ {
			ok := true
			for _, pc := range partners {
				vals := make([]string, len(pc.cols))
				for i, col := range pc.cols {
					vals[i] = p.referencedValue(c.refTable, refColOf[col], r, depth+1)
				}
				if len(p.compositeMatches(table, pc.c, pc.positions, depth)[strings.Join(vals, "\x1f")]) == 0 {
					ok = false
					break
				}
			}
			if ok {
				cands = append(cands, r)
			}
		}
	}
	if p.compositeCands == nil {
		p.compositeCands = map[string][]int{}
	}
	p.compositeCands[key] = cands
	return cands
}

// compositeMatches indexes the composite's parent rows by the values they hold
// at the given key positions. Memoized per finalize: the index is a pure
// function of the plan and its row counts.
func (p *Plan) compositeMatches(table string, c compositeFK, positions []int, depth int) map[string][]int {
	parts := make([]string, len(positions))
	for i, k := range positions {
		parts[i] = strconv.Itoa(k)
	}
	key := table + "\x00" + c.name + "\x00" + strings.Join(parts, ",")
	if idx, ok := p.compositeMemo[key]; ok {
		return idx
	}
	idx := map[string][]int{}
	vals := make([]string, len(positions))
	for r := 0; r < p.rowsOf[c.refTable]; r++ {
		for i, k := range positions {
			vals[i] = p.referencedValue(c.refTable, c.members[k].RefColumn, r, depth+1)
		}
		v := strings.Join(vals, "\x1f")
		idx[v] = append(idx[v], r)
	}
	if p.compositeMemo == nil {
		p.compositeMemo = map[string]map[string][]int{}
	}
	p.compositeMemo[key] = idx
	return idx
}
