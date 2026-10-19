// File: internal/codegen/composition_graph_emit.go
//
// GenerateGraph: discover the nodes and roots, reconcile the owned compose.go,
// check the graph for cycles, and emit the forge-owned graph_gen.go (the
// accessors, roots, Build/Run) and routes_gen.go (the per-audience route
// sets). Every shape error — a missing provider, an ambiguous match, a cycle,
// a root field nothing produces — fails generate with the node, field and
// type named, before anything is written.

package codegen

import (
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/templates"
)

// GraphGenInput is GenerateGraph's input: the discovered component set.
type GraphGenInput struct {
	GenContext
	Services        []ServiceDef
	Packages        []BootstrapPackageData
	Workers         []BootstrapWorkerData
	Operators       []BootstrapOperatorData
	WebhookServices map[string]bool
	// Warnings receives advisory lines; nil is os.Stderr.
	Warnings io.Writer
}

func (in GraphGenInput) warnings() io.Writer {
	if in.Warnings != nil {
		return in.Warnings
	}
	return os.Stderr
}

// graphImport is one import line of a generated file.
type graphImport struct{ Alias, Path string }

// graphNodeView is a node as graph_gen.go.tmpl renders it.
type graphNodeView struct {
	Name, LogName, Type, Doc string
	HasLifecycle             bool
	LifecycleDoc             string
	NodeFields               string
	NilGuard                 bool
}

type graphRootFieldView struct{ Name, Type, Accessor string }

type graphRootView struct {
	Name, TypeName string
	Generated      bool
	Fields         []graphRootFieldView
	ForceOrder     []graphRootFieldView
	// Lazy are the nodes the closure reaches only through a lazy edge (a
	// method value), forced after the fields so they are built at boot.
	Lazy []string
	Plan string
}

type graphOperatorView struct{ Field, Type, Alias, Label string }

type graphOperatorsView struct {
	LeaderElectionID string
	Ops              []graphOperatorView
}

type graphGenView struct {
	Module    string
	Imports   []graphImport
	Nodes     []graphNodeView
	Roots     []graphRootView
	Operators *graphOperatorsView
}

type routesServiceView struct {
	InventoryServiceData
	Type string
}

type routesAudienceView struct {
	Name, Audience string
	Default        bool
	Services       []routesServiceView
}

type routesGenView struct {
	Module         string
	ConnectImports []string
	Imports        []graphImport
	Services       []routesServiceView
	Audiences      []routesAudienceView
}

