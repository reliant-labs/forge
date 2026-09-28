package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/naming"
)

// An env's deploy/kcl/<env>/main.k BINDS each workload to where it runs, one
// line per workload (ADR 0002 §2: there is no env-level runtime):
//
//	_workloads = [
//	    _on_host(wl.item)
//	    _on_host_job(wl.migrate)
//	    _on_k3d(wl.reaper)
//	]
//
// Each `_<binder>` is a lambda the env declares — it applies the env's own
// layer (env vars, capacity) and one runtime. The scaffold writes these
// lines, and `forge scaffold <kind>` appends one per env for a new workload,
// so a workload added later is bound exactly like one the project was born
// with.

// DevEnvName is the env the scaffold binds to the local loop (host processes
// plus the local k3d cluster). Every other env is bound to its own cluster.
const DevEnvName = "dev"

// EnvBinder names the binder an env uses for a workload of kind: in the
// local loop a host process where one can run and the local cluster where
// it cannot (a cron is scheduled by Kubernetes, an operator watches its
// API); elsewhere the env's cluster. A tool is never run anywhere.
func EnvBinder(env, kind string) string {
	if kind == WorkloadKindTool {
		return "_build_only"
	}
	if env != DevEnvName {
		return "_on_cluster"
	}
	switch kind {
	case WorkloadKindCron, WorkloadKindOperator:
		return "_on_k3d"
	case WorkloadKindJob:
		return "_on_host_job"
	default:
		return "_on_host"
	}
}

// EnvBinding is one line of an env's `_workloads` list.
func EnvBinding(env, kind, workloadName string) string {
	return fmt.Sprintf("    %s(wl.%s)", EnvBinder(env, kind), naming.KCLIdentifier(workloadName))
}

// envWorkloadsList matches the scaffolded `_workloads = [` ... `]` block,
// capturing its body. The closing bracket is the first line that is exactly
// `]`, which is how the templates write it.
var envWorkloadsList = regexp.MustCompile(`(?ms)^_workloads = \[\n(.*?)^\]$`)

// AppendEnvBinding adds the binding for a new workload to one env's main.k,
// at the end of its `_workloads` list, and reports whether it did.
//
// Like AppendWorkloadStanza it never rewrites what is there: it returns
// applied=false with no write when the file is missing, already binds the
// workload (`wl.<ident>` anywhere in the list), or has been restructured
// past the point where the list is unambiguous. The caller prints the line.
func AppendEnvBinding(projectDir, env, kind, workloadName string) (applied bool, err error) {
	path := filepath.Join(projectDir, "deploy", "kcl", env, "main.k")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	content := string(raw)
	locs := envWorkloadsList.FindAllStringSubmatchIndex(content, -1)
	if len(locs) != 1 {
		return false, nil
	}
	body := content[locs[0][2]:locs[0][3]]
	ref := regexp.MustCompile(`\bwl\.` + regexp.QuoteMeta(naming.KCLIdentifier(workloadName)) + `\b`)
	if ref.MatchString(StripKCLProse(body)) {
		return false, nil
	}
	line := EnvBinding(env, kind, workloadName) + "\n"
	updated := content[:locs[0][3]] + line + content[locs[0][3]:]
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// EnvBindingHint is the message shown when a binding could not be appended.
func EnvBindingHint(env, kind, workloadName string) string {
	return fmt.Sprintf("Bind workload %q in deploy/kcl/%s/main.k — add to its `_workloads` list:\n\n%s\n",
		workloadName, env, strings.TrimRight(EnvBinding(env, kind, workloadName), "\n"))
}
