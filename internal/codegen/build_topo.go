// File: internal/codegen/build_topo.go
//
// Type-topological ordering over component Deps structs — the reusable
// core of the owned composition-root model (FORGE_SHAPE_REDESIGN §2).
//
// The owned internal/app/build.go is a typed composition root: it must
// construct each registered component via its New(Deps) AFTER every
// collaborator that component depends on by TYPE has been constructed.
// "By TYPE, not by name" is the whole point — billing.Deps.Users typed
// user.Service means construct user before billing, regardless of the
// field's name. This file computes that order.
//
// Conventional deps (Logger / Config / Authorizer / DB / Repo) come from
// the shared Infra value, never another component, so they NEVER
// participate in a topo edge. Only a Deps field whose declared type
// resolves to another registered component's Service interface produces
// an edge consumer -> producer.
//
// Cycle handling is deliberately LOUD (spec: "detect a cycle and emit a
// clearly-commented post-construction setter stub the user completes — do
// NOT silently break"). Kahn's algorithm yields the order; any nodes that
// never reach in-degree zero are a residual cycle, reported with the
// back-edges so build.go can emit a `// forge:build-twophase` block and
// `forge map`/`audit` can flag it as a guardrail finding.
//
// This is PURE over its inputs (the parsed component set + a type
// resolver) so it is unit-testable without a real project on disk.

package codegen

import "sort"

// BuildComponent is one node in the construction graph: a registered
// service / worker / operator, its parsed Deps fields, and the metadata
// the build.go scaffold needs to emit its constructor call.
type BuildComponent struct {
	// Name is the runtime kebab name (display / inventory selection only —
	// never a construction key). Used for stable sort + diagnostics.
	Name string
	// FieldName is the exported Go field name on the *Services struct
	// (e.g. "Billing", "SvcBilling"). The collision-aware name shared
	// with the inventory.
	FieldName string
	// VarName is the lower-camel local variable name build.go binds the
	// constructed instance to (e.g. "billing").
	VarName string
	// Alias is the import alias for the component's package.
	Alias string
	// ImportPath is the module-relative import path
	// (e.g. "internal/handlers/billing").
	ImportPath string
	// ServiceTypeKey is the type-identity key this component PRODUCES —
	// the thing a consumer's Deps field type is matched against to draw an
	// edge. Conventionally the package-qualified Service interface
	// (e.g. "user.Service"). Empty for components that expose no
	// collaborator interface (pure leaf workers).
	ServiceTypeKey string
	// Deps are the parsed Deps fields, in declaration order.
	Deps []DepsField
}

// BuildEdge records a resolved consumer -> producer dependency: the
// consumer's Deps field Field (typed Type) was matched to the producer
// component. Used both to order construction and to render each
// constructor call's collaborator assignments.
type BuildEdge struct {
	Consumer string // consumer FieldName
	Producer string // producer FieldName
	Field    string // the Deps field on the consumer that carries the dep
	Type     string // the declared field type (for comments / setter stubs)
}

// BuildPlan is the result of ordering the construction graph.
type BuildPlan struct {
	// Order is the topo-sorted construction order (producers first). When
	// a cycle exists, Order contains the acyclic prefix and Cycles names
	// the components that could not be ordered.
	Order []BuildComponent
	// Edges are every resolved consumer->producer collaborator edge,
	// including back-edges that participate in a cycle (see CycleEdges).
	Edges []BuildEdge
	// Cycles lists the FieldNames of components left unordered because
	// they sit on a dependency cycle. Empty for a clean DAG.
	Cycles []string
	// CycleEdges are the specific edges build.go must break with a
	// two-phase setter stub: each is a consumer->producer edge where both
	// endpoints are in Cycles. The user completes the setter after both
	// instances exist.
	CycleEdges []BuildEdge
}

// HasCycle reports whether the plan contains an unresolved dependency
// cycle. forge map / audit use this as a guardrail signal.
func (p BuildPlan) HasCycle() bool { return len(p.Cycles) > 0 }

