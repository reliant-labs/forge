// File: internal/codegen/composition_roots_kcl.go
//
// The deploy half of per-root config (composition graph PR 6): config_gen.k's
// ROOTS, and the two lambdas that bind a workload to a root.
//
//   - ROOTS: one Root per root of the composition graph — the args that run
//     it and the env vars it reaches, split into values and secrets, read off
//     its closure when generate ran (GraphRoot, composition_graph_config.go).
//   - root_of(name) is what a workload declaration merges in to run a root:
//     `{root, args}`. An unknown root fails the render, naming the roots.
//   - workloadEnvMap(cfg, w) is a workload's config env: for a workload bound
//     to a root, exactly the values the root reaches and, of the credentials,
//     the ones it reaches AND requires plus those `config_secrets` opts into
//     (each of which the root must reach — the render fails on one it does
//     not, so a credential reaches only the workloads whose root reads it);
//     for any other workload, appConfigEnvMap as before.
//
// A root's optional credentials stay opt-in for the reason appConfigEnvMap
// gives: an env without that Secret key must not stall the pod.

package codegen

import (
	"sort"
	"strconv"

	"github.com/reliant-labs/forge/internal/templates"
)

// RootsKCL renders the ROOTS block appended to config_gen.k, or "" when the
// roots' reach is unknown (no root config message) or there are none.
func RootsKCL(roots []GraphRoot) string {
	if len(roots) == 0 {
		return ""
	}
	for _, r := range roots {
		if r.Config == nil {
			return ""
		}
	}
	sorted := append([]GraphRoot(nil), roots...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	data := rootsKCLData{}
	for _, r := range sorted {
		v := rootKCLView{Name: strconv.Quote(r.Name), Args: kclStringList(r.Args)}
		var env, secrets, required []string
		for _, l := range r.Config {
			if l.EnvVar == "" {
				continue
			}
			switch {
			case l.Sensitive && l.Required:
				required = append(required, l.EnvVar)
				secrets = append(secrets, l.EnvVar)
			case l.Sensitive:
				secrets = append(secrets, l.EnvVar)
			default:
				env = append(env, l.EnvVar)
			}
		}
		v.Env, v.Secrets, v.Required = kclStringList(env), kclStringList(secrets), kclStringList(required)
		data.Roots = append(data.Roots, v)
	}
	out, err := templates.ProjectTemplates().Render("config-roots.k.tmpl", data)
	if err != nil {
		panic("render config-roots.k.tmpl (a forge bug): " + err.Error())
	}
	return string(out)
}

// rootsKCLData is config-roots.k.tmpl's input: each root's KCL literals.
type rootsKCLData struct{ Roots []rootKCLView }

type rootKCLView struct{ Name, Args, Env, Secrets, Required string }

// DefaultServerRoot is the root a graph-mode project's config module projects
// when this run did not read the graph (a narrowed run, a project whose graph
// does not assemble yet): the default `server`, reaching every field — what
// it reaches in a scaffold, whose API listener takes the whole config.
func DefaultServerRoot(fields []ConfigField) GraphRoot {
	r := GraphRoot{Name: "server", Args: []string{"server"}, Config: []ConfigLeaf{}}
	for _, f := range fields {
		path := f.Name
		if f.KCLBlock != "" {
			path = f.KCLBlock + "." + f.Name
		}
		r.Config = append(r.Config, ConfigLeaf{Path: path, EnvVar: f.EnvVar, Sensitive: f.Sensitive, Required: f.Required})
	}
	return r
}
