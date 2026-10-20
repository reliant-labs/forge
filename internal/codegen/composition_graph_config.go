// File: internal/codegen/composition_graph_config.go
//
// Per-root config, derived from reach (docs/design/composition-graph.md §2
// P2, §9.4; composition graph PR 6). A root's process reads exactly:
//
//   - the PROCESS fields — what runRoot (cmd/<bin>/cmd/serve.go) projects onto
//     serverkit.Config for every root: address, TLS, logging, telemetry,
//     payload caps, the drain budget (processConfigFields);
//   - the DATABASE fields, when the root reaches the database (the same fact
//     that makes it migrate): the URL, the pool tuning, auto-migrate
//     (databaseConfigFields);
//   - every field a node in its closure reads through compose.go's value
//     expressions: `g.Cfg.X` / `g.Cfg.GetX()` is field X (a whole config
//     block when X is one), `g.Cfg.X.Y` the block leaf Y, and `g.Cfg` handed
//     on whole is every field — the honest answer until the component takes
//     a config block of its own (K3).
//
// The set is projected twice: into cmd/<bin>/cmd/roots_gen.go, where
// runRoot's config.LoadReach demands only those fields, and into
// deploy/kcl/config_gen.k's ROOTS, which gives a workload bound to the root
// exactly that env and those secrets.

package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

// processConfigFields are the root config fields runRoot reads for every
// root, by Go name (the serve template's `cfg.<X>` reads, minus the
// database ones). TestProcessConfigFieldsAreWhatRunRootReads pins the pair
// to the template.
var processConfigFields = []string{
	"Port", "OtlpEndpoint", "PprofAddr", "TlsCertPath", "TlsKeyPath", "LogFormat", "LogLevel",
	"Environment", "ReadMaxBytes", "SendMaxBytes", "SecurityHeadersEnabled", "CorsAllowCredentials",
	"CorsOrigins", "PreStopDelay", "ShutdownTimeout",
}

// databaseConfigFields are the fields a root reaches when it reaches the
// database.
var databaseConfigFields = []string{
	"AutoMigrate", "DatabaseUrl", "DbMaxOpenConns", "DbMaxIdleConns", "DbConnMaxIdleTime",
	"DbConnMaxLifetime", "DbConnectTimeout",
}

// ConfigLeaf is one config field a root reaches.
type ConfigLeaf struct {
	// Path names it from the root message: "database_url", "stripe.secret_key".
	Path      string
	EnvVar    string
	Sensitive bool
	Required  bool
}

// GraphRoot is one root as a process: the command that runs it and the
// config it reaches.
type GraphRoot struct {
	Name string
	// Args is the command path that runs it (`<bin> <args>`).
	Args []string
	// Config is every config field the root reaches; nil when the project
	// has no root config message to read reach against.
	Config []ConfigLeaf
}

// configModel maps the root config message's top-level Go field names to the
// leaves each one is.
type configModel struct {
	all    []ConfigLeaf
	byGo   map[string][]ConfigLeaf            // top-level field → its leaves (a block: all of them)
	blocks map[string]map[string][]ConfigLeaf // block field → leaf Go name → leaf
}

// loadConfigModel reads proto/config; nil when the project has no root
// config message (no proto/config, or only per-binary configs).
func loadConfigModel(projectDir string) *configModel {
	messages, err := ParseConfigProtosFromDir(filepath.Join(projectDir, "proto", "config"))
	if err != nil || len(messages) == 0 {
		return nil
	}
	root := BuildConfigTemplateData(messages).RootConfigMessage
	if root == "" {
		return nil
	}
	byName := map[string]*ConfigMessage{}
	for i := range messages {
		byName[messages[i].Name] = &messages[i]
	}
	m := &configModel{byGo: map[string][]ConfigLeaf{}, blocks: map[string]map[string][]ConfigLeaf{}}
	leaf := func(f ConfigField, prefix string) ConfigLeaf {
		return ConfigLeaf{Path: prefix + f.Name, EnvVar: f.EnvVar, Sensitive: f.Sensitive, Required: f.Required}
	}
	for _, f := range byName[root].Fields {
		if bm := byName[f.MessageType]; f.MessageType != "" && bm != nil && f.EnvVar == "" && f.Flag == "" {
			m.blocks[f.GoName] = map[string][]ConfigLeaf{}
			for _, bf := range bm.Fields {
				if bf.MessageType != "" && bf.EnvVar == "" && bf.Flag == "" {
					continue // one nesting level, as elsewhere
				}
				l := leaf(bf, f.Name+".")
				m.blocks[f.GoName][bf.GoName] = []ConfigLeaf{l}
				m.byGo[f.GoName] = append(m.byGo[f.GoName], l)
				m.all = append(m.all, l)
			}
			continue
		}
		l := leaf(f, "")
		m.byGo[f.GoName] = []ConfigLeaf{l}
		m.all = append(m.all, l)
	}
	return m
}

