package obsbackend

import (
	"sort"
	"time"
)

// Trace-tree caps (DESIGN §8.1, Sentry's overview discipline). --full lifts
// them.
const (
	MaxTreeDepth = 8
	MaxTreeSpans = 100
)

// TreeNode is one rendered node of a trace: a span, or a collapsed run of
// identical siblings.
type TreeNode struct {
	Span Span
	// Count > 1 marks a collapsed run: Count identical sibling spans (same
	// service, name and status) shown once, with Min/P50/P95/Max over their
	// durations.
	Count    int
	Min, Max time.Duration
	P50, P95 time.Duration
	// Error is true when this span (or, for a run, any span in it) failed.
	Error bool
	// Critical marks the spans on the critical path: from the root, the child
	// that ends last at each level.
	Critical bool
	// Offset is the span's start relative to the trace's first span.
	Offset   time.Duration
	Children []*TreeNode
	// Hidden counts descendants not rendered because of a cap, so the output
	// can say "… 12 more spans" instead of dropping them silently.
	Hidden int
}

// Tree is a rendered trace.
type Tree struct {
	Roots []*TreeNode
	// Spans is every span of the trace; Shown how many are rendered (a
	// collapsed run counts once).
	Spans, Shown int
	// Duration is first start to last end.
	Duration time.Duration
	// Orphans counts spans whose parent is not in the trace (a dropped or
	// not-yet-exported parent); they are rendered as extra roots.
	Orphans int
	// Capped reports that a cap hid something.
	Capped bool
}

