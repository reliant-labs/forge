package cli

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/obsdash"
)

// dashboardSpecForEntities assembles from the already-rendered selected env.
// It does not render, write, retire artifacts, or inspect another environment.
func dashboardSpecForEntities(projectDir string, entities *KCLEntities) (*obsdash.Spec, error) {
	in, ok, err := obsdash.ReadFragments(projectDir)
	if err != nil || !ok {
		return nil, err
	}
	if entities == nil {
		return nil, fmt.Errorf("process dashboard assembly requires the selected environment's rendered workloads")
	}
	module, err := codegen.GetModulePath(projectDir)
	if err != nil {
		return nil, err
	}
	services, err := codegen.ParseDeclaredServices(projectDir)
	if err != nil {
		return nil, err
	}
	cfg := &config.ProjectConfig{Name: in.Project, ModulePath: module}
	ctx := &pipelineContext{AbsPath: projectDir, ProjectDir: projectDir, Cfg: cfg, Services: codegen.ManagedServices(services)}
	in.Processes, err = renderedDashboardProcesses(ctx, entities)
	if err != nil {
		return nil, err
	}
	sp := obsdash.Build(in)
	return &sp, nil
}

// dashboardProcesses reads deployed process identities, never proto package
// names. Multiple environments declaring the same executable/role contribute
// workload aliases to one dashboard; roles of a shared executable stay apart.
func renderedDashboardProcesses(ctx *pipelineContext, entities *KCLEntities) ([]obsdash.Process, error) {
	roots := ctx.graphRoots
	if !ctx.graphRootsSet {
		var err error
		roots, err = codegen.GraphRoots(ctx.AbsPath)
		if err != nil {
			return nil, err
		}
	}
	processes, err := processesForWorkloads(bootstrapBinaryName(ctx.AbsPath), roots, entities.Workloads)
	if err != nil {
		return nil, err
	}
	for i := range processes {
		p := &processes[i]
		if p.Root != "" {
			continue
		}
		p.RPCServices, err = codegen.CommandRPCServices(ctx.AbsPath, ctx.Cfg.ModulePath, p.Binary, p.Args, ctx.Services)
		if err != nil {
			return nil, err
		}
		// Legacy composition has no database reach model; do not claim a DB.
	}
	return processes, nil
}

func processesForWorkloads(primary string, roots []codegen.GraphRoot, workloads []WorkloadEntity) ([]obsdash.Process, error) {
	byID := map[string]*obsdash.Process{}
	for _, w := range workloads {
		if w.Runtime.Type == RuntimeBuildOnly {
			continue
		}
		binary := ""
		if g := w.GoBuild(); g != nil {
			binary = filepath.Base(filepath.Clean(g.Cmd))
		}
		// An explicit entrypoint identifies prebuilt/shell-built Go executables.
		// Do not infer one from an image name or from an RPC package.
		if len(w.Spec.Command) > 0 {
			binary = filepath.Base(w.Spec.Command[0])
		}
		if binary == "" {
			continue
		}
		role := strings.Join(w.Spec.Args, "-")
		var root *codegen.GraphRoot
		if binary == primary {
			for i := range roots {
				if equalArgs(roots[i].Args, w.Spec.Args) {
					root = &roots[i]
					role = root.Name
					break
				}
			}
		}
		// A process without a matching graph root is still a real workload (jobs,
		// secondary binaries): keep its runtime panels, but claim no RPC ownership.
		if role == "" {
			role = "default"
		}
		id := dashboardProcessID(binary, role, w.Spec.Args)
		p := byID[id]
		if p == nil {
			p = &obsdash.Process{Name: id, Binary: binary, Args: append([]string(nil), w.Spec.Args...)}
			if root != nil {
				p.Root = root.Name
				p.RPCServices = append([]string(nil), root.RPCServices...)
				p.HasDatabase = root.HasDatabase
			}
			byID[id] = p
		} else if p.Binary != binary || !equalArgs(p.Args, w.Spec.Args) {
			return nil, fmt.Errorf("dashboard process id collision %q", id)
		}
		service := w.Name
		for _, e := range w.EnvVars() {
			if e.Name == "OTEL_SERVICE_NAME" && e.Value != "" {
				service = e.Value
			}
		}
		p.ServiceNames = append(p.ServiceNames, service)
		p.Workloads = append(p.Workloads, w.Name)
	}
	var out []obsdash.Process
	for _, p := range byID {
		p.ServiceNames = uniqueSorted(p.ServiceNames)
		p.Workloads = uniqueSorted(p.Workloads)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var processIDUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func dashboardProcessID(binary, role string, args []string) string {
	raw := binary + "-" + role
	safe := processIDUnsafe.ReplaceAllString(raw, "-")
	if len(safe) > 100 {
		safe = safe[:100]
	}
	if safe != raw {
		sum := sha256.Sum256([]byte(binary + "\x00" + strings.Join(args, "\x00")))
		safe += fmt.Sprintf("-%x", sum[:6])
	}
	return safe
}

func equalArgs(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }
func uniqueSorted(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, v := range values {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}
