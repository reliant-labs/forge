package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/kcleval"
	"github.com/reliant-labs/forge/internal/obsdash"
	deployv1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// dashboardProcesses reads deployed process identities, never proto package
// names. Multiple environments declaring the same executable/role contribute
// workload aliases to one dashboard; roles of a shared executable stay apart.
func dashboardProcesses(ctx *pipelineContext) ([]obsdash.Process, error) {
	roots := ctx.graphRoots
	if !ctx.graphRootsSet {
		var err error
		roots, err = codegen.GraphRoots(ctx.AbsPath)
		if err != nil {
			return nil, err
		}
	}
	workloads, err := dashboardDeclaredWorkloads(ctx.AbsPath)
	if err != nil {
		return nil, err
	}
	processes, err := processesForWorkloads(bootstrapBinaryName(ctx.AbsPath), roots, workloads)
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

// dashboardDeclaredWorkloads evaluates the source library, not environment
// binding. A scaffold can have a valid operator/root with prod not yet bound to
// a cluster. That must not prevent codegen, nor retire a partial inventory.
// Environment-specific names/OTEL overrides are not inferred here: diagnostics
// explicitly name this boundary so a binding cannot silently claim coverage.
func dashboardDeclaredWorkloads(projectDir string) ([]WorkloadEntity, error) {
	const file = "deploy/kcl/workloads.k"
	if _, err := os.Stat(filepath.Join(projectDir, file)); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	armReadOnlyKCLContext(projectDir)
	before := dirtyTrackedFiles(projectDir)
	defer func() { reportImpureRender("structural workloads", restoreImpureKCLWrites(projectDir, before)) }()
	fmt.Println("  ℹ️  process dashboards use workloads.k ALL identities; environment-only name/OTEL_SERVICE_NAME overrides must retain those identities or use an owned dashboard override.")
	result, err := kcleval.Eval(kcleval.Request{ProjectDir: projectDir, File: file, Selectors: []string{"ALL"}})
	if err != nil {
		return nil, fmt.Errorf("dashboard structural process inventory (%s): %w", file, err)
	}
	raw, err := kcleval.Render(result, kcleval.FormatJSON)
	if err != nil {
		return nil, err
	}
	return decodeDashboardWorkloads(raw)
}

func decodeDashboardWorkloads(raw []byte) ([]WorkloadEntity, error) {
	var declared []struct {
		Name    string                     `json:"name"`
		Kind    string                     `json:"kind"`
		Build   json.RawMessage            `json:"build"`
		Command []string                   `json:"command"`
		Args    []string                   `json:"args"`
		Env     map[string]json.RawMessage `json:"env"`
	}
	if err := json.Unmarshal(raw, &declared); err != nil {
		return nil, err
	}
	out := make([]WorkloadEntity, 0, len(declared))
	for _, d := range declared {
		build, err := dispatchBuild(d.Name, d.Build)
		if err != nil {
			return nil, err
		}
		w := WorkloadEntity{Name: d.Name, Kind: d.Kind, Build: build, Spec: deployv1.WorkloadSpec{Command: d.Command, Args: d.Args}}
		// Identity may only be a declared literal. A secret/reference/expression
		// unresolved by KCL is not guessed from another field.
		if value, ok := d.Env["OTEL_SERVICE_NAME"]; ok {
			var service string
			if err := json.Unmarshal(value, &service); err != nil {
				return nil, fmt.Errorf("workload %q: OTEL_SERVICE_NAME must be a literal for process dashboard ownership", d.Name)
			}
			w.Spec.Env = []deployv1.EnvVar{{Name: "OTEL_SERVICE_NAME", Value: service}}
		}
		out = append(out, w)
	}
	return out, nil
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