// reachesSelf reports whether start can reach itself by following
// producer edges restricted to the within set (the unplaced nodes). A
// node that can reach itself sits on a genuine dependency cycle; a node
// that is merely downstream of a cycle cannot. DFS from start over the
// consumer->producer relation.
func reachesSelf(start string, producers map[string]map[string]bool, within map[string]bool) bool {
	seen := map[string]bool{}
	var dfs func(n string) bool
	dfs = func(n string) bool {
		for p := range producers[n] {
			if !within[p] {
				continue
			}
			if p == start {
				return true
			}
			if !seen[p] {
				seen[p] = true
				if dfs(p) {
					return true
				}
			}
		}
		return false
	}
	return dfs(start)
}

// TypeResolver maps a Deps field's declared type to the FieldName of the
// component that PRODUCES that type, or "" when no registered component
// produces it (the field is a conventional dep filled from Infra, or an
// external collaborator). Implementations resolve by TYPE — see
// depsTypeResolver for the production matcher and the test helpers for
// in-memory variants.
type TypeResolver interface {
	// Resolve returns the producing component's FieldName for a Deps
	// field of the given declared type, or "" if none.
	Resolve(depsType string) string
}

// ServiceKeyResolver is the default TypeResolver: it resolves a Deps
// field type against the set of component-exposed service type keys
// (each producer's ServiceTypeKey, e.g. "user.Service"). The match is on
// the pretty-printed type string the Deps parser already produces,
// tolerating a leading pointer (`*user.Service`) since a consumer may
// hold either the interface value or a pointer to it.
//
// This is intentionally string-structural rather than go/types-based:
// it is pure, deterministic, and cheap, and the package-qualified
// Service interface name is unambiguous across a project (one `Service`
// per component package by the strict-contract-names convention). When a
// project genuinely needs assignability-by-implementation (a narrow
// collaborator interface satisfied by another component's Service), that
// is surfaced as an unresolved dep + TODO in build.go rather than guessed
// — matching the fail-loud stance of the wire matcher.
type ServiceKeyResolver struct {
	byKey map[string]string // ServiceTypeKey -> producer FieldName
}

// NewServiceKeyResolver indexes comps by their ServiceTypeKey. Components
// with an empty ServiceTypeKey produce nothing and are skipped.
func NewServiceKeyResolver(comps []BuildComponent) *ServiceKeyResolver {
	byKey := make(map[string]string, len(comps))
	for _, c := range comps {
		if c.ServiceTypeKey == "" {
			continue
		}
		byKey[c.ServiceTypeKey] = c.FieldName
	}
	return &ServiceKeyResolver{byKey: byKey}
}

// Resolve matches a Deps field type to a producing component FieldName.
func (r *ServiceKeyResolver) Resolve(depsType string) string {
	t := depsType
	for len(t) > 0 && t[0] == '*' {
		t = t[1:]
	}
	if f, ok := r.byKey[t]; ok {
		return f
	}
	return ""
}

