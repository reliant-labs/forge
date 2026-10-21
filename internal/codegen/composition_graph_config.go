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
// A project whose primary binary binds configs per command
// ((forge.v1.binary_config) `command` / `subtrees`, #957) has more than one
// config a process loads. Each of the primary binary's configs other than the
// one config.Config aliases is a graph INPUT of its own, app.Graph.<Message>
// (graph_gen.go), and a root's process loads exactly the ones #957 says a
// process at its command path loads: every subtree config one of whose paths
// prefixes it, and the command config bound to that exact path. Reach is still
// PER FIELD: of each config the process loads, the root reaches the leaves its
// closure reads through `g.<Message>` (the same selector rules as g.Cfg), and
// nothing else. A loaded config bounds what a root MAY reach; the closure
// decides what it does. A closure that reads a config its root's process does
// not load is a generate error — that field would be nil at run time.
//
// The set is projected twice: into cmd/<bin>/cmd/roots_gen.go, where
// runRoot's config.LoadReach — and each other config's Load<Msg>Reach —
// demands only those fields, and into deploy/kcl/config_gen.k's ROOTS, which
// gives a workload bound to the root exactly that env and those secrets.

package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
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

// rootConfigInput is the graph input the root config is read through.
const rootConfigInput = "Cfg"

// ConfigLeaf is one config field a root reaches.
type ConfigLeaf struct {
	// Path names it from its config message: "database_url", "stripe.secret_key".
	Path      string
	EnvVar    string
	Sensitive bool
	Required  bool
}

// RootConfigLoad is one config beside the root config that a root's process
// loads: a subtree config over its command path, or the command config bound
// to it (or, beside a project-global AppConfig, the binary's own root
// config).
type RootConfigLoad struct {
	// Message is the config message — also the app.Graph field it is read
	// through, and the Load<Message>Reach that loads it.
	Message string
	// Config is every field of it the root reaches, by path from Message.
	// Empty (never nil): the process loads it and demands none of it.
	Config []ConfigLeaf
	// Read is true when a node in the root's closure reads it at all.
	Read bool
	// Command is the command path of a command config; Subtrees the paths
	// of a subtree config. Both empty: the binary's root config.
	Command  []string
	Subtrees [][]string
}

// GraphRoot is one root as a process: the command that runs it and the
// config it reaches.
type GraphRoot struct {
	Name string
	// Args is the command path that runs it (`<bin> <args>`).
	Args []string
	// Config is every root-config (config.Config) field the root reaches;
	// nil when the project has no root config message to read reach
	// against.
	Config []ConfigLeaf
	// Loads are the other configs the root's process loads, each with the
	// fields of it the root reaches, in declaration order.
	Loads []RootConfigLoad
}

// Leaves is every config field the root reaches, across every config its
// process loads: the root config's, then each load's.
func (r GraphRoot) Leaves() []ConfigLeaf {
	if r.Config == nil {
		return nil
	}
	out := append([]ConfigLeaf{}, r.Config...)
	for _, l := range r.Loads {
		out = append(out, l.Config...)
	}
	return out
}

// configModel maps one config message's top-level Go field names to the
// leaves each one is.
type configModel struct {
	all    []ConfigLeaf
	byGo   map[string][]ConfigLeaf            // top-level field → its leaves (a block: all of them)
	blocks map[string]map[string][]ConfigLeaf // block field → leaf Go name → leaf
}

// configInput is one of the primary binary's configs other than the root
// config: an app.Graph input of its own.
type configInput struct {
	msg   *ConfigMessage
	model *configModel
}

// loadedAt reports whether the process running the command at path loads
// the config — #957's rule, as the generated Load<Msg> checks it.
func (c configInput) loadedAt(path []string) bool {
	switch {
	case len(c.msg.Command) > 0:
		return slices.Equal(c.msg.Command, path)
	case len(c.msg.Subtrees) > 0:
		for _, p := range c.msg.Subtrees {
			if isPathPrefix(p, path) {
				return true
			}
		}
		return false
	}
	return true // the binary's root config: every process of it
}

// processConfigs is every config a graph-mode project's processes load: the
// root config (g.Cfg) and the primary binary's other configs, by graph input.
type processConfigs struct {
	binary string
	root   *configModel
	inputs []configInput
}