// GenerateGraph emits the composition graph for a project in graph mode
// (GraphComposition): compose.go reconciled, graph_gen.go and routes_gen.go
// regenerated.
func GenerateGraph(in GraphGenInput) error {
	appDir := filepath.Join(in.ProjectDir, "internal", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}
	inject := InjectGenInput{GenContext: in.GenContext, Services: in.Services, Packages: in.Packages, Workers: in.Workers, Operators: in.Operators, Graph: true}
	comps, err := assembleBuildComponents(inject)
	if err != nil {
		return err
	}
	comps, notComponents := composedComponents(in.ProjectDir, comps)
	if consumed := notAComponentConsumers(comps, notComponents); len(consumed) > 0 {
		return notAComponentConsumedError(in.ProjectDir, consumed)
	}
	if bad := unconstructibleComponents(in.ProjectDir, comps); len(bad) > 0 {
		return unconstructibleError(bad)
	}
	providers, roots, err := discoverAppDecls(in.ProjectDir)
	if err != nil {
		return err
	}

	// Nodes: components, providers, then the generated ones.
	var nodes []*graphNode
	byName := map[string]*graphNode{}
	add := func(n *graphNode) error {
		if prev, dup := byName[n.Name]; dup {
			return fmt.Errorf("the composition graph has two nodes named %s (%s and %s): every node needs its own accessor g.%s() — rename the provider func", n.Name, prev.Doc, n.Doc, n.Name)
		}
		if reservedGraphNames[n.Name] {
			return fmt.Errorf("a node named %s (%s) would collide with app.Graph's own %s — rename it", n.Name, n.Doc, n.Name)
		}
		byName[n.Name] = n
		nodes = append(nodes, n)
		return nil
	}
	compNode := map[string]*graphNode{} // FieldName → node
	for i := range comps {
		c := &comps[i]
		n := componentNode(in.ModulePath, c)
		compNode[c.FieldName] = n
		if err := add(n); err != nil {
			return err
		}
	}
	for _, p := range providers {
		typeImports, unknown := typeExprImports(p.Result, p.Imports)
		if len(unknown) > 0 {
			return fmt.Errorf("%s: New%s returns %s, whose package %q the file does not import", p.File, p.Name, p.Result, unknown[0])
		}
		n := &graphNode{
			Name: p.Name, LogName: logName(p.Name), Kind: nodeProvider,
			Doc:  fmt.Sprintf("the %s provider (New%s, %s)", p.Name, p.Name, p.File),
			Type: p.Result, Imports: typeImports,
			Ctor: "New" + p.Name, DepsType: p.Name + "Deps", Fallible: p.Fallible,
			Deps: p.Deps, DepsKeys: p.DepsKeys, DepsFound: true,
			SrcImports: p.Imports, Pos: p.Pos,
		}
		if err := add(n); err != nil {
			return err
		}
	}

	rows, connectImports, err := inventoryRows(InventoryGenInput{GenContext: in.GenContext, Services: in.Services, Packages: in.Packages, Workers: in.Workers, Operators: in.Operators, WebhookServices: in.WebhookServices})
	if err != nil {
		return err
	}
	routesView := routesGenView{Module: in.ModulePath, ConnectImports: connectImports}
	routesImports := map[string]string{}
	byAudience := map[string][]routesServiceView{"user": nil}
	for _, row := range rows {
		cn := compNode[row.FieldName]
		if cn == nil {
			continue // a not-a-component handler: nothing to mount
		}
		sv := routesServiceView{InventoryServiceData: row, Type: cn.Type}
		routesView.Services = append(routesView.Services, sv)
		byAudience[row.Audience] = append(byAudience[row.Audience], sv)
		for a, p := range cn.Imports {
			routesImports[a] = p
		}
	}
	audiences := make([]string, 0, len(byAudience))
	for a := range byAudience {
		audiences = append(audiences, a)
	}
	sort.Slice(audiences, func(i, j int) bool {
		if (audiences[i] == "user") != (audiences[j] == "user") {
			return audiences[i] == "user"
		}
		return audiences[i] < audiences[j]
	})
	for _, a := range audiences {
		name := audienceRoutesName(a)
		av := routesAudienceView{Name: name, Audience: a, Default: a == "user", Services: byAudience[a]}
		routesView.Audiences = append(routesView.Audiences, av)
		n := &graphNode{
			Name: name, LogName: logName(name), Kind: nodeRoutes, Audience: a,
			Doc:  fmt.Sprintf("the %s-audience route set (routes_gen.go)", a),
			Type: "*" + name, Ctor: "New" + name, DepsType: name + "Deps", Fallible: true,
			DepsKeys: map[string]bool{"Logger": true, "Options": true}, DepsFound: true,
			SrcImports: routesImports,
		}
		for _, sv := range av.Services {
			n.Deps = append(n.Deps, DepsField{Name: sv.FieldName, Type: sv.Type})
			n.DepsKeys[sv.FieldName] = true
			n.Services = append(n.Services, compNode[sv.FieldName])
		}
		n.Deps = append(n.Deps, DepsField{Name: "Logger", Type: "*slog.Logger"}, DepsField{Name: "Options", Type: "MountOptions"})
		if err := add(n); err != nil {
			return err
		}
	}
	routesView.Imports = sortedImports(routesImports, "fmt", "log/slog", "net/http", "strings", "connectrpc.com/connect",
		"github.com/reliant-labs/forge/pkg/middleware", "github.com/reliant-labs/forge/pkg/mountkit",
		"github.com/reliant-labs/forge/pkg/mountkit/inventory", in.ModulePath+"/pkg/middleware")

	var opsView *graphOperatorsView
	var opNodes []*graphNode
	for _, n := range nodes {
		if n.Role == "internal/operators" {
			opNodes = append(opNodes, n)
		}
	}
	if len(opNodes) > 0 {
		opsView = &graphOperatorsView{LeaderElectionID: leaderElectionID(in.ModulePath)}
		n := &graphNode{
			Name: "Operators", LogName: "operators", Kind: nodeOperators,
			Doc:  "the controller manager running every operator (graph_gen.go)",
			Type: "*Operators", Ctor: "NewOperators", DepsType: "OperatorsDeps", Fallible: true,
			DepsKeys: map[string]bool{"Logger": true, "ServerCfg": true}, DepsFound: true,
			Lifecycle: lifecycle{Run: "Run"}, NilGuard: true,
			SrcImports: map[string]string{},
		}
		for _, op := range opNodes {
			n.Deps = append(n.Deps, DepsField{Name: op.Name, Type: op.Type})
			n.DepsKeys[op.Name] = true
			for a, p := range op.Imports {
				n.SrcImports[a] = p
			}
			opsView.Ops = append(opsView.Ops, graphOperatorView{Field: op.Name, Type: op.Type, Alias: op.Comp.Alias, Label: op.Comp.Name})
		}
		n.Deps = append(n.Deps, DepsField{Name: "Logger", Type: "*slog.Logger"}, DepsField{Name: "ServerCfg", Type: "serverkit.Config"})
		if err := add(n); err != nil {
			return err
		}
	}

	// Types: what each node produces, its Deps field types, its lifecycle.
	u := loadGraphUniverse(in.ProjectDir, comps)
	u.typeNodes(nodes)
	for _, n := range nodes {
		if n.Kind == nodeRoutes || n.Kind == nodeOperators {
			continue
		}
		if !n.NilGuard && (strings.HasPrefix(n.Type, "*") || n.Comp != nil && !strings.HasPrefix(n.Type, "*")) {
			// Untyped (mid-edit): a pointer, or a component's contract
			// interface, can be nil.
			n.NilGuard = true
		}
	}

	resolver := &graphResolver{
		module:         in.ModulePath,
		nodes:          nodes,
		byName:         byName,
		u:              u,
		blocks:         parseConfigBlocks(in.ProjectDir, in.ModulePath),
		configFields:   parseConfigFields(in.ProjectDir),
		storeAccessors: parseStoreAccessors(in.ProjectDir),
	}

	// compose.go: reconcile (or, when disowned, read as is).
	composeRel := filepath.Join("internal", "app", "compose.go")
	composeAbs := filepath.Join(in.ProjectDir, composeRel)
	existing, rerr := os.ReadFile(composeAbs)
	if rerr != nil && !os.IsNotExist(rerr) {
		return fmt.Errorf("read internal/app/compose.go: %w", rerr)
	}
	compose := string(existing)
	disowned := disownedFilePresent(in.Checksums, composeRel, composeAbs)
	if !disowned {
		out, problems, err := reconcileGraphCompose(compose, nodes, resolver)
		if err != nil {
			return err
		}
		if len(problems) > 0 {
			return graphProblemsError(problems)
		}
		compose = out
	}

	edges, lazy := graphComposeEdgesLazy(compose, nodes)
	if cycle := graphCycle(nodes, edges); cycle != nil {
		return graphCycleError(cycle)
	}
	for _, w := range staleGraphMethods(compose, byName) {
		fmt.Fprintf(in.warnings(), "  ⚠️  %s\n", w)
	}

	// Roots.
	if len(roots) == 0 {
		roots = []graphRoot{defaultGraphRoot(nodes, edges)}
	} else if w := serveRootWarning(in.ProjectDir, roots); w != "" {
		fmt.Fprintf(in.warnings(), "  ⚠️  %s\n", w)
	}
	var rootViews []graphRootView
	seenRoot := map[string]string{}
	for i := range roots {
		r := &roots[i]
		if prev, dup := seenRoot[r.Name]; dup {
			return fmt.Errorf("%s:%d: two roots are named %q (%s and %s)", r.File, r.Line, r.Name, prev, r.TypeName)
		}
		seenRoot[r.Name] = r.TypeName
		if !r.Generated {
			if err := resolveRootFields(r, nodes, u); err != nil {
				return err
			}
		}
		rootViews = append(rootViews, rootViewLazy(r, edges, lazy))
	}

	// graph_gen.go
	gv := graphGenView{Module: in.ModulePath, Roots: rootViews, Operators: opsView}
	nodeImports := map[string]string{}
	for _, n := range nodes {
		for a, p := range n.Imports {
			nodeImports[a] = p
		}
		gv.Nodes = append(gv.Nodes, nodeView(n))
	}
	if opsView != nil {
		for _, op := range opNodes {
			nodeImports[op.Comp.Alias] = in.ModulePath + "/" + op.Comp.ImportPath
		}
	}
	gv.Imports = sortedImports(nodeImports, "context", "fmt", "log/slog", "strings", "github.com/reliant-labs/forge/pkg/appkit/operatorkit",
		"github.com/reliant-labs/forge/pkg/graphkit", "github.com/reliant-labs/forge/pkg/lifecyclekit",
		"github.com/reliant-labs/forge/pkg/serverkit", in.ModulePath+"/pkg/config")

	graphSrc, err := renderGoTemplate("graph_gen.go.tmpl", gv)
	if err != nil {
		return err
	}
	routesSrc, err := renderGoTemplate("routes_gen.go.tmpl", routesView)
	if err != nil {
		return err
	}

	// Write: the generated pair, then the owned compose.go.
	RetireGenerated(in.ProjectDir, filepath.Join("internal", "app", "mounts_services_gen.go"), in.Checksums)
	if err := writeForgeOwned(in.ProjectDir, filepath.Join("internal", "app", "graph_gen.go"), graphSrc, in.Checksums); err != nil {
		return fmt.Errorf("write internal/app/graph_gen.go: %w", err)
	}
	if err := writeForgeOwned(in.ProjectDir, filepath.Join("internal", "app", "routes_gen.go"), routesSrc, in.Checksums); err != nil {
		return fmt.Errorf("write internal/app/routes_gen.go: %w", err)
	}
	if !disowned && compose != string(existing) {
		if err := writeUserScaffold(composeAbs, []byte(compose)); err != nil {
			return fmt.Errorf("write internal/app/compose.go: %w", err)
		}
	}
	return nil
}