// ComputeBuildPlan orders comps so every component is constructed after
// the components it depends on by type. resolver decides, per Deps field,
// whether the field's type is produced by another registered component
// (an edge) or is a conventional/external dep (no edge).
//
// Determinism: comps are processed in a stable order (by FieldName) and
// Kahn's queue is kept sorted, so the emitted build.go is byte-stable
// across regenerates regardless of map iteration order upstream.
func ComputeBuildPlan(comps []BuildComponent, resolver TypeResolver) BuildPlan {
	// Stable input order.
	sorted := make([]BuildComponent, len(comps))
	copy(sorted, comps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].FieldName < sorted[j].FieldName })

	byField := make(map[string]BuildComponent, len(sorted))
	for _, c := range sorted {
		byField[c.FieldName] = c
	}

	// Build edges. An edge consumer->producer means producer must be
	// constructed first; in Kahn terms the consumer has an in-edge.
	var edges []BuildEdge
	// producers[consumer] = set of distinct producer FieldNames it needs.
	producers := make(map[string]map[string]bool, len(sorted))
	// consumers[producer] = consumers that depend on it (for queue drain).
	consumers := make(map[string][]string, len(sorted))
	for _, c := range sorted {
		producers[c.FieldName] = map[string]bool{}
	}

	for _, c := range sorted {
		for _, df := range c.Deps {
			prod := resolver.Resolve(df.Type)
			if prod == "" || prod == c.FieldName {
				// No registered producer (conventional/external dep) or a
				// self-reference (degenerate) — not an ordering edge.
				continue
			}
			if _, known := byField[prod]; !known {
				continue
			}
			edges = append(edges, BuildEdge{
				Consumer: c.FieldName,
				Producer: prod,
				Field:    df.Name,
				Type:     df.Type,
			})
			if !producers[c.FieldName][prod] {
				producers[c.FieldName][prod] = true
				consumers[prod] = append(consumers[prod], c.FieldName)
			}
		}
	}

	// In-degree = number of distinct producers a consumer still waits on.
	indeg := make(map[string]int, len(sorted))
	for f, ps := range producers {
		indeg[f] = len(ps)
	}

	// Kahn with a sorted ready-queue for determinism.
	var ready []string
	for _, c := range sorted {
		if indeg[c.FieldName] == 0 {
			ready = append(ready, c.FieldName)
		}
	}
	sort.Strings(ready)

	var order []BuildComponent
	placed := map[string]bool{}
	for len(ready) > 0 {
		f := ready[0]
		ready = ready[1:]
		order = append(order, byField[f])
		placed[f] = true
		var newlyReady []string
		for _, consumer := range consumers[f] {
			indeg[consumer]--
			if indeg[consumer] == 0 {
				newlyReady = append(newlyReady, consumer)
			}
		}
		if len(newlyReady) > 0 {
			ready = append(ready, newlyReady...)
			sort.Strings(ready)
		}
	}

	plan := BuildPlan{Order: order, Edges: edges}

	// Residual cycle: anything not placed sits on a cycle. Append the
	// unplaced nodes (sorted) so build.go still constructs them — with the
	// back-edge field zeroed and a two-phase setter stub — rather than
	// dropping them.
	if len(order) < len(sorted) {
		var unplaced []string
		for _, c := range sorted {
			if !placed[c.FieldName] {
				unplaced = append(unplaced, c.FieldName)
			}
		}
		sort.Strings(unplaced)
		plan.Cycles = unplaced
		for _, c := range unplaced {
			plan.Order = append(plan.Order, byField[c])
		}

		// A node merely DOWNSTREAM of a cycle (e.g. Report -> User where
		// User<->Billing cycle) is also unplaceable by Kahn, but it sits on
		// no cycle of its own and needs no two-phase setter. Distinguish
		// TRUE cycle members — those that are mutually reachable through the
		// producer graph (an SCC of size > 1, or a self-loop) — from the
		// transitively-blocked tail. Only edges between true cycle members
		// become CycleEdges (the setters build.go must emit).
		unplacedSet := map[string]bool{}
		for _, f := range unplaced {
			unplacedSet[f] = true
		}
		// onCycle[f] iff f can reach itself following producer edges
		// restricted to unplaced nodes.
		onCycle := map[string]bool{}
		for _, start := range unplaced {
			if reachesSelf(start, producers, unplacedSet) {
				onCycle[start] = true
			}
		}
		for _, e := range edges {
			if onCycle[e.Consumer] && onCycle[e.Producer] {
				plan.CycleEdges = append(plan.CycleEdges, e)
			}
		}
		sort.Slice(plan.CycleEdges, func(i, j int) bool {
			if plan.CycleEdges[i].Consumer != plan.CycleEdges[j].Consumer {
				return plan.CycleEdges[i].Consumer < plan.CycleEdges[j].Consumer
			}
			return plan.CycleEdges[i].Field < plan.CycleEdges[j].Field
		})
	}

	return plan
}
