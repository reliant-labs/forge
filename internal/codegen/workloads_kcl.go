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

// MigrateCommand returns the argv that applies a project's embedded
// migrations from inside its runtime image.
//
// `/app/<project>` is where the generated Dockerfile's production stage puts
// the primary binary (WORKDIR /app, `COPY --from=builder /app/bin/<project>
// ./<project>`), and `db migrate up` is the subcommand that applies the
// EMBEDDED set (cmd-tree-db.go.tmpl). Both halves are derived from the same
// project name, so the argv cannot drift from the image it runs in.
//
// This is a SCAFFOLD-TIME default only. It is written literally into
// deploy/kcl/workloads.k once, and the project owns it from then on: how a
// system migrates is an operational decision that differs per environment and
// changes over time (an env may migrate out of band, or run a different
// tool entirely). forge does not re-derive it.
func MigrateCommand(projectName string) []string {
	return []string{"/app/" + projectName, "db", "migrate", "up"}
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
// No `build` block: the migration runs the project's own image, which
// some other workload already builds. Declaring a second build target for
// the same binary would compile it twice.
func MigrateWorkloadStanza(projectName string) string {
	var b strings.Builder
	b.WriteString("# The deploy-time schema migration — YOURS to change.\n")
	b.WriteString("#\n")
	b.WriteString("# `before = [fw.BEFORE_ALL]` is the BROADCAST form: it gates EVERY workload\n")
	b.WriteString("# in whichever environment deploys it, without naming any of them. On\n")
	b.WriteString("# Kubernetes that is an initContainer on each dependent, so the schema is\n")
	b.WriteString("# current BEFORE any new pod serves a request and a failed migration stalls\n")
	b.WriteString("# the rollout with the old pods still serving. On compose it is\n")
	b.WriteString("# `depends_on: {condition: service_completed_successfully}`. Concurrent\n")
	b.WriteString("# replicas are safe — golang-migrate takes a postgres advisory lock.\n")
	b.WriteString("#\n")
	b.WriteString("# Do NOT replace the wildcard with a list of workload names. The list would\n")
	b.WriteString("# go stale the next time someone adds a workload, and the failure is silent:\n")
	b.WriteString("# it renders, it passes every check, and the new workload just is not gated.\n")
	b.WriteString("#\n")
	b.WriteString("# An environment that migrates OUT OF BAND (a DBA-run pipeline, a managed\n")
	b.WriteString("# database console, a separate release train) drops this workload from the\n")
	b.WriteString("# list it deploys, in deploy/kcl/<env>/main.k:\n")
	b.WriteString("#\n")
	b.WriteString("#     _declared = [w for w in wl.ALL if w.name != \"" + MigrateWorkloadName + "\"]\n")
	b.WriteString("#\n")
	b.WriteString("# To run a different tool entirely, change the command below.\n")
	fmt.Fprintf(&b, "%s = fw.Workload {\n", naming.KCLIdentifier(MigrateWorkloadName))
	fmt.Fprintf(&b, "    name = %q\n", MigrateWorkloadName)
	fmt.Fprintf(&b, "    kind = %q\n", WorkloadKindJob)
	fmt.Fprintf(&b, "    command = %s\n", kclStringList(MigrateCommand(projectName)))
	b.WriteString("    before = [fw.BEFORE_ALL]\n")
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

// IDPProvisionCommand returns the argv that converges this project's dev
// IdP application from inside its runtime image — the identity twin of
// MigrateCommand.
func IDPProvisionCommand(projectName string) []string {
	return []string{"/app/" + projectName, "auth", "idp-provision"}
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
// image, no `build` block of its own.
//
// UNLIKE migrate, this is not broadcast-gated: nothing needs to wait on
// it, because nothing else reads its output through an ordering
// dependency — a consumer reads the published ConfigMap (cluster) or the
// committed KCL file (compose/dev) whenever it next renders, not via an
// initContainer. `before` therefore stays empty; this workload converges
// on its own schedule.
//
// `namespaced_rbac` is the RBAC grant this job needs on a CLUSTER target:
// get/create/update/patch on ConfigMaps in its OWN namespace, so it can
// publish the identity it converges. get/list/watch on configmaps/secrets
// (every workload's default) is not enough — this workload WRITES, and
// nothing else forge scaffolds needs to. It costs nothing on compose/dev,
// where the equivalent output is a committed file and the RBAC block
// simply does not apply (compose has no Role/RoleBinding concept).
func IDPProvisionWorkloadStanza(projectName string) string {
	var b strings.Builder
	b.WriteString("# The dev-IdP identity convergence step — YOURS to change.\n")
	b.WriteString("#\n")
	b.WriteString("# Registers this project's browser application against the dev identity\n")
	b.WriteString("# provider and PUBLISHES what it generates (the client_id, the project id\n")
	b.WriteString("# that becomes the token audience) — never returns them through a render-time\n")
	b.WriteString("# hook. On a cluster target the output is a ConfigMap, referenced by name via\n")
	b.WriteString("# configMapKeyRef; on compose/dev it is a committed KCL file this project's\n")
	b.WriteString("# config.k imports directly. See `cmd/" + projectName + "/cmd/auth.go` for the\n")
	b.WriteString("# convergence logic and pkg/devidp for the two publishers.\n")
	b.WriteString("#\n")
	b.WriteString("# `namespaced_rbac` grants exactly the write this job needs — get/create/\n")
	b.WriteString("# update/patch on ConfigMaps in its own namespace — and nothing more. It is\n")
	b.WriteString("# additive to the default config-read rules every workload's Role already\n")
	b.WriteString("# carries (see kcl/lib/rbac.k), and it is meaningful only for a cluster\n")
	b.WriteString("# target: compose has no Role/RoleBinding concept, so this field is inert\n")
	b.WriteString("# there.\n")
	b.WriteString("#\n")
	b.WriteString("# A project with no dev IdP (no frontend, or one pointed at a real issuer\n")
	b.WriteString("# instead) drops this workload from the list it deploys, in\n")
	b.WriteString("# deploy/kcl/<env>/main.k:\n")
	b.WriteString("#\n")
	b.WriteString("#     _declared = [w for w in wl.ALL if w.name != \"" + IDPProvisionWorkloadName + "\"]\n")
	fmt.Fprintf(&b, "%s = fw.Workload {\n", naming.KCLIdentifier(IDPProvisionWorkloadName))
	fmt.Fprintf(&b, "    name = %q\n", IDPProvisionWorkloadName)
	fmt.Fprintf(&b, "    kind = %q\n", WorkloadKindJob)
	fmt.Fprintf(&b, "    command = %s\n", kclStringList(IDPProvisionCommand(projectName)))
	b.WriteString("    namespaced_rbac = [\n")
	b.WriteString("        {apiGroups = [\"\"], resources = [\"configmaps\"], verbs = [\"get\", \"create\", \"update\", \"patch\"]}\n")
	b.WriteString("    ]\n")
	b.WriteString("}\n")
	return b.String()
}

// WorkloadStanza renders ONE workload as the typed KCL literal that belongs
// in deploy/kcl/workloads.k. It is the single formatter behind both the
// initial scaffold and `forge scaffold <kind>`'s append, so a workload added
// later is indistinguishable from one the project was born with.
//
// The emitted declaration is COMPLETE — a valid workload on its own, ports
// and all. That is the inheritance contract: each environment REFINES this
// declaration with `|` rather than reconstructing it, so the base file
// answers "what is this?" in full and an env states only what it CHANGES.
// Per-env facts forge cannot know (replicas, resources, registry, secrets)
// keep their schema defaults here and are overridden in deploy/kcl/<env>/main.k.
func WorkloadStanza(projectName string, c config.ComponentConfig) string {
	var b strings.Builder
	kind := WorkloadKindFor(c.EffectiveKind())

	fmt.Fprintf(&b, "%s = fw.Workload {\n", naming.KCLIdentifier(c.Name))
	fmt.Fprintf(&b, "    name = %q\n", c.Name)
	fmt.Fprintf(&b, "    kind = %q\n", kind)

	// A secondary binary is its OWN entrypoint in the shared image: it lives
	// at cmd/<binpkg>/main.go and the Dockerfile builds it to /app/<binpkg>,
	// so the argv selects that binary directly. Every other kind runs the
	// image's default entrypoint.
	if c.EffectiveKind() == config.ComponentKindBinary {
		fmt.Fprintf(&b, "    command = [%q]\n", "/app/"+naming.ServicePackage(c.Name))
	}
	if kind == WorkloadKindCron && c.Schedule != "" {
		fmt.Fprintf(&b, "    schedule = %q\n", c.Schedule)
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
	// (config.DefaultServePort). Stating it HERE — rather than leaving it to
	// a per-env overlay — is what makes this declaration complete: the port
	// is a fact about the workload, and an env that genuinely differs
	// overrides it.
	if kind == WorkloadKindService {
		fmt.Fprintf(&b, "    ports = [fw.Port {name = \"http\", port = %d, expose = True}]\n",
			config.DefaultServePort)
	}

	// Build target. A secondary binary builds its own cmd/<binpkg> package;
	// everything else builds the shared cmd/<project> binary and selects its
	// behavior via a cobra subcommand at runtime. The project name is used
	// VERBATIM (hyphens preserved): `forge project new` scaffolds the raw
	// ./cmd/<project> path and `go build ./cmd/<hyphenated>` is valid,
	// because it is a directory path rather than a package identifier.
	buildCmd, outputName := "./cmd/"+projectName, projectName
	if c.EffectiveKind() == config.ComponentKindBinary {
		binPkg := naming.ServicePackage(c.Name)
		buildCmd, outputName = "./cmd/"+binPkg, binPkg
	}
	fmt.Fprintf(&b, "    build = {type = \"go\", cmd = %q, output_name = %q}\n", buildCmd, outputName)
	b.WriteString("}\n")
	return b.String()
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
// An unrecognized kind falls back to `service`: a plain Deployment+Service is
// the least surprising thing to render for something forge has no name for.
func WorkloadKindFor(componentKind string) string {
	switch componentKind {
	case config.ComponentKindWorker:
		return WorkloadKindWorker
	case config.ComponentKindCron:
		return WorkloadKindCron
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