// reservedGraphNames are app.Graph's own members, which no node may shadow.
var reservedGraphNames = map[string]bool{
	"Ctx": true, "Cfg": true, "Log": true, "ServerCfg": true, "Probes": true, "Built": true,
}

// componentNode is a discovered component as a graph node.
func componentNode(module string, c *BuildComponent) *graphNode {
	importPath := module + "/" + c.ImportPath
	typ := c.compFieldType
	if typ == "" {
		typ = "*" + c.Alias + ".Service"
	}
	ctor := c.compConstructor
	if ctor == "" {
		ctor = DefaultConstructorName
	}
	role := map[string]string{
		"internal/handlers":  "handler",
		"internal":           "package",
		"internal/workers":   "worker",
		"internal/operators": "operator",
	}[c.compRoleRoot]
	if role == "" {
		role = "component"
	}
	return &graphNode{
		Name: c.FieldName, LogName: logName(c.FieldName), Kind: nodeComponent,
		Doc:  fmt.Sprintf("the %s %s (%s)", c.Name, role, c.ImportPath),
		Type: typ, Imports: map[string]string{c.Alias: importPath},
		Ctor: c.Alias + "." + ctor, DepsType: c.Alias + ".Deps", DepsImport: importPath,
		Fallible: c.compFallible, Wrapped: c.compWrapped, MiddlewareCtor: c.compMiddlewareCtor,
		Deps: c.Deps, DepsKeys: c.DepsKeys, DepsFound: c.DepsFound,
		Comp: c, Role: c.compRoleRoot,
	}
}