// configRead is what one node's constructor method reads of g.Cfg.
type configRead struct {
	whole  bool
	fields map[string]bool    // top-level Go name
	leaves map[[2]string]bool // block Go name, leaf Go name
}

// graphComposeConfigReads reads each node's g.Cfg reads off compose.go.
func graphComposeConfigReads(src string, nodes []*graphNode) map[string]*configRead {
	out := map[string]*configRead{}
	file, err := parser.ParseFile(token.NewFileSet(), "compose.go", src, parser.SkipObjectResolution)
	if err != nil {
		return out
	}
	isNode := map[string]bool{}
	for _, n := range nodes {
		isNode[n.Name] = true
	}
	for name, fd := range graphComposeMethods(file) {
		x, ok := strings.CutPrefix(name, "new")
		if !ok || !isNode[x] || len(fd.Recv.List[0].Names) != 1 {
			continue
		}
		recv := fd.Recv.List[0].Names[0].Name
		r := &configRead{fields: map[string]bool{}, leaves: map[[2]string]bool{}}
		var stack []ast.Node
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cfg" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
					r.read(cfgSelectorPath(sel, stack))
				}
			}
			stack = append(stack, n)
			return true
		})
		out[x] = r
	}
	return out
}

// cfgSelectorPath climbs from g.Cfg through the selectors (and Get<X>()
// calls) applied to it: g.Cfg.Stripe.GetSecretKey() is [Stripe SecretKey].
func cfgSelectorPath(base ast.Expr, stack []ast.Node) []string {
	var path []string
	cur := base
	for i := len(stack) - 1; i >= 0; i-- {
		switch p := stack[i].(type) {
		case *ast.SelectorExpr:
			if p.X != cur {
				return path
			}
			path = append(path, p.Sel.Name)
			cur = p
		case *ast.CallExpr:
			if p.Fun != cur || len(path) == 0 {
				return path
			}
			last := path[len(path)-1]
			if !strings.HasPrefix(last, "Get") {
				return path
			}
			path[len(path)-1] = strings.TrimPrefix(last, "Get")
			cur = p
		case *ast.ParenExpr:
			cur = p
		default:
			return path
		}
	}
	return path
}

func (r *configRead) read(path []string) {
	switch len(path) {
	case 0:
		r.whole = true
	case 1:
		r.fields[path[0]] = true
	default:
		r.leaves[[2]string{path[0], path[1]}] = true
	}
}

// rootConfig is the config one root reaches.
func (m *configModel) rootConfig(v graphRootView, reads map[string]*configRead) []ConfigLeaf {
	reached := map[string]ConfigLeaf{}
	add := func(ls []ConfigLeaf) {
		for _, l := range ls {
			reached[l.Path] = l
		}
	}
	addGo := func(name string) bool {
		ls, ok := m.byGo[name]
		add(ls)
		return ok
	}
	for _, f := range processConfigFields {
		addGo(f)
	}
	if v.Migrate {
		for _, f := range databaseConfigFields {
			addGo(f)
		}
	}
	for _, node := range v.Reach {
		r := reads[node]
		if r == nil {
			continue
		}
		if r.whole {
			add(m.all)
			break
		}
		for f := range r.fields {
			if !addGo(f) {
				// A method of the config (ProtoReflect, String, …): the
				// whole of it.
				add(m.all)
			}
		}
		for bl := range r.leaves {
			if ls, ok := m.blocks[bl[0]][bl[1]]; ok {
				add(ls)
			} else {
				addGo(bl[0])
			}
		}
	}
	out := make([]ConfigLeaf, 0, len(reached))
	for _, l := range m.all { // declaration order
		if _, ok := reached[l.Path]; ok {
			out = append(out, l)
		}
	}
	return out
}

// markRootConfig sets each root's config reach (nil without a config model).
func markRootConfig(views []graphRootView, m *configModel, reads map[string]*configRead) {
	if m == nil {
		return
	}
	for i := range views {
		views[i].Config = m.rootConfig(views[i], reads)
		views[i].HasConfig = true
	}
}

// graphRoots is the roots as processes, sorted by name.
func graphRoots(views []graphRootView) []GraphRoot {
	out := make([]GraphRoot, 0, len(views))
	for _, v := range views {
		r := GraphRoot{Name: v.Name, Args: append([]string(nil), v.Command...)}
		if v.HasConfig {
			r.Config = append([]ConfigLeaf{}, v.Config...)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GraphRoots assembles a graph-mode project's graph and returns its roots as
// processes, writing nothing; nil for a legacy-composition project.
func GraphRoots(projectDir string) ([]GraphRoot, error) {
	if !GraphComposition(projectDir) {
		return nil, nil
	}
	in, err := graphLintInput(projectDir)
	if err != nil {
		return nil, err
	}
	m, err := assembleGraph(in)
	if err != nil {
		return nil, err
	}
	return graphRoots(m.rootViews), nil
}
