package dbanalyze

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// rawPlan is one node of postgres's `EXPLAIN (VERBOSE, FORMAT JSON)`.
type rawPlan struct {
	NodeType    string     `json:"Node Type"`
	Operation   string     `json:"Operation"`
	Relation    string     `json:"Relation Name"`
	Schema      string     `json:"Schema"`
	Alias       string     `json:"Alias"`
	Index       string     `json:"Index Name"`
	IndexCond   string     `json:"Index Cond"`
	RecheckCond string     `json:"Recheck Cond"`
	Filter      string     `json:"Filter"`
	HashCond    string     `json:"Hash Cond"`
	MergeCond   string     `json:"Merge Cond"`
	JoinFilter  string     `json:"Join Filter"`
	SortKey     []string   `json:"Sort Key"`
	Disabled    bool       `json:"Disabled"`
	Parent      string     `json:"Parent Relationship"`
	Subplan     string     `json:"Subplan Name"`
	Rows        float64    `json:"Plan Rows"`
	Cost        float64    `json:"Total Cost"`
	Plans       []*rawPlan `json:"Plans"`

	up *rawPlan
}

// parsePlan reads an EXPLAIN (FORMAT JSON) document.
func parsePlan(doc []byte) (*rawPlan, error) {
	var out []struct {
		Plan *rawPlan `json:"Plan"`
	}
	if err := json.Unmarshal(doc, &out); err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	if len(out) != 1 || out[0].Plan == nil {
		return nil, fmt.Errorf("read the plan: %d plans, not one", len(out))
	}
	var link func(n *rawPlan)
	link = func(n *rawPlan) {
		for _, c := range n.Plans {
			c.up = n
			link(c)
		}
	}
	link(out[0].Plan)
	return out[0].Plan, nil
}