// nodeView renders a node for graph_gen.go.
func nodeView(n *graphNode) graphNodeView {
	v := graphNodeView{Name: n.Name, LogName: n.LogName, Type: n.Type, Doc: n.Doc, NilGuard: n.NilGuard}
	l := n.Lifecycle
	if !l.any() {
		return v
	}
	v.HasLifecycle = true
	var fields, doc []string
	for _, f := range []struct{ phase, method string }{{"Run", l.Run}, {"Drain", l.Drain}, {"Close", l.Close}, {"Ready", l.Ready}} {
		if f.method == "" {
			continue
		}
		fields = append(fields, f.phase+": c."+f.method)
		if f.phase == f.method {
			doc = append(doc, f.phase)
		} else {
			doc = append(doc, f.phase+" ("+f.method+")")
		}
	}
	v.NodeFields = strings.Join(fields, ", ")
	v.LifecycleDoc = strings.Join(doc, ", ")
	return v
}

// defaultGraphRoot is the generated root when none is declared: every node
// with a Run method that no other node depends on, providers first (in
// declaration order), then workers, then the operators' manager.
func defaultGraphRoot(nodes []*graphNode, edges map[string][]string) graphRoot {
	consumed := map[string]bool{}
	for _, ds := range edges {
		for _, d := range ds {
			consumed[d] = true
		}
	}
	rank := func(n *graphNode) int {
		switch {
		case n.Kind == nodeProvider:
			return 0
		case n.Role == "internal/workers":
			return 2
		case n.Kind == nodeOperators:
			return 3
		}
		return 1
	}
	var sinks []*graphNode
	for _, n := range nodes {
		if n.Lifecycle.Run != "" && !consumed[n.Name] {
			sinks = append(sinks, n)
		}
	}
	sort.SliceStable(sinks, func(i, j int) bool {
		if rank(sinks[i]) != rank(sinks[j]) {
			return rank(sinks[i]) < rank(sinks[j])
		}
		if sinks[i].Kind == nodeProvider {
			return sinks[i].Pos < sinks[j].Pos
		}
		return sinks[i].Name < sinks[j].Name
	})
	r := graphRoot{Name: "server", TypeName: "Server", Generated: true}
	for _, n := range sinks {
		r.Fields = append(r.Fields, graphRootField{Name: n.Name, Type: n.Type, Node: n})
	}
	return r
}