// inputNames is every graph input a node may read config through.
func (pc *processConfigs) inputNames() []string {
	names := []string{rootConfigInput}
	if pc == nil {
		return names
	}
	for _, in := range pc.inputs {
		names = append(names, in.msg.Name)
	}
	return names
}

// configInputs is the config inputs' message names; nil without configs.
func (pc *processConfigs) configInputs() map[string]bool {
	if pc == nil || len(pc.inputs) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, in := range pc.inputs {
		out[in.msg.Name] = true
	}
	return out
}

// inputViews renders the config inputs for graph_gen.go's Graph.
func (pc *processConfigs) inputViews() []graphConfigInputView {
	if pc == nil {
		return nil
	}
	var out []graphConfigInputView
	for _, in := range pc.inputs {
		out = append(out, graphConfigInputView{Name: in.msg.Name, Doc: in.scope(pc.binary)})
	}
	return out
}

// scope names the processes that load the config, for a comment.
func (c configInput) scope(binary string) string {
	switch {
	case len(c.msg.Command) > 0:
		return fmt.Sprintf("the `%s %s` command config", binary, strings.Join(c.msg.Command, " "))
	case len(c.msg.Subtrees) > 0:
		return fmt.Sprintf("the config of every %#q process at or below %s", binary, subtreeList(binary, c.msg.Subtrees))
	}
	return fmt.Sprintf("the %#q binary's root config", binary)
}

// loadProcessConfigs reads proto/config: the root config — the project's
// AppConfig, or else the primary binary's own root config (the message
// config.Config aliases, PrimaryConfigMessage) — and every other config the
// primary binary binds. nil when there is no root config.
//
// No config messages — no descriptor yet (a fresh scaffold before its first
// generate), or none annotated — means the scaffold defaults, exactly as the
// config loader and config_gen.k have it (generateConfigLoader,
// generatePerEnvDeployConfig): the graph models the config the process
// really loads. Reading it as "no root config" left every root's reach
// unknown, so config_gen.k carried no ROOTS and no workloadEnvMap while each
// env's main.k calls it, and no env rendered.
func loadProcessConfigs(projectDir string) *processConfigs {
	messages, err := ParseConfigProtosFromDir(filepath.Join(projectDir, "proto", "config"))
	if err != nil {
		return nil
	}
	if len(messages) == 0 {
		messages = DefaultConfigMessages()
	}
	binary := ""
	if treeDir := primaryCmdTreeDir(projectDir); treeDir != "" {
		binary = filepath.Base(filepath.Dir(treeDir))
	}
	return processConfigsFrom(messages, binary)
}

// processConfigsFrom is loadProcessConfigs over parsed messages, for the
// primary binary binary ("" when the project has none).
func processConfigsFrom(messages []ConfigMessage, binary string) *processConfigs {
	root := BuildConfigTemplateData(messages).RootConfigMessage
	if root == "" {
		for _, m := range messages {
			if binary != "" && m.Binary == binary && len(m.Command) == 0 && len(m.Subtrees) == 0 {
				root = m.Name
				break
			}
		}
	}
	if root == "" {
		return nil
	}
	byName := map[string]*ConfigMessage{}
	for i := range messages {
		byName[messages[i].Name] = &messages[i]
	}
	pc := &processConfigs{binary: binary, root: newConfigModel(byName, root)}
	for i := range messages {
		m := &messages[i]
		if binary == "" || m.Binary != binary || m.Name == root {
			continue
		}
		pc.inputs = append(pc.inputs, configInput{msg: m, model: newConfigModel(byName, m.Name)})
	}
	return pc
}

