package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// WorkloadsKCLRelPath is the project-relative path of the workload
// declaration — what the project DEPLOYS, one typed literal per deployable
// unit (an image + args + placement).
//
// It is SCAFFOLDED ONCE and owned by the project from then on: forge writes
// it when the file does not exist and appends to it when `forge scaffold`
// adds a component; it never rewrites or reformats what is already there.
// Deploy has exactly one source of truth, it is KCL, and it is tracked in
// git — so a fresh clone renders with nothing generated first.
const WorkloadsKCLRelPath = "deploy/kcl/workloads.k"

// WorkloadsKCLExists reports whether the project already declares its
// workloads. Callers use it to decide between scaffolding the file and
// appending to it; neither path ever overwrites existing content.
func WorkloadsKCLExists(projectDir string) bool {
	_, err := os.Stat(filepath.Join(projectDir, WorkloadsKCLRelPath))
	return err == nil
}

// MigrateArgs returns the subcommand of the project binary that applies its
// embedded migrations (`db migrate up`, cmd-tree-db.go.tmpl).
//
// It is the workload's `args`, not a container argv: the image's ENTRYPOINT
// is the binary (Dockerfile.tmpl), and the host runtime derives
// `go run ./cmd/<project> db migrate up` from the same build + args. One
// declaration selects the subcommand on every runtime, so it cannot drift
// from the image or the host process it runs in.
//
// This is a SCAFFOLD-TIME default only. It is written literally into
// deploy/kcl/workloads.k once, and the project owns it from then on: how a
// system migrates is an operational decision that differs per environment and
// changes over time (an env may migrate out of band, or run a different
// tool entirely). forge does not re-derive it.
func MigrateArgs() []string {
	return []string{"db", "migrate", "up"}
}

// MigrateWorkloadName is the name of the scaffolded migration workload. It
// is a workload name like any other, so it must be a legal DNS-1123 label —
// which is also why it can never collide with the `*` broadcast selector.
const MigrateWorkloadName = "migrate"

// MigrateWorkloadStanza renders the deploy-time migration step as the KCL
// literal that belongs in deploy/kcl/workloads.k.
//
// It is an ORDINARY WORKLOAD — `kind = "job"` with the broadcast `before`
// — and that is the entire point. Migration used to be a bespoke
// `migrate: [str]` field on WorkloadEnv that the render layer had special
// knowledge of. It is now one instance of the one-shot primitive, so it
// reads, refines, and can be dropped exactly like anything else the
// project ships.
//
// `before = [fw.BEFORE_ALL]` rather than an enumerated list of dependents
// is the load-bearing choice. An enumerated list is a list that goes
// STALE: add a workload six months from now and the list still renders,
// still passes every check, and silently does not gate the new one — a
// pod serving traffic against a schema it does not have. The broadcast
// selector has nothing to forget to update.
//
// The `build` names the project's own binary, the same GoBuild every
// component declares: forge builds a (cmd, output) pair once however many
// workloads name it, so this compiles nothing extra. It is what lets the
// host runtime derive `go run ./cmd/<project> db migrate up`, and the
// cluster runtime pick the image, from one declaration.
//
// `config_secrets = ["DATABASE_URL"]`: the job needs the DSN and nothing
// else, so it is the one credential projected into it.
func MigrateWorkloadStanza(projectName string) string {
	var b strings.Builder
	b.WriteString("# The deploy-time schema migration — YOURS to change.\n")
	b.WriteString("#\n")
	b.WriteString("# `before = [fw.BEFORE_ALL]` is the BROADCAST form: it gates EVERY workload\n")
	b.WriteString("# in whichever environment deploys it, without naming any of them. On a\n")
	b.WriteString("# cluster that is an initContainer on each dependent, so the schema is\n")
	b.WriteString("# current BEFORE any new pod serves a request and a failed migration stalls\n")
	b.WriteString("# the rollout with the old pods still serving. On the host runtime forge\n")
	b.WriteString("# runs it to completion before starting anything it gates, and on the\n")
	b.WriteString("# hosted runtime the control plane does the same. Concurrent replicas are\n")
	b.WriteString("# safe — golang-migrate takes a postgres advisory lock.\n")
	b.WriteString("#\n")
	b.WriteString("# Do NOT replace the wildcard with a list of workload names. The list would\n")
	b.WriteString("# go stale the next time someone adds a workload, and the failure is silent:\n")
	b.WriteString("# it renders, it passes every check, and the new workload just is not gated.\n")
	b.WriteString("#\n")
	b.WriteString("# An environment that migrates OUT OF BAND (a DBA-run pipeline, a managed\n")
	b.WriteString("# database console, a separate release train) drops this workload from the\n")
	b.WriteString("# list it deploys, in deploy/kcl/<env>/main.k:\n")
	b.WriteString("#\n")
	b.WriteString("#     workloads = [w for w in wl.ALL if w.name != \"" + MigrateWorkloadName + "\"]\n")
	b.WriteString("#\n")
	b.WriteString("# To run a different tool entirely, set `command` (the argv, verbatim).\n")
	fmt.Fprintf(&b, "%s = fw.Workload {\n", naming.KCLIdentifier(MigrateWorkloadName))
	fmt.Fprintf(&b, "    name = %q\n", MigrateWorkloadName)
	fmt.Fprintf(&b, "    kind = %q\n", WorkloadKindJob)
	fmt.Fprintf(&b, "    build = %s\n", projectGoBuild(projectName))
	fmt.Fprintf(&b, "    args = %s\n", kclStringList(MigrateArgs()))
	b.WriteString("    before = [fw.BEFORE_ALL]\n")
	b.WriteString("    config_secrets = [\"DATABASE_URL\"]\n")
	b.WriteString("}\n")
	return b.String()
}