// resolveRootFields resolves each field of a declared root to the node it
// holds, by type.
func resolveRootFields(r *graphRoot, nodes []*graphNode, u *graphUniverse) error {
	for i := range r.Fields {
		f := &r.Fields[i]
		cands := u.producersOf(u.rootFieldType(r.TypeName, f.Name), "", nodes)
		if len(cands) == 0 {
			for _, n := range nodes {
				if n.Type == f.Type {
					cands = append(cands, n)
				}
			}
		}
		switch len(cands) {
		case 1:
			f.Node = cands[0]
		case 0:
			return fmt.Errorf("%s:%d: root %s field %s (%s): no node produces that type — add a provider func `func New<X>(<X>Deps) (%s, error)` to internal/app, or type the field as a node's own type", r.File, r.Line, r.TypeName, f.Name, f.Type, f.Type)
		default:
			names := make([]string, len(cands))
			for j, c := range cands {
				names[j] = c.Name + " (" + c.Type + ")"
			}
			return fmt.Errorf("%s:%d: root %s field %s (%s) could be any of %s — type the field as exactly one node's type (instance identity is the Go type)", r.File, r.Line, r.TypeName, f.Name, f.Type, strings.Join(names, ", "))
		}
	}
	return nil
}

// rootView renders a root: its fields, the order Build forces them in, and
// the start order its closure gets, from compose.go's edges.
func rootView(r *graphRoot, edges map[string][]string) graphRootView {
	return rootViewLazy(r, edges, nil)
}

// rootViewLazy is rootView for a graph with lazy edges (a method value
// g.X in compose.go): Build also forces every node its closure reaches
// lazily, so a lazily-used node is constructed at boot, never on first use.
func rootViewLazy(r *graphRoot, edges, lazy map[string][]string) graphRootView {
	v := graphRootView{Name: r.Name, TypeName: r.TypeName, Generated: r.Generated}
	for _, f := range r.Fields {
		v.Fields = append(v.Fields, graphRootFieldView{Name: f.Name, Type: f.Type, Accessor: f.Node.Name})
	}
	for i := len(v.Fields) - 1; i >= 0; i-- {
		v.ForceOrder = append(v.ForceOrder, v.Fields[i])
	}
	// The same walk Build makes: fields last to first, each node's
	// dependencies (in compose.go's order) before it.
	var order []string
	done := map[string]bool{}
	logNameToNode := map[string]string{}
	var visit func(string)
	visit = func(n string) {
		if done[n] {
			return
		}
		done[n] = true
		for _, d := range edges[n] {
			visit(d)
		}
		logNameToNode[logName(n)] = n
		order = append(order, logName(n))
	}
	for _, f := range v.ForceOrder {
		visit(f.Accessor)
	}
	// Lazy targets of the closure, until nothing new is reached.
	for j := 0; j < len(order); j++ {
		for _, l := range lazy[logNameToNode[order[j]]] {
			if !done[l] {
				v.Lazy = append(v.Lazy, l)
				visit(l)
			}
		}
	}
	if len(order) == 0 {
		v.Plan = "(nothing)"
	} else {
		v.Plan = strings.Join(order, " → ")
	}
	return v
}