// newConfigModel models one config message: its own leaves, and the leaves
// of each block it composes (one nesting level, as elsewhere).
func newConfigModel(byName map[string]*ConfigMessage, name string) *configModel {
	m := &configModel{byGo: map[string][]ConfigLeaf{}, blocks: map[string]map[string][]ConfigLeaf{}}
	leaf := func(f ConfigField, prefix string) ConfigLeaf {
		return ConfigLeaf{Path: prefix + f.Name, EnvVar: f.EnvVar, Sensitive: f.Sensitive, Required: f.Required}
	}
	msg := byName[name]
	if msg == nil {
		return m
	}
	for _, f := range msg.Fields {
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

// configRead is what one node's constructor method reads of one config.
type configRead struct {
	whole  bool
	fields map[string]bool    // top-level Go name
	leaves map[[2]string]bool // block Go name, leaf Go name
}

// nodeConfigReads is what one node reads of each config, by graph input:
// "Cfg" for the root config, a config input's message name for the others.
type nodeConfigReads map[string]*configRead

// graphComposeConfigReads reads each node's g.Cfg reads off compose.go.
func graphComposeConfigReads(src string, nodes []*graphNode) map[string]*configRead {
	out := map[string]*configRead{}
	for node, reads := range graphComposeInputConfigReads(src, nodes, []string{rootConfigInput}) {
		if r := reads[rootConfigInput]; r != nil {
			out[node] = r
		}
	}
	return out
}

// graphComposeInputConfigReads reads, off compose.go, what each node's
// constructor method reads of each config input named in inputs
// (`g.<input>...`).
func graphComposeInputConfigReads(src string, nodes []*graphNode, inputs []string) map[string]nodeConfigReads {
	out := map[string]nodeConfigReads{}
	file, err := parser.ParseFile(token.NewFileSet(), "compose.go", src, parser.SkipObjectResolution)
	if err != nil {
		return out
	}
	isNode := map[string]bool{}
	for _, n := range nodes {
		isNode[n.Name] = true
	}
	isInput := map[string]bool{}
	for _, in := range inputs {
		isInput[in] = true
	}
	for name, fd := range graphComposeMethods(file) {
		x, ok := strings.CutPrefix(name, "new")
		if !ok || !isNode[x] || len(fd.Recv.List[0].Names) != 1 {
			continue
		}
		recv := fd.Recv.List[0].Names[0].Name
		reads := nodeConfigReads{}
		var stack []ast.Node
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if sel, ok := n.(*ast.SelectorExpr); ok && isInput[sel.Sel.Name] {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
					r := reads[sel.Sel.Name]
					if r == nil {
						r = &configRead{fields: map[string]bool{}, leaves: map[[2]string]bool{}}
						reads[sel.Sel.Name] = r
					}
					r.read(cfgSelectorPath(sel, stack))
				}
			}
			stack = append(stack, n)
			return true
		})
		if _, reads0 := reads[rootConfigInput]; !reads0 && isInput[rootConfigInput] {
			// Every node method is recorded, reading g.Cfg or not, as
			// before config inputs existed.
			reads[rootConfigInput] = &configRead{fields: map[string]bool{}, leaves: map[[2]string]bool{}}
		}
		out[x] = reads
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

// reached accumulates the leaves of m a set of reads names, by path.
type reached map[string]ConfigLeaf

func (m *configModel) addLeaves(into reached, ls []ConfigLeaf) {
	for _, l := range ls {
		into[l.Path] = l
	}
}

func (m *configModel) addGo(into reached, name string) bool {
	ls, ok := m.byGo[name]
	m.addLeaves(into, ls)
	return ok
}

// addRead adds what one node reads of m.
func (m *configModel) addRead(into reached, r *configRead) {
	if r == nil {
		return
	}
	if r.whole {
		m.addLeaves(into, m.all)
		return
	}
	for f := range r.fields {
		if !m.addGo(into, f) {
			// A method of the config (ProtoReflect, String, …): the
			// whole of it.
			m.addLeaves(into, m.all)
		}
	}
	for bl := range r.leaves {
		if ls, ok := m.blocks[bl[0]][bl[1]]; ok {
			m.addLeaves(into, ls)
		} else {
			m.addGo(into, bl[0])
		}
	}
}

// ordered is the reached leaves in declaration order, never nil.
func (m *configModel) ordered(in reached) []ConfigLeaf {
	out := make([]ConfigLeaf, 0, len(in))
	for _, l := range m.all {
		if _, ok := in[l.Path]; ok {
			out = append(out, l)
		}
	}
	return out
}

// rootConfig is the root config one root reaches.
func (m *configModel) rootConfig(v graphRootView, reads map[string]nodeConfigReads) []ConfigLeaf {
	got := reached{}
	for _, f := range processConfigFields {
		m.addGo(got, f)
	}
	if v.Migrate {
		for _, f := range databaseConfigFields {
			m.addGo(got, f)
		}
	}
	for _, node := range v.Reach {
		m.addRead(got, reads[node][rootConfigInput])
	}
	return m.ordered(got)
}

// markRootConfig sets each root's config reach (nil without a root config),
// refusing a closure that reads a config its root's process does not load,
// and a root whose loaded configs both declare one env var.
func markRootConfig(views []graphRootView, pc *processConfigs, reads map[string]*configRead) error {
	byNode := map[string]nodeConfigReads{}
	for node, r := range reads {
		byNode[node] = nodeConfigReads{rootConfigInput: r}
	}
	return markProcessConfig(views, pc, byNode)
}

// markProcessConfig is markRootConfig over every config input.
func markProcessConfig(views []graphRootView, pc *processConfigs, reads map[string]nodeConfigReads) error {
	if pc == nil {
		return nil
	}
	var problems []string
	for i := range views {
		v := &views[i]
		v.Config = pc.root.rootConfig(*v, reads)
		v.HasConfig = true
		v.Loads = nil
		for _, in := range pc.inputs {
			var readers []string
			got := reached{}
			for _, node := range v.Reach {
				if r := reads[node][in.msg.Name]; r != nil {
					readers = append(readers, node)
					in.model.addRead(got, r)
				}
			}
			if !in.loadedAt(v.Command) {
				for _, node := range readers {
					problems = append(problems, unloadedConfigProblem(pc.binary, *v, node, in.msg))
				}
				continue
			}
			v.Loads = append(v.Loads, RootConfigLoad{
				Message:  in.msg.Name,
				Config:   in.model.ordered(got),
				Read:     len(readers) > 0,
				Command:  in.msg.Command,
				Subtrees: in.msg.Subtrees,
			})
		}
		problems = append(problems, duplicateRootEnv(*v)...)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("a root reaches config its process cannot have:\n\n  - %s", strings.Join(problems, "\n  - "))
}

// unloadedConfigProblem explains a closure that reads a config the root's
// process never loads.
func unloadedConfigProblem(binary string, v graphRootView, node string, msg *ConfigMessage) string {
	process := strings.TrimSpace(binary + " " + strings.Join(v.Command, " "))
	return fmt.Sprintf(
		"root %q (%#q) reaches %s, whose constructor in internal/app/compose.go (new%s) reads g.%s — %s, which the %#q process never loads, so g.%s is nil there. Read the setting from a config that process loads (the binary's root config, a subtree config over %#q, or its own command config), or keep %s out of the %s root's closure.",
		v.Name, process, node, node, msg.Name, scopePhrase(msg), process, msg.Name, process, node, v.Name)
}

// duplicateRootEnv refuses an env var two configs one root loads both
// declare. ValidateBinaryConfigBindings refuses the pair first; this is the
// reach's own guard, so ROOTS never carries a key whose config is a guess.
func duplicateRootEnv(v graphRootView) []string {
	seen := map[string]string{}
	var out []string
	note := func(where string, ls []ConfigLeaf) {
		for _, l := range ls {
			if l.EnvVar == "" {
				continue
			}
			if prev, dup := seen[l.EnvVar]; dup && prev != where {
				out = append(out, fmt.Sprintf("root %q reaches env var %s in both %s and %s, which its process loads together: declare it once, on the config whose scope covers every process that reads it.", v.Name, l.EnvVar, prev, where))
				continue
			}
			seen[l.EnvVar] = where
		}
	}
	note("the root config", v.Config)
	for _, l := range v.Loads {
		note(l.Message, l.Config)
	}
	return out
}

// graphRoots is the roots as processes, sorted by name.
func graphRoots(views []graphRootView) []GraphRoot {
	out := make([]GraphRoot, 0, len(views))
	for _, v := range views {
		r := GraphRoot{Name: v.Name, Args: append([]string(nil), v.Command...)}
		if v.HasConfig {
			r.Config = append([]ConfigLeaf{}, v.Config...)
			r.Loads = append([]RootConfigLoad(nil), v.Loads...)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GraphRoots assembles a graph-mode project's graph and returns its roots as
// processes, writing nothing; nil for a legacy-composition project, and for
// one generate has not composed yet (no internal/app/compose.go — a fresh
// scaffold whose bootstrap generate failed). There, the constructors reach is
// read from, and the providers that open the database, do not exist, so the
// graph would claim the roots reach almost nothing: config_gen.k's ROOTS then
// refuse the scaffold's own `config_secrets = ["DATABASE_URL"]` and no env
// renders. nil is "reach unknown", which config_gen.k projects as the default
// `server` root reaching every field (GenerateConfigNativeSharedRoots).
func GraphRoots(projectDir string) ([]GraphRoot, error) {
	if !GraphComposition(projectDir) {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(projectDir, "internal", "app", "compose.go")); err != nil {
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