// walk visits n and every node below it.
func (n *rawPlan) walk(fn func(*rawPlan)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// relName names the node's relation as the document names relations.
func (n *rawPlan) relName() string {
	if n.Schema == "" || n.Schema == "public" {
		return n.Relation
	}
	return n.Schema + "." + n.Relation
}

// trim is the node as the document carries it.
func (n *rawPlan) trim() *PlanNode {
	out := &PlanNode{
		NodeType: n.NodeType, Operation: n.Operation, Alias: n.Alias, Index: n.Index,
		IndexCond: n.IndexCond, RecheckCond: n.RecheckCond, Filter: n.Filter,
		HashCond: n.HashCond, MergeCond: n.MergeCond, JoinFilter: n.JoinFilter,
		SortKey: n.SortKey, Disabled: n.Disabled, Subplan: n.Subplan, Rows: n.Rows, Cost: n.Cost,
	}
	if n.Relation != "" {
		out.Relation = n.relName()
	}
	for _, c := range n.Plans {
		out.Plans = append(out.Plans, *c.trim())
	}
	return out
}

// isScan reports whether the node reads a table's rows (a ModifyTable node
// names its target relation too, but reads nothing itself).
func (n *rawPlan) isScan() bool {
	switch n.NodeType {
	case "Seq Scan", "Index Scan", "Index Only Scan", "Bitmap Heap Scan", "Tid Scan", "Tid Range Scan":
		return n.Relation != ""
	}
	return false
}

func (n *rawPlan) isIndexScan() bool {
	return n.NodeType == "Index Scan" || n.NodeType == "Index Only Scan"
}

// ancestors are the nodes above n within its own query level: the walk stops
// at an InitPlan or SubPlan, whose LIMIT or sort is another statement's.
func (n *rawPlan) ancestors() []*rawPlan {
	var out []*rawPlan
	for cur := n; cur.up != nil; cur = cur.up {
		if cur.Parent == "InitPlan" || cur.Parent == "SubPlan" {
			break
		}
		out = append(out, cur.up)
	}
	return out
}

// indexUses are the indexes a scan reads and their Index Conds: its own, or
// a Bitmap Heap Scan's bitmap index scans.
func (n *rawPlan) indexUses() []*rawPlan {
	if n.isIndexScan() {
		return []*rawPlan{n}
	}
	var out []*rawPlan
	if n.NodeType == "Bitmap Heap Scan" {
		for _, c := range n.Plans {
			c.walk(func(b *rawPlan) {
				if b.NodeType == "Bitmap Index Scan" {
					out = append(out, b)
				}
			})
		}
	}
	return out
}

// feedsLimitInOrder reports whether an index scan's rows reach a LIMIT in
// the index's order: nothing between re-sorts or hashes them, so the index
// delivers the ORDER BY … LIMIT and the scan stops early.
func (n *rawPlan) feedsLimitInOrder() bool {
	cur := n
	for _, a := range n.ancestors() {
		switch a.NodeType {
		case "Limit":
			return true
		case "Result", "Subquery Scan", "Unique", "Incremental Sort", "Gather Merge", "Append", "Merge Append":
		case "Nested Loop":
			// A nested loop keeps its outer side's order.
			if len(a.Plans) == 0 || a.Plans[0] != cur {
				return false
			}
		default:
			return false
		}
		cur = a
	}
	return false
}

// sortedUnderLimit is the sort key of a Sort between n and a LIMIT above it:
// every row is sorted to return the first few. Nil when there is none.
func (n *rawPlan) sortedUnderLimit() []string {
	var keys []string
	for _, a := range n.ancestors() {
		switch a.NodeType {
		case "Sort", "Incremental Sort":
			if keys == nil {
				keys = append([]string{}, a.SortKey...)
			}
		case "Limit":
			return keys
		}
	}
	return nil
}

// predicates are the node's condition strings.
func (n *rawPlan) predicates() []string {
	var out []string
	for _, p := range []string{n.IndexCond, n.RecheckCond, n.Filter, n.HashCond, n.MergeCond, n.JoinFilter} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// relInfo is what the catalog says of a scanned relation.
type relInfo struct {
	// table names the table the document reports: a partition's root.
	table     string
	reltuples float64
	// indexes are the relation's own valid indexes, by name.
	indexes map[string]indexInfo
}

type indexInfo struct {
	partial bool
	// leading is the first key column, "" for an expression.
	leading string
}

// leadingIndex is a non-partial index of r whose first column is one of cols.
func (r *relInfo) leadingIndex(cols []string) string {
	names := make([]string, 0, len(r.indexes))
	for name := range r.indexes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ix := r.indexes[name]
		for _, c := range cols {
			if !ix.partial && ix.leading != "" && ix.leading == c {
				return name
			}
		}
	}
	return ""
}

// lookupFunc resolves a scanned relation (schema, name) in the catalog.
type lookupFunc func(schema, name string) (*relInfo, error)

// classified is one table's access with what sizing it needs.
type classified struct {
	TableAccess
	reltuples float64
}

// classify decides, per table, whether the no-seqscan plan serves the
// statement's filters, joins and ORDER BY … LIMIT with an index, and marks
// the tables the normal plan sequentially scans.
//
// A scan is served when it has an Index Cond (its own, or a bitmap index
// scan's), reads a partial index (whose predicate implies the filter: the
// planner drops the condition), or is an index scan whose order reaches a
// LIMIT untouched. Otherwise it is unserved when it evaluates a Filter on
// every row — a disabled Seq Scan, or an index walked in full for want of a
// usable one —, when every row is sorted for a LIMIT, or when it is read in
// full to be joined in a statement that restricts something and no index
// leads with its join column. A full read that nothing constrains is
// unfiltered: no index would help it.
func classify(normal, noSeq *rawPlan, lookup lookupFunc) ([]classified, error) {
	aliases := map[string]bool{}
	noSeq.walk(func(n *rawPlan) {
		if n.Alias != "" {
			aliases[n.Alias] = true
		}
	})
	partialUse := func(n *rawPlan, ri *relInfo) string {
		for _, u := range n.indexUses() {
			if ri.indexes[u.Index].partial {
				return u.Index
			}
		}
		return ""
	}

	type scan struct {
		n  *rawPlan
		ri *relInfo
	}
	var scans []scan
	var lookupErr error
	noSeq.walk(func(n *rawPlan) {
		if !n.isScan() || lookupErr != nil {
			return
		}
		ri, err := lookup(n.Schema, n.Relation)
		if err != nil {
			lookupErr = err
			return
		}
		scans = append(scans, scan{n, ri})
	})
	if lookupErr != nil {
		return nil, lookupErr
	}

	// A statement restricts rows when some predicate names at most one
	// relation (a filter, not a join condition), or a partial index implies
	// one.
	restricted := false
	noSeq.walk(func(n *rawPlan) {
		for _, p := range n.predicates() {
			if len(referenced(p, aliases)) <= 1 {
				restricted = true
			}
		}
	})
	for _, sc := range scans {
		if partialUse(sc.n, sc.ri) != "" {
			restricted = true
		}
	}

	byTable := map[string]*classified{}
	var order []string
	for _, sc := range scans {
		a := accessOf(sc.n, sc.ri, noSeq, aliases, restricted, partialUse(sc.n, sc.ri))
		cur, ok := byTable[sc.ri.table]
		if !ok {
			byTable[sc.ri.table] = &classified{TableAccess: a, reltuples: sc.ri.reltuples}
			order = append(order, sc.ri.table)
			continue
		}
		if rank(a.Verdict) > rank(cur.Verdict) {
			cur.TableAccess = a
		}
	}

	seq := map[string]bool{}
	var seqErr error
	normal.walk(func(n *rawPlan) {
		if n.NodeType != "Seq Scan" || n.Relation == "" || seqErr != nil {
			return
		}
		ri, err := lookup(n.Schema, n.Relation)
		if err != nil {
			seqErr = err
			return
		}
		seq[ri.table] = true
		if _, ok := byTable[ri.table]; !ok {
			byTable[ri.table] = &classified{TableAccess: TableAccess{Table: ri.table, Verdict: AccessUnfiltered, Scan: n.NodeType}, reltuples: ri.reltuples}
			order = append(order, ri.table)
		}
	})
	if seqErr != nil {
		return nil, seqErr
	}
	out := make([]classified, 0, len(order))
	for _, t := range order {
		c := byTable[t]
		c.SeqScan = seq[t]
		out = append(out, *c)
	}
	return out, nil
}

// accessOf classifies one scan node of the no-seqscan plan.
func accessOf(n *rawPlan, ri *relInfo, root *rawPlan, aliases map[string]bool, restricted bool, partial string) TableAccess {
	a := TableAccess{Table: ri.table, Scan: n.NodeType}
	served := func(how, index, detail string) TableAccess {
		a.Verdict, a.How, a.Index, a.Detail = AccessServed, how, index, detail
		return a
	}
	unserved := func(how, detail string) TableAccess {
		a.Verdict, a.How, a.Detail = AccessUnserved, how, detail
		if n.isIndexScan() {
			a.Index = n.Index
		}
		return a
	}
	if n.NodeType == "Tid Scan" || n.NodeType == "Tid Range Scan" {
		return served(ServedByTID, "", firstNonEmpty(n.IndexCond, n.Filter))
	}
	for _, u := range n.indexUses() {
		if u.IndexCond != "" {
			return served(ServedByIndexCond, u.Index, u.IndexCond)
		}
	}
	if partial != "" {
		return served(ServedByPartialIndex, partial, n.RecheckCond)
	}
	if n.isIndexScan() && n.feedsLimitInOrder() {
		return served(ServedByOrder, n.Index, "")
	}
	if n.Filter != "" {
		return unserved(UnservedFilter, n.Filter)
	}
	if keys := n.sortedUnderLimit(); keys != nil {
		return unserved(UnservedOrder, strings.Join(keys, ", "))
	}
	if cols := joinColumns(n.Alias, root, aliases); len(cols) > 0 && restricted {
		if idx := ri.leadingIndex(cols); idx != "" {
			return served(ServedByJoinKey, idx, strings.Join(cols, ", "))
		}
		return unserved(UnservedJoin, strings.Join(cols, ", "))
	}
	a.Verdict = AccessUnfiltered
	return a
}

func rank(verdict string) int {
	switch verdict {
	case AccessUnserved:
		return 2
	case AccessServed:
		return 1
	}
	return 0
}

// joinColumns are the columns of alias that a predicate joining it to
// another relation names, sorted.
func joinColumns(alias string, root *rawPlan, aliases map[string]bool) []string {
	if alias == "" {
		return nil
	}
	set := map[string]bool{}
	root.walk(func(n *rawPlan) {
		for _, p := range n.predicates() {
			refs := referenced(p, aliases)
			if len(refs) < 2 || !refs[alias] {
				continue
			}
			for _, m := range qualifiedRE(alias).FindAllStringSubmatch(p, -1) {
				set[unquoteIdent(m[1])] = true
			}
		}
	})
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// referenced are the aliases a VERBOSE predicate qualifies a column with.
func referenced(pred string, aliases map[string]bool) map[string]bool {
	out := map[string]bool{}
	for a := range aliases {
		if qualifiedRE(a).MatchString(pred) {
			out[a] = true
		}
	}
	return out
}

var qualifiedCache sync.Map // alias → *regexp.Regexp

// qualifiedRE matches `alias.column` (either part may be quoted) and
// captures the column.
func qualifiedRE(alias string) *regexp.Regexp {
	if re, ok := qualifiedCache.Load(alias); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?:^|[^\w$."])(?:` + regexp.QuoteMeta(alias) + `|"` +
		regexp.QuoteMeta(strings.ReplaceAll(alias, `"`, `""`)) + `")\.("(?:[^"]|"")+"|[\w$]+)`)
	qualifiedCache.Store(alias, re)
	return re
}

func unquoteIdent(s string) string {
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2 {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
