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
	"fmt"
	"sort"
	"strconv"
	"strings"
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

	var b strings.Builder
	b.WriteString(`
# ---------------------------------------------------------------------------
# ROOTS — one per root of the composition graph (the //forge:root structs in
# internal/app, or the default ` + "`server`" + `): the args that run it and the config
# it REACHES, read off its closure by forge generate. Bind a workload to one:
#
#     api = fw.Workload {
#         name = "api"
#         build = forge.GoBuild {cmd = "./cmd/<bin>", output_name = "<bin>"}
#     } | config_gen.root_of("server")
#
# and project its env with ` + "`config_gen.workloadEnvMap(cfg, w)`" + `: exactly the
# values its root reaches and, of the credentials, those it reaches and
# requires, plus any ` + "`config_secrets`" + ` opts into (each must be reached).
# ---------------------------------------------------------------------------
schema Root:
    """One root of the composition graph, as a process."""
    args: [str]
    env: [str] = []
    secrets: [str] = []
    required_secrets: [str] = []

ROOTS: {str: Root} = {
`)
	for _, r := range sorted {
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
		fmt.Fprintf(&b, "    %s = Root {\n", strconv.Quote(r.Name))
		fmt.Fprintf(&b, "        args = %s\n", kclStringList(r.Args))
		fmt.Fprintf(&b, "        env = %s\n", kclStringList(env))
		fmt.Fprintf(&b, "        secrets = %s\n", kclStringList(secrets))
		fmt.Fprintf(&b, "        required_secrets = %s\n", kclStringList(required))
		b.WriteString("    }\n")
	}
	b.WriteString(`}

# root_of is what a workload merges in to run a root: its name and the args
# that run it. An unknown root fails the render.
root_of = lambda name: str -> {str:} {
    assert name in ROOTS, "root_of: no root named \"${name}\" — the roots are ${sorted([_k for _k in ROOTS])} (//forge:root structs in internal/app; forge generate refreshes this list)"
    {root = name, args = ROOTS[name].args}
}

# rootEnvMap is the config env of a workload bound to a root (w.root).
rootEnvMap = lambda c: AppConfig, w: any -> {str: str | forge.SecretRef} {
    assert w.root in ROOTS, "workload '${w.name}': root \"${w.root}\" is not a root of this binary — the roots are ${sorted([_k for _k in ROOTS])}"
    _r = ROOTS[w.root]
    _opted = w.config_secrets or []
    assert all _n in _opted { _n in _r.secrets }, \
        "workload '${w.name}': config_secrets names ${[_n for _n in _opted if _n not in _r.secrets]}, which root \"${w.root}\" does not reach (it reaches ${_r.secrets}): a credential reaches only the workloads whose root reads it"
    _all = appConfigEnvMap(c, _r.required_secrets + [_n for _n in _opted if _n not in _r.required_secrets])
    {_k: _v for _k, _v in _all if _k in _r.env or _k in _r.secrets}
}

# workloadEnvMap is a workload's config env: rootEnvMap for a workload bound
# to a root, appConfigEnvMap (its config_secrets) for any other.
workloadEnvMap = lambda c: AppConfig, w: any -> {str: str | forge.SecretRef} {
    rootEnvMap(c, w) if w.root else appConfigEnvMap(c, w.config_secrets or [])
}
`)
	return b.String()
}