// kclStringList formats a Go string slice as a KCL list literal.
func kclStringList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, s := range items {
		quoted = append(quoted, fmt.Sprintf("%q", s))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// projectGoBuild is the typed build of the project's primary binary, as a
// KCL literal. The project name is used VERBATIM (hyphens preserved): `go
// build ./cmd/<hyphenated>` is valid, because it is a directory path rather
// than a package identifier.
func projectGoBuild(projectName string) string {
	return goBuild("./cmd/"+projectName, projectName)
}

func goBuild(cmd, outputName string) string {
	return fmt.Sprintf("forge.GoBuild {cmd = %q, output_name = %q}", cmd, outputName)
}

// IDPProvisionArgs returns the subcommand that converges this project's dev
// IdP application — the identity twin of MigrateArgs.
func IDPProvisionArgs() []string {
	return []string{"auth", "idp-provision"}
}

// IDPProvisionWorkloadName is the name of the scaffolded IdP-convergence
// workload, and the ConfigMap a cluster target's consumers reference by
// name (see the Role grant this stanza also declares).
const IDPProvisionWorkloadName = "idp-provision"

// IDPProvisionConfigMapName is the ConfigMap this job publishes into on a
// cluster target — deterministic from the project name, so a consumer's
// configMapKeyRef can be written by hand without reading this job's flags.
func IDPProvisionConfigMapName(projectName string) string {
	return projectName + "-idp-identity"
}

// IDPProvisionWorkloadStanza renders the dev-IdP convergence step as the
// KCL literal that belongs in deploy/kcl/workloads.k, following the exact
// precedent MigrateWorkloadStanza sets: `kind = "job"`, the project's own
// binary, a subcommand in `args`.
//
// UNLIKE migrate, this is not broadcast-gated: nothing needs to wait on
// it, because nothing else reads its output through an ordering
// dependency — a consumer reads the published ConfigMap (cluster) or the
// committed KCL file (host dev loop) whenever it next renders, not via an
// initContainer. `before` therefore stays empty.
//
// It is a DEV job: it registers against the dev identity provider the dev
// env runs. The scaffolded dev env deploys it; staging and prod, whose
// issuer is a real one registered out of band, leave it out of their
// workload list.
//
// No RBAC on the base declaration. A host process has no Kubernetes
// identity (the host runtime refuses RBAC rather than dropping it), and the
// dev env runs this job on the host. An env that runs it on a cluster, where
// it publishes a ConfigMap, grants the write THERE, where the runtime makes
// it meaningful (see the stanza's comment).
func IDPProvisionWorkloadStanza(projectName string) string {
	var b strings.Builder
	b.WriteString("# The dev-IdP identity convergence step — YOURS to change.\n")
	b.WriteString("#\n")
	b.WriteString("# Registers this project's browser application against the dev identity\n")
	b.WriteString("# provider and PUBLISHES what it generates (the client_id, the project id\n")
	b.WriteString("# that becomes the token audience) — never returns them through a render-time\n")
	b.WriteString("# hook. As a host process (the dev env) the output is a committed KCL file\n")
	b.WriteString("# this project's dev config.k imports; on a cluster it is a ConfigMap,\n")
	b.WriteString("# referenced by name via configMapKeyRef. See `cmd/" + projectName + "/cmd/auth.go`\n")
	b.WriteString("# for the convergence logic and pkg/devidp for the two publishers.\n")
	b.WriteString("#\n")
	b.WriteString("# Only the dev env deploys it: a deployed env's issuer is a real one whose\n")
	b.WriteString("# applications are registered out of band. To run it on a cluster instead,\n")
	b.WriteString("# grant the ConfigMap write where you bind it:\n")
	b.WriteString("#\n")
	b.WriteString("#     wl.idp_provision | {\n")
	b.WriteString("#         runtime = forge.OnCluster {target = _k3d}\n")
	b.WriteString("#         namespacedRBAC = [fw.PolicyRule {apiGroups = [\"\"], resources = [\"configmaps\"], verbs = [\"get\", \"create\", \"update\", \"patch\"]}]\n")
	b.WriteString("#     }\n")
	fmt.Fprintf(&b, "%s = fw.Workload {\n", naming.KCLIdentifier(IDPProvisionWorkloadName))
	fmt.Fprintf(&b, "    name = %q\n", IDPProvisionWorkloadName)
	fmt.Fprintf(&b, "    kind = %q\n", WorkloadKindJob)
	fmt.Fprintf(&b, "    build = %s\n", projectGoBuild(projectName))
	fmt.Fprintf(&b, "    args = %s\n", kclStringList(IDPProvisionArgs()))
	b.WriteString("}\n")
	return b.String()
}

// WorkloadStanza renders ONE workload as the typed KCL literal that belongs
// in deploy/kcl/workloads.k. It is the single formatter behind both the
// initial scaffold and `forge scaffold <kind>`'s append, so a workload added
// later is indistinguishable from one the project was born with.
//
// The emitted declaration is COMPLETE and RUNTIME-INDEPENDENT (ADR 0002): the
// build, the subcommand in `args`, the ports, the credentials it reads. It
// says nothing about WHERE it runs — each env binds it to a runtime
// (`forge.OnHost`, `OnCluster`, `OnHosted`, ...) and the same `args` select the
// subcommand on every one: `go run ./cmd/<p> <args>` on the host, the image's
// ENTRYPOINT `/app/<p>` plus `<args>` in a pod. Per-env facts (replicas,
// resources, env values) are refined in deploy/kcl/<env>/main.k.
//
// No probes: the lowering writes `/readyz` + `/healthz` on the `http` port
// into the spec of every service forge builds, on every runtime, which is
// exactly what serverkit serves.
func WorkloadStanza(projectName string, c config.ComponentConfig) string {
	var b strings.Builder
	kind := WorkloadKindFor(c.EffectiveKind())

	fmt.Fprintf(&b, "%s = fw.Workload {\n", naming.KCLIdentifier(c.Name))
	fmt.Fprintf(&b, "    name = %q\n", c.Name)
	fmt.Fprintf(&b, "    kind = %q\n", kind)

	// Build target and subcommand. A secondary binary is its OWN program:
	// it builds its own cmd/<binpkg> package and runs with no subcommand.
	// Every other component is a subcommand of the project binary
	// (`<bin> <component>`, generated under cmd/<bin>/cmd/{services,workers,
	// operators}/), which runs exactly that component — one service's
	// routes, one worker, one operator — rather than `server`, which runs
	// them all.
	if c.EffectiveKind() == config.ComponentKindBinary {
		binPkg := naming.ServicePackage(c.Name)
		fmt.Fprintf(&b, "    build = %s\n", goBuild("./cmd/"+binPkg, binPkg))
	} else {
		fmt.Fprintf(&b, "    build = %s\n", projectGoBuild(projectName))
		fmt.Fprintf(&b, "    args = %s\n", kclStringList([]string{componentSubcommand(c)}))
	}
	if kind == WorkloadKindOperator {
		if c.Group != "" {
			fmt.Fprintf(&b, "    group = %q\n", c.Group)
		}
		if c.Version != "" {
			fmt.Fprintf(&b, "    version = %q\n", c.Version)
		}
		crds := make([]string, 0, len(c.CRDs))
		for _, crd := range c.CRDs {
			crds = append(crds, fmt.Sprintf("%q", crd.Name))
		}
		fmt.Fprintf(&b, "    crds = [%s]\n", strings.Join(crds, ", "))
	}

	// A service serves the standard mux on the one port forge itself knows
	// (config.DefaultServePort), named `http` — the port the default probes
	// target and the one an env's routes address.
	if kind == WorkloadKindService {
		fmt.Fprintf(&b, "    ports = [fw.Port {name = \"http\", port = %d, expose = True}]\n",
			config.DefaultServePort)
	}
	// Every scheduled component of the project binary reads the database:
	// the binary constructs the whole DI graph whichever subcommand runs.
	// A tool is never scheduled, so it reads nothing.
	if kind != WorkloadKindTool {
		b.WriteString("    config_secrets = [\"DATABASE_URL\"]\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// componentSubcommand is the cobra subcommand the project binary registers
// for a component: CmdServiceCommand's kebab form for a service (the same
// derivation the cmd tree uses), and the component's own name for a worker
// or operator (cmd-worker-group / cmd-operator-group `Use:`).
func componentSubcommand(c config.ComponentConfig) string {
	if c.EffectiveKind() == config.ComponentKindServer || c.EffectiveKind() == "" {
		if runtime, _, ok := CmdServiceCommand(c.Name); ok {
			return runtime
		}
	}
	return c.Name
}

// The KCL workload kinds — how a deployable unit RUNS. These are the values
// of `forge.workloads.Workload.kind` and are deliberately a different axis
// from config.ComponentKind*, which describes what forge SCAFFOLDS (whether
// a component gets a proto, a cmd/ dir, bootstrap wiring). Two components of
// different Go kinds can deploy identically; WorkloadKindFor is the mapping.
const (
	WorkloadKindService  = "service"
	WorkloadKindWorker   = "worker"
	WorkloadKindCron     = "cron"
	WorkloadKindJob      = "job"
	WorkloadKindOperator = "operator"
	WorkloadKindTool     = "tool"
)

// WorkloadKindFor maps a scaffolded component kind onto the KCL workload
// kind that deploys it.
//
// The two axes are NOT the same, which is the whole reason this function
// exists rather than a cast:
//
//   - config.ComponentKindServer names a Connect-RPC surface — a code fact.
//     It deploys as a `service` (Deployment + Service).
//   - config.ComponentKindBinary names a second cmd/<name>/main.go — a build
//     fact. It deploys as a `tool`: forge builds it into the image, but
//     nothing schedules it. It is run on demand (`kubectl exec`, a CI step),
//     which is exactly what the old `binary` deploy kind meant in practice —
//     its manifests were an unscheduled Deployment nobody addressed.
//
//   - config.ComponentKindCron is a worker whose process runs an IN-PROCESS
//     scheduler (worker-cron: robfig/cron, Start blocks until shutdown). It
//     deploys as a long-running `worker`. A Kubernetes CronJob would start
//     that process on the schedule and wait for an exit that never comes.
//
// An unrecognized kind falls back to `service`: a plain Deployment+Service is
// the least surprising thing to render for something forge has no name for.
func WorkloadKindFor(componentKind string) string {
	switch componentKind {
	case config.ComponentKindWorker:
		return WorkloadKindWorker
	case config.ComponentKindCron:
		// See above: the scheduler lives in the process.
		return WorkloadKindWorker
	case config.ComponentKindJob:
		return WorkloadKindJob
	case config.ComponentKindOperator:
		return WorkloadKindOperator
	case config.ComponentKindBinary:
		return WorkloadKindTool
	default:
		return WorkloadKindService
	}
}