// graphCycleError names a dependency cycle by its edges.
func graphCycleError(cycle []string) error {
	var steps []string
	for i := 0; i+1 < len(cycle); i++ {
		steps = append(steps, fmt.Sprintf("new%s (compose.go) calls g.%s()", cycle[i], cycle[i+1]))
	}
	return fmt.Errorf("the composition graph has a dependency cycle: %s\n\n  %s\n\nA node cannot be constructed before itself. Break one edge by making it lazy: type that Deps field as a func returning the dependency (`Peer func() *peer.Service`). forge wires it to the accessor's method value — `Peer: g.Peer,`, no call — so it is resolved on first use, after both nodes exist, and draws no construction edge (an existing key is yours: change its value to the method value yourself)",
		strings.Join(cycle, " → "), strings.Join(steps, "\n  "))
}

// staleGraphMethods lists compose.go constructor methods for nodes the
// project no longer has.
func staleGraphMethods(compose string, byName map[string]*graphNode) []string {
	var out []string
	for _, x := range composeMethodNames(compose) {
		if _, ok := byName[x]; !ok {
			out = append(out, fmt.Sprintf("internal/app/compose.go: new%s constructs a node the project no longer has — delete the method", x))
		}
	}
	sort.Strings(out)
	return out
}

// renderGoTemplate renders a project template and gofmts it.
func renderGoTemplate(name string, data any) ([]byte, error) {
	src, err := templates.ProjectTemplates().Render(name, data)
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	out, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf("format %s (a forge bug — please report it): %w\n%s", name, err, src)
	}
	return out, nil
}

// sortedImports is m as import lines, minus the paths the template imports
// itself.
func sortedImports(m map[string]string, fixed ...string) []graphImport {
	skip := map[string]bool{}
	for _, p := range fixed {
		skip[p] = true
	}
	out := make([]graphImport, 0, len(m))
	for a, p := range m {
		if skip[p] {
			continue
		}
		out = append(out, graphImport{Alias: a, Path: p})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path || out[i].Path == out[j].Path && out[i].Alias < out[j].Alias
	})
	return out
}

// GenerateGraphProviders scaffolds a graph-mode project's owned
// internal/app/providers.go (the default providers: the database pool and
// ORM, the outbound HTTP client, the user audience's authenticator, chain and
// API listener, pprof) and auth.go, each exactly once.
func GenerateGraphProviders(modulePath, databaseDriver, projectDir string) error {
	hasDB := databaseDriver != ""
	if err := GenerateAuthSetupData(projectDir, AuthSetupTemplateData{Module: modulePath, Graph: true, HasDatabase: hasDB}); err != nil {
		return err
	}
	rel := filepath.Join("internal", "app", "providers.go")
	configFields := DefaultConfigFieldNames()
	if msgs, err := ParseConfigProtosFromDir(filepath.Join(projectDir, "proto", "config")); err == nil && len(msgs) > 0 {
		configFields = ConfigFieldNamesFromMessages(msgs)
	}
	data := struct {
		Module       string
		HasDatabase  bool
		RESTEnabled  bool
		ConfigFields map[string]bool
	}{Module: modulePath, HasDatabase: hasDB, RESTEnabled: projectAPIRESTEnabled(projectDir), ConfigFields: configFields}
	content, err := templates.ProjectTemplates().Render("providers-graph.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render providers-graph.go.tmpl: %w", err)
	}
	if _, err := writeForgeScaffoldOnce(projectDir, rel, content); err != nil {
		return fmt.Errorf("write internal/app/providers.go: %w", err)
	}
	return nil
}

// serveRootWarning explains the one build break declaring roots can cause:
// the scaffolded serve.go runs the default root (g.RunServer), which stops
// being generated once internal/app declares roots of its own.
func serveRootWarning(projectDir string, roots []graphRoot) string {
	for _, r := range roots {
		if r.TypeName == "Server" {
			return ""
		}
	}
	matches, _ := filepath.Glob(filepath.Join(projectDir, "cmd", "*", "cmd", "serve.go"))
	for _, m := range matches {
		src, err := os.ReadFile(m)
		if err != nil || !strings.Contains(string(src), ".RunServer(") {
			continue
		}
		rel, _ := filepath.Rel(projectDir, m)
		names := make([]string, len(roots))
		for i, r := range roots {
			names[i] = r.TypeName
		}
		return fmt.Sprintf("internal/app declares roots (%s), so the default Server root is no longer generated, but %s still calls g.RunServer: declare `//forge:root server` on a `type Server struct` in internal/app/roots.go (what `server` should run), or point %s at g.Run<Root>",
			strings.Join(names, ", "), rel, rel)
	}
	return ""
}