// BuildTree arranges spans into a tree under the caps. full lifts them.
func BuildTree(spans []Span, full bool) Tree {
	t := Tree{Spans: len(spans)}
	if len(spans) == 0 {
		return t
	}
	byID := map[string]*Span{}
	for i := range spans {
		byID[spans[i].SpanID] = &spans[i]
	}
	kids := map[string][]*Span{}
	var roots []*Span
	for i := range spans {
		s := &spans[i]
		if s.ParentID == "" {
			roots = append(roots, s)
			continue
		}
		if _, ok := byID[s.ParentID]; !ok {
			roots = append(roots, s)
			t.Orphans++
			continue
		}
		kids[s.ParentID] = append(kids[s.ParentID], s)
	}
	order := func(ss []*Span) {
		sort.SliceStable(ss, func(i, j int) bool {
			if !ss[i].Start.Equal(ss[j].Start) {
				return ss[i].Start.Before(ss[j].Start)
			}
			return ss[i].SpanID < ss[j].SpanID
		})
	}
	order(roots)
	first, last := spans[0].Start, spans[0].Start.Add(time.Duration(spans[0].DurationNs))
	for _, s := range spans {
		if s.Start.Before(first) {
			first = s.Start
		}
		if e := s.Start.Add(time.Duration(s.DurationNs)); e.After(last) {
			last = e
		}
	}
	t.Duration = last.Sub(first)

	// failing[id]: this span or any descendant failed (error spans' ancestors
	// stay expanded so the failure is reachable).
	failing := map[string]bool{}
	var mark func(s *Span) bool
	mark = func(s *Span) bool {
		f := isErrorSpan(s)
		for _, c := range kids[s.SpanID] {
			if mark(c) {
				f = true
			}
		}
		failing[s.SpanID] = f
		return f
	}
	for _, r := range roots {
		mark(r)
	}
	// Critical path: from each root, follow the child that ends last.
	critical := map[string]bool{}
	var walk func(s *Span)
	walk = func(s *Span) {
		critical[s.SpanID] = true
		var best *Span
		var bestEnd time.Time
		for _, c := range kids[s.SpanID] {
			if e := c.Start.Add(time.Duration(c.DurationNs)); best == nil || e.After(bestEnd) {
				best, bestEnd = c, e
			}
		}
		if best != nil {
			walk(best)
		}
	}
	for _, r := range roots {
		walk(r)
	}

	budget := MaxTreeSpans
	if full {
		budget = len(spans) + 1
	}
	var build func(s *Span, depth int) *TreeNode
	// expand reports whether a span must be shown despite the caps.
	expand := func(s *Span) bool { return failing[s.SpanID] || critical[s.SpanID] }
	// split reports whether a span keeps its own node instead of folding into
	// a run of identical siblings: a failing one, or a critical one the path
	// continues through. A critical LEAF folds in and marks the run.
	split := func(s *Span) bool { return failing[s.SpanID] || (critical[s.SpanID] && len(kids[s.SpanID]) > 0) }

	subtree := func(s *Span) int { // spans under s, itself included
		n := 1
		var count func(p *Span)
		count = func(p *Span) {
			for _, c := range kids[p.SpanID] {
				n++
				count(c)
			}
		}
		count(s)
		return n
	}

	build = func(s *Span, depth int) *TreeNode {
		nd := &TreeNode{Span: *s, Count: 1, Error: isErrorSpan(s), Critical: critical[s.SpanID], Offset: s.Start.Sub(first)}
		budget--
		t.Shown++
		cs := kids[s.SpanID]
		order(cs)
		if !full && depth >= MaxTreeDepth {
			for _, c := range cs {
				nd.Hidden += subtree(c)
			}
			if nd.Hidden > 0 {
				t.Capped = true
			}
			return nd
		}
		// Group runs of identical siblings; a group with an expanded member
		// is split so that member keeps its own node.
		type group struct{ members []*Span }
		var groups []*group
		index := map[string]*group{}
		for _, c := range cs {
			if !full && split(c) {
				groups = append(groups, &group{members: []*Span{c}})
				continue
			}
			k := c.Service + "\x00" + c.Name + "\x00" + c.StatusCode
			if g, ok := index[k]; ok {
				g.members = append(g.members, c)
				continue
			}
			g := &group{members: []*Span{c}}
			index[k] = g
			groups = append(groups, g)
		}
		for _, g := range groups {
			if budget <= 0 && !(!full && expand(g.members[0])) {
				for _, m := range g.members {
					nd.Hidden += subtree(m)
				}
				t.Capped = true
				continue
			}
			if len(g.members) == 1 || full {
				for _, m := range g.members {
					nd.Children = append(nd.Children, build(m, depth+1))
				}
				continue
			}
			run := collapse(g.members, first, &budget, &t.Shown)
			for _, m := range g.members {
				run.Critical = run.Critical || critical[m.SpanID]
			}
			nd.Children = append(nd.Children, run)
			// The run itself is shown as ×N; what is not shown is what hangs
			// under its members.
			for _, m := range g.members {
				nd.Hidden += subtree(m) - 1
			}
		}
		if nd.Hidden > 0 {
			t.Capped = true
		}
		return nd
	}
	for _, r := range roots {
		if budget <= 0 && !(!full && expand(r)) {
			t.Capped = true
			continue
		}
		t.Roots = append(t.Roots, build(r, 1))
	}
	return t
}

// collapse folds a run of identical siblings into one node. Their descendants
// are not rendered (Hidden on the parent counts them): a run is "×37 SELECT
// jobs", its children would be 37 copies of the same thing.
func collapse(ms []*Span, first time.Time, budget, shown *int) *TreeNode {
	ds := make([]time.Duration, len(ms))
	for i, m := range ms {
		ds[i] = time.Duration(m.DurationNs)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	pick := func(q float64) time.Duration { return ds[min(int(q*float64(len(ds))), len(ds)-1)] }
	*budget--
	*shown++
	return &TreeNode{
		Span: *ms[0], Count: len(ms), Min: ds[0], Max: ds[len(ds)-1], P50: pick(0.5), P95: pick(0.95),
		Offset: ms[0].Start.Sub(first),
	}
}

func isErrorSpan(s *Span) bool {
	switch s.StatusCode {
	case "error":
		return true
	}
	for _, e := range s.Events {
		if e.Name == "exception" {
			return true
		}
	}
	return false
}

// ErrorSpans are the failing spans of a trace, earliest first.
func ErrorSpans(spans []Span) []Span {
	var out []Span
	for _, s := range spans {
		if isErrorSpan(&s) {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}
