// Package cluster owns the render-KCL → kubectl-apply → wait-rollouts
// pipeline that `forge deploy`, `forge cluster reload`, and the
// deploy phase of `forge up` all execute. Before this package existed,
// the pipeline was duplicated across three call sites:
//
//   - runDeploy           (internal/cli/deploy.go)
//   - runDevClusterReload (internal/cli/dev_cluster.go)
//   - reconcileCluster    (internal/cli/up.go)
//
// All three drove the same kubectl invocations against the same KCL
// renderer; they differed only in the pre-flight (context guard vs
// context pin), the dev-cluster bootstrap (deploy-only), and the
// per-call defaults (prune, host-skip, one-shot Job wait).
//
// This package intentionally does NOT own:
//
//   - the kubectl-context guard (verifyKubectlContext) — that's the
//     deploy command's affordance and applies to non-dev envs the dev
//     cluster reload doesn't reach;
//   - the k3d cluster bootstrap (ensureDevCluster, buildAndPushLocal)
//     — that's a deploy-time concern for the dev env that the reload
//     deliberately skips;
//   - the typed KCLEntities schema (still in internal/cli/) — callers
//     compute the per-call HostSkip / OneShotJobs slices from that
//     and pass them in.
//
// The shape mirrors internal/hostlaunch: a small Opts struct
// expressing the differences between call sites, plus a single Apply
// entry point and the kubectl/KCL helpers exported for callers that
// need them piecewise.
package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/kclrender"
	"gopkg.in/yaml.v3"
)

// ApplyOpts expresses the differences between the three existing call
// sites. Every field has a sensible zero value so callers that don't
// care about a knob can leave it unset.
type ApplyOpts struct {
	// MainK is the path to deploy/kcl/<env>/main.k — the KCL entrypoint
	// that renders the cluster manifests. Required.
	MainK string

	// ImageTag is the value bound to KCL's `image_tag` -D variable.
	// Required (callers default to gitShortSHA at the call site).
	ImageTag string

	// Namespace is the value bound to KCL's `namespace` -D variable and
	// passed to every kubectl invocation. Required.
	Namespace string

	// EnvConfigKV is the per-env config map projected as additional
	// `-D key=value` bindings to KCL. May be nil — the dev cluster
	// reload doesn't project per-env config (it would force a re-deploy
	// pipeline rebuild, defeating the inner-loop purpose).
	EnvConfigKV map[string]string

	// DryRun skips kubectl apply and prints the rendered manifests
	// instead. With DryRunFramed, the output is wrapped in
	// "--- Generated Manifests (dry-run) ---" / "--- End Manifests ---"
	// markers (the forge deploy convention). Without it, raw manifests
	// are printed (the forge cluster reload convention).
	DryRun       bool
	DryRunFramed bool

	// Prune deletes forge-managed Deployments in the namespace that the
	// just-applied KCL render no longer produces. Opt-in — pruning is
	// destructive (see deploy.go's pruneOrphanDeployments docstring).
	Prune bool

	// HostSkip is the set of Deployment names to skip in the rollout
	// wait — services declared `deploy: host` in KCL, which run as host
	// processes and don't have a Deployment in the cluster. Empty
	// disables the skip (every managed Deployment is awaited).
	HostSkip map[string]struct{}

	// OneShotJobs is an OPTIONAL caller-supplied list of Job names to
	// wait on. Apply UNIONs it with every `kind: Job` it finds in the
	// rendered manifest stream (see RenderedJobNames), so the wait set
	// is authoritative-by-manifest and a caller no longer has to derive
	// it correctly for the schedule=="" migrate-Job wait to fire — this
	// field is now belt-and-suspenders for a Job not present in the
	// stream. Each Job in the union is waited on with `kubectl wait
	// --for=condition=complete` so the caller gets a definitive
	// done/fail signal before Apply returns. Scheduled CronJobs render
	// as `kind: CronJob` (not `kind: Job`) and are NOT waited on — they
	// run on their own cadence and the deploy is done once applied.
	OneShotJobs []string

	// Quiet suppresses the section-header banners ("Applying
	// manifests...", "Waiting for rollouts...") and emits the matching
	// per-resource warnings in the bare format ("Warning: <msg>" with
	// no leading indent) — the shape `forge cluster reload` used
	// pre-extraction. Off by default; the deploy and up call sites
	// keep the framed banners.
	Quiet bool

	// Env is the environment name (e.g. "dev", "dev-host", "prod")
	// passed to KCL as `-D env=<env>`. User main.k files can read it via
	// `option("env")` to conditionally include manifests — typical use
	// is skipping in-cluster infra (NATS, Temporal, LiteLLM) on dev-host
	// envs where docker-compose provides the same services.
	Env string

	// Context, when non-empty, is the kubectl context every kubectl
	// invocation in the apply/wait path runs against — passed as
	// `--context <ctx>` per command rather than mutating the global
	// active context (`kubectl config use-context`). This is what makes
	// concurrent multi-cluster `forge deploy` safe: two deploys sharing
	// one kubeconfig but targeting different clusters no longer race on
	// the single global context. Empty = use kubectl's current/default
	// context (unchanged for single-cluster users).
	Context string

	// Targets, when non-empty, scopes the apply to the named
	// applications (service / frontend names). The whole env bundle is
	// still rendered (KCL renders the env as a unit), but after
	// RenderManifests and before KubectlApply the multi-doc YAML is
	// filtered: a workload manifest is kept when its
	// `app.kubernetes.io/name` label is in Targets, and a shared/infra
	// manifest (no `app.kubernetes.io/name` label — Namespace, the
	// shared ConfigMap/Secret, RuntimeClass, etc.) is always kept so a
	// targeted app's dependencies aren't dropped. Empty means "apply the
	// whole bundle", the unchanged default. See FilterManifestsByApp.
	Targets []string

	// ClusterScope, when non-nil, scopes the rendered env bundle to ONE
	// deploy group's cluster before applying — declared-cluster-only
	// multi-cluster routing. KCL renders the whole env as a unit (every
	// service's manifests in one stream), but each manifest must land ONLY
	// on the cluster of its OWNING service (identified by its
	// `app.kubernetes.io/name` label), and no other cluster may receive it.
	// Without this, every group applied the entire bundle to its own
	// `--context`, so a two-cluster env cross-contaminated both clusters
	// (the secondary got the whole stack and hard-failed on missing CRDs).
	// ScopeManifestsToGroup does the per-doc partition by owner; nil leaves
	// the stream untouched (the single-cluster path, byte-identical to the
	// pre-scoping behaviour). See ScopeManifestsToGroup for the ownership
	// rule.
	ClusterScope *GroupScope
}

// GroupScope describes how to filter the env's rendered manifest stream
// down to ONE deploy group's cluster. It is the input to
// ScopeManifestsToGroup, applied by Apply before the kubectl apply when
// ApplyOpts.ClusterScope is set.
//
// Routing is DECLARED-CLUSTER-ONLY: there is no "primary cluster". A
// manifest lands on the cluster of its OWNING service, identified by the
// service's `app.kubernetes.io/name` label, which forge stamps on every
// workload AND on every per-service owned manifest (forge.Service.manifests).
// See ScopeManifestsToGroup for the precise per-document keep/drop rule.
type GroupScope struct {
	// Cluster is THIS group's cluster name (forge.K8sCluster.cluster). It
	// is the value a manifest's first-class `forge.dev/cluster` routing
	// label is matched against: a manifest carrying that label lands on
	// this group iff the label equals Cluster, and is dropped otherwise —
	// no app-label indirection. Empty disables the first-class match (the
	// stream is routed purely by OwnApps/OtherApps, the pre-existing
	// behaviour). See clusterRoutingLabel and ScopeManifestsToGroup.
	Cluster string

	// OwnApps is the set of `app.kubernetes.io/name` values belonging to
	// THIS group's services — their workloads (Deployment / Service /
	// per-service RBAC / HPA) AND the raw manifests those services own (a
	// CRD, env-level infra pinned to this cluster via an image-less infra
	// service). All carry the service's app label. These land on this
	// group's cluster.
	OwnApps map[string]struct{}

	// OtherApps is the set of app-name labels owned by OTHER k8s groups —
	// services that target a DIFFERENT cluster. These are dropped from this
	// group so a manifest never lands on a cluster its owner doesn't declare.
	OtherApps map[string]struct{}
}

// appNameLabel is the per-service workload label forge's KCL renderer
// stamps on every Deployment / Service / RBAC object via
// `_managed_labels(<svc-name>)` (kcl/lib/services.k, kcl/lib/rbac.k).
// Shared / infra manifests (Namespace, the shared ConfigMap/Secret,
// RuntimeClass) deliberately omit it — they carry only
// `app.kubernetes.io/managed-by` and `app.kubernetes.io/part-of`. That
// asymmetry is what lets FilterManifestsByApp tell a targeted app's
// workloads from another app's workloads while keeping shared deps.
const appNameLabel = "app.kubernetes.io/name"

// clusterRoutingLabel is the FIRST-CLASS per-manifest cluster-attribution
// key. forge's KCL gateway/route builders stamp it
// (`forge.dev/cluster: k3d-<name>`) when an ingress entity (Gateway /
// HTTPRoute / GRPCRoute) declares `cluster = <forge.Cluster>` — the
// builder denormalizes the referenced Cluster's kubectl CONTEXT
// (`k3d-<name>`) into the label, which is exactly the value
// GroupScope.Cluster (== forge.K8sCluster.cluster, the kubectl context)
// is matched against. ScopeManifestsToGroup reads it directly: a manifest
// carrying this label routes to the named cluster ONLY, no
// `app.kubernetes.io/name` indirection. It is the
// replacement for the older label-piggyback trick (stamping an unrelated
// service's app label so the manifest rode that service's group routing).
// A manifest WITHOUT this label still routes by app label exactly as
// before, so existing consumers are unaffected.
const clusterRoutingLabel = "forge.dev/cluster"

// Apply runs the render-KCL → kubectl-apply → wait-rollouts pipeline.
// It is the single entry point for the three call sites this package
// collapses. Behavior matches the pre-extraction `runDeploy` /
// `runDevClusterReload` shapes exactly (including stdout framing,
// warning messages, and ordering); per-call differences are expressed
// through ApplyOpts fields.
func Apply(ctx context.Context, opts ApplyOpts) error {
	manifests, err := RenderManifests(ctx, opts.MainK, opts.ImageTag, opts.Namespace, opts.Env, opts.EnvConfigKV)
	if err != nil {
		// Reload's pre-extraction form used the shorter "KCL render:"
		// wrap; the framed deploy/up path used the longer message.
		if opts.Quiet {
			return fmt.Errorf("KCL render: %w", err)
		}
		return fmt.Errorf("KCL manifest generation failed: %w", err)
	}

	// Application-level filter: when --target scoped the deploy to one
	// or more named apps, drop the other apps' workload manifests while
	// keeping every shared/infra manifest. Applied to the rendered
	// bundle (which KCL produces as a unit) so a single targeted app
	// still lands with its Namespace, shared ConfigMap/Secret, etc.
	if len(opts.Targets) > 0 {
		filtered, ferr := FilterManifestsByApp(manifests, opts.Targets)
		if ferr != nil {
			return ferr
		}
		manifests = filtered
	}

	// Multi-cluster scope: a service whose `deploy` targets a SECOND cluster
	// must land ONLY on that cluster. KCL renders the whole env as one
	// stream, so each per-group apply scopes that stream to its own cluster
	// here — the secondary cluster gets only its services' workloads (+ its
	// Namespace), the primary gets everything else. nil = single-cluster
	// (whole bundle), unchanged. Applied AFTER the --target filter so a
	// `forge deploy <env> --target <svc>` in a multi-cluster env still scopes
	// to the right cluster.
	if opts.ClusterScope != nil {
		manifests = ScopeManifestsToGroup(manifests, *opts.ClusterScope)
	}

	if opts.DryRun {
		if opts.DryRunFramed {
			fmt.Println("\n--- Generated Manifests (dry-run) ---")
			fmt.Println(manifests)
			fmt.Println("--- End Manifests ---")
			fmt.Println("\nDry run complete. No changes applied.")
		} else {
			fmt.Println(manifests)
		}
		return nil
	}

	if !opts.Quiet {
		fmt.Println("Applying manifests...")
	}
	// Two-pass apply to close the ConfigMap/Secret ordering race: a
	// single `kubectl apply --server-side -f -` admits documents as it
	// streams them, but the apiserver can schedule a workload pod before
	// the ConfigMaps/Secrets it references are readable across every
	// apiserver cache — the pod then wedges on CreateContainerConfigError
	// / "secret not found" and the rollout wait below expires as a
	// spurious timeout (seen on a real GKE launch where the config was
	// fine). Splitting the stream so the config kinds (Namespace,
	// ConfigMap, Secret) are applied AND returned BEFORE the workloads
	// gives a real happens-before: the second pass's pods only schedule
	// once their referenced config already exists. apply --server-side is
	// idempotent, so re-sending any doc is harmless. When the bundle has
	// no config kinds, config is empty and we fall through to a single
	// apply of rest — identical to the pre-split behaviour.
	config, rest := PartitionConfigManifests(manifests)
	if strings.TrimSpace(config) != "" {
		if err := KubectlApply(ctx, opts.Context, config); err != nil {
			if opts.Quiet {
				return fmt.Errorf("kubectl apply (config): %w", err)
			}
			return fmt.Errorf("kubectl apply failed (config): %w", err)
		}
	}
	if err := KubectlApply(ctx, opts.Context, rest); err != nil {
		// Reload uses the shorter "kubectl apply:" wrap; the framed
		// deploy/up path uses the longer "kubectl apply failed:" form.
		if opts.Quiet {
			return fmt.Errorf("kubectl apply: %w", err)
		}
		return fmt.Errorf("kubectl apply failed: %w", err)
	}

	if opts.Prune {
		if err := Prune(ctx, opts.Context, manifests, opts.Namespace); err != nil {
			fmt.Printf("Warning: prune: %v\n", err)
		}
	}

	if !opts.Quiet {
		fmt.Println("Waiting for rollouts...")
	}
	deployments, lerr := ListManagedDeployments(ctx, opts.Context, opts.Namespace)
	if lerr != nil {
		// Reload's pre-extraction form printed "Warning: ..." (no
		// leading indent) and short-circuited the rest of the wait
		// loop with `return nil`. The framed path indents the warning
		// and continues to the (now-empty) rollout loop.
		if opts.Quiet {
			fmt.Printf("Warning: list deployments: %v\n", lerr)
			return nil
		}
		fmt.Printf("  Warning: list deployments: %v\n", lerr)
	} else {
		var skipped []string
		for _, dep := range deployments {
			if _, skip := opts.HostSkip[dep]; skip {
				skipped = append(skipped, dep)
				continue
			}
			if err := WaitRollout(ctx, opts.Context, dep, opts.Namespace); err != nil {
				fmt.Printf("  Warning: rollout for %s: %v\n", dep, err)
			} else {
				fmt.Printf("  %s: ready\n", dep)
			}
		}
		if len(skipped) > 0 {
			fmt.Printf("Skipped rollout wait for %d host-mode service(s): %s\n",
				len(skipped), strings.Join(skipped, ", "))
		}
	}

	// Wait set = caller-supplied OneShotJobs UNION every `kind: Job` in
	// the rendered stream. Deriving from the applied manifests is the
	// authoritative path (see RenderedJobNames) and is what makes the
	// schedule=="" migrate-Job wait reliable even when the entity-list
	// derivation comes back empty; OneShotJobs is still honoured so a
	// caller can request a wait on a Job name not present in this
	// stream. De-duped, with the caller's order preserved first.
	for _, name := range unionJobNames(opts.OneShotJobs, RenderedJobNames(manifests)) {
		fmt.Printf("Waiting for one-shot Job %q to complete...\n", name)
		if err := WaitJobComplete(ctx, opts.Context, name, opts.Namespace); err != nil {
			fmt.Printf("  Warning: job %s: %v\n", name, err)
		} else {
			fmt.Printf("  %s: complete\n", name)
		}
	}

	return nil
}

// unionJobNames merges the caller-supplied one-shot Job names with the
// names derived from the rendered manifests, de-duping while keeping the
// caller's entries first (stable, predictable wait order). Used by Apply
// so the manifest-derived wait set augments rather than replaces an
// explicit OneShotJobs request.
func unionJobNames(supplied, rendered []string) []string {
	seen := make(map[string]struct{}, len(supplied)+len(rendered))
	out := make([]string, 0, len(supplied)+len(rendered))
	for _, n := range supplied {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, n := range rendered {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// RenderManifests shells `kcl run <mainK> -D image_tag=<tag>
// -D namespace=<ns> [-D env=<env>] [-D <key>=<val>]...` and returns
// the rendered `manifests:` list as a `---`-separated YAML document
// stream (the shape `kubectl apply -f -` consumes).
//
// KCL emits the program's top-level variables wrapped in a YAML
// object, so we unwrap the canonical `manifests` key. All other
// top-level KCL vars MUST be declared private (underscore-prefix) or
// they'll be dropped with a warning to stderr — only `manifests` is
// part of the contract.
//
// env (when non-empty) is passed as `-D env=<env>` so main.k can do
// `option("env")` and conditionally include manifests per-env.
// projectRootFromMainK recovers the project root from a
// `<root>/deploy/kcl/<env>/main.k` path by stripping the four trailing
// components. Returns "" for a path that doesn't match that shape (a
// relative or unexpected mainK) so the caller leaves cmd.Dir unset and
// inherits the current cwd (which forge runs from the project root).
func projectRootFromMainK(mainK string) string {
	if mainK == "" || !filepath.IsAbs(mainK) {
		return ""
	}
	// main.k -> <env> -> kcl -> deploy -> root
	dir := filepath.Dir(mainK) // <root>/deploy/kcl/<env>
	if filepath.Base(filepath.Dir(dir)) != "kcl" {
		return ""
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(dir))) // <root>
}

// renderDArgs builds the `-D key=value` top-level bindings for the KCL
// manifest render. The forge-controlled string args (image_tag, namespace,
// env) are passed as QUOTED KCL string literals via strconv.Quote so KCL
// types them as `str` — without the quotes an all-digit value like a
// numeric git-describe tag ("3826648") is coerced to `int`, which fails
// RenderEnv.image_tag's `str` type (the forge-deploy-prod regression).
// envCfgKV values are left unquoted: they are project config whose KCL
// type (int/bool/str) is intentional.
func renderDArgs(imageTag, namespace, env string, envCfgKV map[string]string) []string {
	dArgs := []string{
		"image_tag=" + strconv.Quote(imageTag),
		"namespace=" + strconv.Quote(namespace),
	}
	if env != "" {
		dArgs = append(dArgs, "env="+strconv.Quote(env))
	}
	keys := make([]string, 0, len(envCfgKV))
	for k := range envCfgKV {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		dArgs = append(dArgs, k+"="+envCfgKV[k])
	}
	return dArgs
}

func RenderManifests(_ context.Context, mainK, imageTag, namespace, env string, envCfgKV map[string]string) (string, error) {
	dArgs := renderDArgs(imageTag, namespace, env, envCfgKV)
	// Render from the project root so the deploy-as-data main.k's
	// `file.read("deploy/kcl/components_gen.json")` resolves. mainK is
	// `<root>/deploy/kcl/<env>/main.k`; strip the four trailing path
	// components to recover the project root. KCL's `file.read` is
	// cwd-relative, so the cwd is part of the contract. Empty (a relative
	// mainK) falls back to the process cwd, matching the old behaviour.
	workDir := projectRootFromMainK(mainK)
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		}
	}
	out, err := kclrender.Run(workDir, mainK, dArgs)
	if err != nil {
		return "", err
	}
	return extractManifests(out)
}

// extractManifests pulls the `manifests` list out of KCL's YAML output
// and emits each item as its own YAML document, separated by `---`.
// See RenderManifests for the contract on top-level KCL vars.
//
// The canonical generated `main.k` exports TWO top-level vars:
//
//   - `manifests` — the YAML manifest list we consume here.
//   - `output`    — the JSON contract `forge build/run/deploy` consume via
//     a separate `kcl run --format json` invocation through
//     [internal/cli.RenderKCL].
//
// Both are part of the documented dual-output contract, so `output` is
// silently skipped here rather than warned about — emitting a warning
// on every `forge deploy` / `forge up` for a sibling that the forge
// pipeline itself produces just trains users to ignore warnings. Any
// OTHER unexpected top-level var still warns.
func extractManifests(kclOutput []byte) (string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(kclOutput, &doc); err != nil {
		return "", fmt.Errorf("parse kcl output: %w", err)
	}
	raw, ok := doc["manifests"]
	if !ok {
		return "", fmt.Errorf("kcl output has no top-level `manifests` key; main.k must end with `manifests = forge.render_manifests(...)` and other top-level vars (besides `output`) must be private (underscore-prefix)")
	}
	items, ok := raw.([]any)
	if !ok {
		return "", fmt.Errorf("`manifests` is not a list (got %T)", raw)
	}
	for k := range doc {
		// `manifests` is what we consume; `output` is the documented
		// sibling for the JSON-contract pipeline.
		if k == "manifests" || k == "output" {
			continue
		}
		fmt.Fprintf(os.Stderr, "warning: ignoring extra top-level KCL var %q (mark as private with `_%s = ...` to suppress)\n", k, k)
	}

	var sb strings.Builder
	for i, it := range items {
		if i > 0 {
			sb.WriteString("---\n")
		}
		b, err := yaml.Marshal(it)
		if err != nil {
			return "", fmt.Errorf("marshal manifest item %d: %w", i, err)
		}
		sb.Write(b)
	}
	return sb.String(), nil
}

// KubectlArgs prepends `--context <kctx>` to a kubectl argument list
// when kctx is non-empty, and returns the args unchanged otherwise.
// Threading the context PER COMMAND (rather than mutating the global
// active context via `kubectl config use-context`) is what makes
// concurrent multi-cluster `forge deploy` safe — two deploys sharing
// one kubeconfig can target different clusters without racing on the
// single global context. An empty kctx means "use kubectl's
// current/default context", the unchanged single-cluster behaviour.
//
// `--context` is a global kubectl flag, so it's valid as the leading
// argument before any subcommand (apply / wait / rollout / get / …).
func KubectlArgs(kctx string, args ...string) []string {
	if kctx == "" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "--context", kctx)
	return append(out, args...)
}

// kubectlCmd builds an *exec.Cmd for `kubectl <args>` with the context
// flag threaded in via KubectlArgs. The single construction point keeps
// the per-command `--context` invariant in one place (and testable).
func kubectlCmd(ctx context.Context, kctx string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "kubectl", KubectlArgs(kctx, args...)...)
}

// KubectlApply pipes the rendered YAML document stream into
// `kubectl [--context <kctx>] apply --server-side --force-conflicts -f -`.
// Stdout/stderr are inherited so the user sees the per-resource
// `created`/`configured`/`unchanged` lines kubectl emits. kctx (when
// non-empty) targets a specific kubectl context for this command only.
//
// --force-conflicts is unconditional and deliberate: forge is the
// declarative source of truth, so its Server-Side Apply field manager
// always wins. Without it, any resource previously touched by a plain
// `kubectl apply` (manager `kubectl-client-side-apply`, common after
// manual debugging or an older bootstrap) makes SSA abort the whole
// deploy with "Apply failed with N conflicts ... conflicts with
// kubectl-client-side-apply" / `exit status 1`. Forcing forge to take
// ownership of those fields overrides the stale manager and keeps the
// deploy idempotent. (--force-conflicts is an SSA-only flag — it has no
// effect without --server-side, which we always pass.)
//
// An empty kctx is a HARD ERROR, never a fall-through to kubectl's
// current/default context. The target cluster is declarative —
// forge.K8sCluster.cluster in the env's KCL IS the context — so an empty
// value here means some group failed to carry its declared cluster. Applying
// to whatever context happens to be active is the footgun where an unrelated
// tool (e.g. `k3d cluster create`, which silently flips current-context) makes
// a deploy land in the WRONG cluster. Writes must fail LOUDLY instead. (Reads
// /waits via KubectlArgs may still default; only the destructive apply is
// gated here.)
func KubectlApply(ctx context.Context, kctx, manifests string) error {
	if strings.TrimSpace(kctx) == "" {
		return fmt.Errorf("refusing to apply manifests without an explicit kubectl context: " +
			"the target cluster is declarative (forge.K8sCluster.cluster in the env's KCL) — " +
			"forge never falls back to the current context for a write")
	}
	return applyWithImmutableRecovery(
		manifests,
		func() (string, error) { return applyOnce(ctx, kctx, manifests) },
		func(t immutableTarget) error { return kubectlDeleteResource(ctx, kctx, t) },
	)
}

// applyWithImmutableRecovery runs apply and, on the recoverable
// immutable-field failure only, deletes the one offending resource and
// re-applies once. The kubectl-touching steps are passed as closures so
// the sequencing (apply → detect immutable → scoped delete → single
// re-apply) is unit-testable without a live cluster; KubectlApply wires
// the real applyOnce / kubectlDeleteResource.
//
// A k8s Job's spec.template is immutable, so a warm re-deploy whose image
// tag changed (every commit does) makes the migrate Job's apply fail
// "is invalid: ... field is immutable" even though cold (fresh namespace)
// works. Any failure that ISN'T the immutable-field case — and a re-apply
// that still fails — surfaces the original error unchanged.
func applyWithImmutableRecovery(
	manifests string,
	apply func() (string, error),
	del func(immutableTarget) error,
) error {
	stderr, err := apply()
	if err == nil {
		return nil
	}
	if res, ok := immutableResource(stderr, manifests); ok {
		if delErr := del(res); delErr == nil {
			if _, reErr := apply(); reErr == nil {
				return nil
			}
		}
	}
	return err
}

// applyOnce runs a single `kubectl [--context] apply --server-side
// --force-conflicts -f -` over the manifest stream. Stdout is inherited
// so the user still sees the per-resource created/configured/unchanged
// lines; stderr is tee'd to os.Stderr (so errors stay visible) AND
// captured so KubectlApply can inspect it for the immutable-field
// recovery. The captured stderr is returned alongside the run error.
func applyOnce(ctx context.Context, kctx, manifests string) (string, error) {
	cmd := kubectlCmd(ctx, kctx, "apply", "--server-side", "--force-conflicts", "-f", "-")
	cmd.Stdin = strings.NewReader(manifests)
	cmd.Stdout = os.Stdout
	var buf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	err := cmd.Run()
	return buf.String(), err
}

// immutableTarget identifies the single resource an immutable-field apply
// failure was about, so the recovery can scope its delete to exactly that
// object (never a wipe).
type immutableTarget struct {
	Kind      string
	Name      string
	Namespace string // empty = cluster-scoped or unknown; delete omits -n
}

// immutableResource decides whether an apply failure is the recoverable
// immutable-field case and, if so, which resource to reset. It keys off
// the error text k8s emits for an immutable update —
//
//	The Job "control-plane-migrate" is invalid: spec.template: Invalid
//	value: ...: field is immutable
//
// requiring BOTH the `is invalid:` framing and `field is immutable` so it
// never fires on an unrelated apply error. The kind+name come from that
// message (generic — Job is the motivating case, but any immutable kind
// matches); the namespace is recovered by matching that kind+name back to
// its source document in the applied manifests. Returns ok=false when the
// stderr isn't an immutable-field error or the named resource can't be
// found in the bundle.
func immutableResource(stderr, manifests string) (immutableTarget, bool) {
	if !strings.Contains(stderr, "is invalid:") || !strings.Contains(stderr, "field is immutable") {
		return immutableTarget{}, false
	}
	kind, name, ok := parseInvalidResource(stderr)
	if !ok {
		return immutableTarget{}, false
	}
	ns := namespaceForResource(manifests, kind, name)
	return immutableTarget{Kind: kind, Name: name, Namespace: ns}, true
}

// parseInvalidResource pulls the Kind and name out of a k8s
// `The <Kind> "<name>" is invalid:` message. It scans for the `"…"`
// quoted name and takes the single capitalized token immediately before
// the opening quote as the Kind. Returns ok=false if the shape doesn't
// match.
func parseInvalidResource(stderr string) (kind, name string, ok bool) {
	const inv = " is invalid:"
	idx := strings.Index(stderr, inv)
	if idx < 0 {
		return "", "", false
	}
	head := stderr[:idx] // e.g. `The Job "control-plane-migrate"`
	close := strings.LastIndex(head, `"`)
	if close < 0 {
		return "", "", false
	}
	open := strings.LastIndex(head[:close], `"`)
	if open < 0 {
		return "", "", false
	}
	name = head[open+1 : close]
	// Kind is the last whitespace-delimited token before the opening quote.
	fields := strings.Fields(strings.TrimSpace(head[:open]))
	if len(fields) == 0 || name == "" {
		return "", "", false
	}
	kind = fields[len(fields)-1]
	return kind, name, true
}

// namespaceForResource finds the metadata.namespace of the manifest
// document whose kind+name match, so the recovery delete can target the
// right namespace. Returns "" when no doc matches or the doc carries no
// namespace (cluster-scoped, or namespace defaulted at apply time).
func namespaceForResource(manifests, kind, name string) string {
	for _, doc := range splitDocs(manifests) {
		var m struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			continue
		}
		if m.Kind == kind && m.Metadata.Name == name {
			return m.Metadata.Namespace
		}
	}
	return ""
}

// kubectlDeleteResource issues a scoped, idempotent
// `kubectl delete <kind> <name> [-n <ns>] --ignore-not-found=true` for the
// one resource an immutable apply failed on. --ignore-not-found keeps it
// safe if the object vanished between apply and delete.
func kubectlDeleteResource(ctx context.Context, kctx string, t immutableTarget) error {
	args := []string{"delete", strings.ToLower(t.Kind), t.Name, "--ignore-not-found=true"}
	if t.Namespace != "" {
		args = append(args, "-n", t.Namespace)
	}
	cmd := kubectlCmd(ctx, kctx, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// EnsureNamespace idempotently creates the target namespace so resources
// scoped to it (e.g. dotenv-projected Secrets) can be applied BEFORE the
// main manifest stream — which is where the Namespace object itself is
// rendered. Without this, the first thing the deploy applies (the
// secret_provider Secrets) lands before the Namespace exists and fails
// "namespaces \"…\" not found". The full manifest apply later re-applies
// the Namespace with its labels (server-side apply is idempotent), so this
// early create is a pure ordering fix, not a competing owner.
//
// Uses `kubectl create --dry-run=client -o yaml | kubectl apply` so a
// pre-existing namespace is a no-op rather than an AlreadyExists error.
func EnsureNamespace(ctx context.Context, kctx, namespace string) error {
	if strings.TrimSpace(kctx) == "" {
		return fmt.Errorf("refusing to create a namespace without an explicit kubectl context")
	}
	if strings.TrimSpace(namespace) == "" {
		return nil
	}
	manifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + namespace + "\n"
	return KubectlApply(ctx, kctx, manifest)
}

// WaitRollout blocks until the named Deployment reaches a healthy
// rollout state, with a 60s timeout (down from 120s — dev iteration
// is the dominant path, and a failing rollout almost always means
// the image won't pull / the pod won't start, not that 120s of patience
// would have rescued it).
//
// On timeout, automatically dumps a short diagnostic burst so the
// developer doesn't have to context-switch to a separate kubectl
// shell to figure out WHY it's stuck. The dump covers:
//   - The non-Ready pod's `kubectl describe` Events tail (image-pull
//     errors, scheduling failures, readiness probe failures).
//   - Recent namespace events (`kubectl get events`) so cluster-level
//     issues (admission webhooks, missing ConfigMaps, etc.) show up.
//   - Pod log tail when the pod is at least pulled, captures
//     CrashLoopBackOff reasons like "NATS_URL is required".
//
// Diagnostics are best-effort — any kubectl invocation failure is
// swallowed so the wait error itself remains the primary signal.
func WaitRollout(ctx context.Context, kctx, name, namespace string) error {
	cmd := kubectlCmd(ctx, kctx, "rollout", "status",
		"deployment/"+name,
		"-n", namespace,
		"--timeout=60s",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		diagnoseFailedRollout(ctx, kctx, name, namespace)
		return err
	}
	return nil
}

// podSelectorForDeploy builds the label selector that matches the pods
// of a forge-deployed Deployment. forge stamps workloads with
// appNameLabel (app.kubernetes.io/name), never a bare `app=` label, so
// the selector MUST reference the constant — a literal "app=" matched
// zero pods and silently produced empty rollout diagnostics.
func podSelectorForDeploy(deploy string) string {
	return appNameLabel + "=" + deploy
}

// diagnoseFailedRollout prints the most useful kubectl diagnostics for
// a Deployment that didn't reach Ready in time. Indented under a clear
// banner so the existing "Warning: rollout for X" line precedes it.
//
// Order matters: pod status first (most likely culprit: ImagePullBackOff,
// CrashLoopBackOff), then events (admission / scheduling failures),
// then log tail (only when the container actually started — catches
// "missing env var" startup crashes).
func diagnoseFailedRollout(ctx context.Context, kctx, deploy, namespace string) {
	fmt.Printf("\n  ── Diagnostics for %s ──────────────────\n", deploy)

	// Pod status — phase, reason, message. The most useful single line.
	pods := kubectlCmd(ctx, kctx, "get", "pods",
		"-n", namespace,
		"-l", podSelectorForDeploy(deploy),
		"-o", "custom-columns=NAME:.metadata.name,STATUS:.status.phase,REASON:.status.containerStatuses[*].state.waiting.reason,MESSAGE:.status.containerStatuses[*].state.waiting.message",
		"--no-headers",
	)
	if out, err := pods.Output(); err == nil && len(out) > 0 {
		fmt.Println("  Pods:")
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Println("    " + line)
		}
	}

	// Recent events for the deployment. Filtered to events that mention
	// the deploy name keeps the output bounded.
	events := kubectlCmd(ctx, kctx, "get", "events",
		"-n", namespace,
		"--sort-by=.lastTimestamp",
		"--field-selector=type!=Normal",
		"-o", "custom-columns=LAST:.lastTimestamp,TYPE:.type,REASON:.reason,OBJECT:.involvedObject.name,MESSAGE:.message",
		"--no-headers",
	)
	if out, err := events.Output(); err == nil && len(out) > 0 {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		// Keep the 5 most recent matching this deployment name.
		var kept []string
		for i := len(lines) - 1; i >= 0 && len(kept) < 5; i-- {
			if strings.Contains(lines[i], deploy) {
				kept = append([]string{lines[i]}, kept...)
			}
		}
		if len(kept) > 0 {
			fmt.Println("  Recent warning/error events:")
			for _, line := range kept {
				fmt.Println("    " + line)
			}
		}
	}

	// Log tail — best effort. When the pod never pulled (ImagePullBackOff)
	// kubectl returns an error which we swallow; when it crashed at
	// startup (CrashLoopBackOff) this surfaces the panic / "X is required"
	// line that explains why.
	logs := kubectlCmd(ctx, kctx, "logs",
		"deployment/"+deploy,
		"-n", namespace,
		"--tail=15",
		"--all-containers=true",
	)
	if out, err := logs.Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		fmt.Println("  Log tail (last 15 lines):")
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Println("    " + line)
		}
	}

	fmt.Println("  ─────────────────────────────────────────────")
}

// WaitJobComplete blocks until the named Job in namespace reaches
// `condition=complete`. Timeout is 5m — Jobs in this lane are
// deploy-time migrations / backfills, which routinely run for minutes.
func WaitJobComplete(ctx context.Context, kctx, name, namespace string) error {
	cmd := kubectlCmd(ctx, kctx, "wait",
		"--for=condition=complete",
		"job/"+name,
		"-n", namespace,
		"--timeout=5m",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ListManagedDeployments returns the names of every forge-owned
// Deployment in the namespace (filtered by the
// `app.kubernetes.io/managed-by=forge` label). This is the
// authoritative list for rollout-watching — it covers shared-binary
// `<project>-<svc>` names, per-service `<svc>` names, operator and
// worker deployments, and anything packs add, without forge having to
// guess naming schemes per scaffold mode.
func ListManagedDeployments(ctx context.Context, kctx, namespace string) ([]string, error) {
	cmd := kubectlCmd(ctx, kctx, "get", "deployments",
		"-n", namespace,
		"-l", "app.kubernetes.io/managed-by=forge",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	names := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// docDelimiter is the separator between documents in a `---`-separated
// multi-doc YAML stream. It is the single source of truth for both
// splitting (splitDocs) and re-joining manifest streams in this file.
const docDelimiter = "\n---\n"

// splitDocs splits a `---`-separated multi-doc YAML stream into its
// individual documents, trimming surrounding whitespace and dropping
// empty / whitespace-only docs. It is the single source of the
// delimiter for the manifest-scanning helpers below.
func splitDocs(manifests string) []string {
	raw := strings.Split(manifests, docDelimiter)
	docs := make([]string, 0, len(raw))
	for _, doc := range raw {
		if trimmed := strings.TrimSpace(doc); trimmed != "" {
			docs = append(docs, trimmed)
		}
	}
	return docs
}

// parsedDoc is the minimal view of a Kubernetes manifest the scanning
// helpers below need: the document's kind, its metadata.name, and its
// metadata.labels.
type parsedDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name   string            `yaml:"name"`
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
}

// parseDoc unmarshals a single YAML manifest document into a parsedDoc.
// A doc that doesn't parse returns ok=false so callers can apply their
// own conservative fallback (keep, or place in rest).
func parseDoc(doc string) (parsedDoc, bool) {
	var m parsedDoc
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		return parsedDoc{}, false
	}
	return m, true
}

// FilterManifestsByApp filters a `---`-separated multi-doc YAML stream
// down to the manifests belonging to the named apps, PLUS every shared
// manifest. The rule, doc by doc:
//
//   - KEEP when the doc's `metadata.labels[app.kubernetes.io/name]`
//     value is one of targets — that's a targeted app's workload.
//   - KEEP when the doc has NO `app.kubernetes.io/name` label — that's
//     a shared/infra resource (Namespace, the shared ConfigMap/Secret,
//     RuntimeClass, CRDs) a targeted app may depend on. Dropping these
//     would leave the app's pods unable to start.
//   - DROP when the label is present but names a NON-targeted app.
//
// Empty / whitespace-only docs are dropped. A doc that doesn't parse as
// YAML is conservatively KEPT (better to apply an opaque doc than to
// silently swallow it; kubectl will reject genuinely-broken YAML).
//
// If the filter would drop every workload-labelled doc — i.e. none of
// targets matched any app in the bundle (a typo'd --target) — it
// returns an error listing the app names actually present, rather than
// applying a shared-only bundle that does nothing the user asked for.
func FilterManifestsByApp(manifests string, targets []string) (string, error) {
	want := map[string]struct{}{}
	for _, t := range targets {
		want[t] = struct{}{}
	}

	var kept []string
	present := map[string]struct{}{} // every app name seen on a labelled doc
	matchedAny := false              // did any doc carry a targeted app label?

	for _, doc := range splitDocs(manifests) {
		app := manifestAppLabel(doc)
		if app == "" {
			// Shared / infra resource — always keep.
			kept = append(kept, doc)
			continue
		}
		present[app] = struct{}{}
		if _, ok := want[app]; ok {
			kept = append(kept, doc)
			matchedAny = true
		}
	}

	if !matchedAny {
		avail := make([]string, 0, len(present))
		for a := range present {
			avail = append(avail, a)
		}
		sort.Strings(avail)
		return "", fmt.Errorf(
			"no application matched --target %s; available apps in this env: %s",
			strings.Join(targets, ", "), strings.Join(avail, ", "))
	}

	return strings.Join(kept, docDelimiter), nil
}

// manifestAppLabel reads metadata.labels["app.kubernetes.io/name"] from
// a single YAML manifest document. Returns "" when the doc has no such
// label (shared/infra resource) or doesn't parse — callers treat "" as
// "shared, keep".
func manifestAppLabel(doc string) string {
	m, ok := parseDoc(doc)
	if !ok {
		return ""
	}
	return m.Metadata.Labels[appNameLabel]
}

// ScopeManifestsToGroup filters a `---`-separated env manifest stream down
// to the documents that belong on ONE deploy group's cluster. Routing is
// DECLARED-CLUSTER-ONLY: a manifest lands on the cluster of its OWNING
// service, identified by the service's `app.kubernetes.io/name` label.
// There is no "primary cluster" and no most-services heuristic — forge
// stamps that label on every workload AND on every per-service owned
// manifest (forge.Service.manifests, which is also how an image-less infra
// service pins env-level resources — Namespace, Gateways, CRDs — to a
// specific declared cluster). KCL still renders the whole env as one
// stream; this filter routes each doc to the cluster its owner declares.
//
// FIRST-CLASS cluster attribution takes priority over the app-label rule
// below: a manifest carrying the `forge.dev/cluster` label (stamped by
// forge's gateway/route builders when an ingress entity declares
// `cluster = "<name>"`) is routed by that label DIRECTLY — kept iff the
// label equals scope.Cluster, dropped otherwise — with no
// `app.kubernetes.io/name` indirection. This is the explicit replacement
// for the old trick of piggybacking an unrelated service's app label.
// When scope.Cluster is empty (the label-only routing path), the
// first-class match is skipped and the doc falls through to the app-label
// rule. A manifest WITHOUT the routing label always uses the app-label
// rule.
//
// The ownership rule, document by document (by its
// `app.kubernetes.io/name` label `a`):
//
//   - a ∈ scope.OwnApps   → KEEP. This group's own service or its owned
//     manifests (Deployment / Service / per-service RBAC / HPA / a CRD or
//     infra resource attached via forge.Service.manifests — all carry the
//     owning service's app label).
//   - a ∈ scope.OtherApps → DROP. Owned by a DIFFERENT cluster's group —
//     never apply it here (this is what stops cross-contamination: a
//     secondary cluster never receives the primary's services, CRDs, or
//     gateways, so it can't hard-fail on a CRD it doesn't have).
//   - a != "" but in NEITHER set → KEEP. An app-labelled doc whose owner
//     isn't in any group should not occur once every service is grouped;
//     keeping (rather than dropping or routing by guess) is the safe,
//     non-heuristic default.
//   - a == "" and kind == Namespace → KEEP. Every cluster needs its
//     namespace (a workload can't apply into a missing namespace), and the
//     namespace is genuinely env-wide, so it is replicated to every group.
//   - a == "" (any other unlabeled doc — an env-level resource the user
//     did NOT attribute to a cluster, e.g. a ConfigMap left on the global
//     bundle rather than an infra service) → KEEP. The deploy layer never
//     PICKS a cluster for an unattributed resource; it replicates the
//     genuinely-shared ones rather than guess a primary. To pin such a
//     resource to ONE cluster, declare it on an image-less infra service's
//     `manifests` so it carries that service's app label and routes via
//     OwnApps/OtherApps above.
//
// A doc that doesn't parse as YAML is conservatively KEPT (it can't be
// confirmed as another cluster's, and silently swallowing it is worse than
// letting kubectl reject genuinely-broken YAML).
//
// Empty / whitespace-only docs are dropped. The single-cluster path never
// reaches here (ApplyOpts.ClusterScope stays nil), so this is a no-op for
// the common case and multi-cluster envs are the only behaviour change.
func ScopeManifestsToGroup(manifests string, scope GroupScope) string {
	var kept []string
	for _, doc := range splitDocs(manifests) {
		m, parsed := parseDoc(doc)
		app := ""
		routeCluster := ""
		if parsed {
			app = m.Metadata.Labels[appNameLabel]
			routeCluster = m.Metadata.Labels[clusterRoutingLabel]
		}
		// First-class cluster attribution wins over the app-label rule: a
		// manifest stamped with `forge.dev/cluster` routes to exactly that
		// cluster. Only engaged when this group knows its own cluster name
		// (scope.Cluster); otherwise fall through to app-label routing.
		if routeCluster != "" && scope.Cluster != "" {
			if routeCluster == scope.Cluster {
				kept = append(kept, doc)
			}
			// routeCluster != scope.Cluster → drop (another cluster's).
			continue
		}
		switch {
		case app != "":
			if _, other := scope.OtherApps[app]; other {
				continue // another cluster's owner — drop
			}
			// Owned by this group, OR app-labelled but ungrouped: keep.
			// (OwnApps membership and the ungrouped case both keep; only an
			// explicit OtherApps match drops.)
			kept = append(kept, doc)
		case !parsed:
			// Unparseable: keep (can't be confirmed another cluster's).
			kept = append(kept, doc)
		default:
			// Unlabeled (Namespace or any other env-level resource the user
			// didn't attribute to a cluster): replicate to every group. The
			// deploy layer never guesses a primary for an unattributed doc.
			kept = append(kept, doc)
		}
	}
	return strings.Join(kept, docDelimiter)
}

// RenderedDeploymentNames extracts the `metadata.name` of every
// `kind: Deployment` document in a `---`-separated YAML stream. Used by
// Prune to compute the desired set against which the namespace's
// actual forge-managed Deployments are diffed.
//
// Malformed documents are skipped (callers get a best-effort list).
func RenderedDeploymentNames(manifests string) []string {
	return renderedNamesByKind(manifests, "Deployment")
}

// renderedNamesByKind extracts the `metadata.name` of every document
// whose `kind` matches kind in a `---`-separated YAML stream. Malformed
// documents are skipped (callers get a best-effort list). It backs both
// RenderedDeploymentNames and RenderedJobNames, which differ only by the
// kind they select.
func renderedNamesByKind(manifests, kind string) []string {
	var out []string
	for _, doc := range splitDocs(manifests) {
		m, ok := parseDoc(doc)
		if !ok {
			continue
		}
		if m.Kind == kind && m.Metadata.Name != "" {
			out = append(out, m.Metadata.Name)
		}
	}
	return out
}

// configFirstKinds are the manifest kinds applied in the first pass of
// Apply's two-pass apply. They are the resources a workload pod can
// reference at schedule time and that must therefore exist (and be
// readable) before the workload lands: the Namespace the workload is
// created in, and the ConfigMaps / Secrets its containers mount or
// project into env. Cluster-scoped config (RuntimeClass, CRDs) is left
// in the second pass — those are already ordered ahead of workloads
// within the rendered stream and don't suffer the same apiserver-cache
// readability race that a freshly-applied namespaced Secret does.
var configFirstKinds = map[string]struct{}{
	"Namespace": {},
	"ConfigMap": {},
	"Secret":    {},
}

// PartitionConfigManifests splits a `---`-separated multi-doc YAML
// stream into (config, rest): config holds the documents whose `kind`
// is one of configFirstKinds (Namespace, ConfigMap, Secret) in their
// original relative order, rest holds everything else (also in order).
// Empty / whitespace-only docs are dropped from both halves. A doc that
// doesn't parse as YAML is conservatively placed in rest — it can't be
// confirmed config, and rest is the pass that always runs.
//
// This is the ordering primitive behind Apply's two-pass apply: config
// is applied (and kubectl returns) before rest, so a workload in rest
// never schedules ahead of the ConfigMap/Secret it references.
func PartitionConfigManifests(manifests string) (config, rest string) {
	var cfg, other []string
	for _, doc := range splitDocs(manifests) {
		m, ok := parseDoc(doc)
		if !ok {
			// Can't be confirmed config — rest is the pass that always runs.
			other = append(other, doc)
			continue
		}
		if _, ok := configFirstKinds[m.Kind]; ok {
			cfg = append(cfg, doc)
		} else {
			other = append(other, doc)
		}
	}
	return strings.Join(cfg, docDelimiter), strings.Join(other, docDelimiter)
}

// RenderedJobNames extracts the `metadata.name` of every `kind: Job`
// document in a `---`-separated YAML stream. This is the authoritative
// source for the one-shot-Job wait set: forge waits on whatever Jobs
// the deploy actually applies, regardless of how they entered the
// bundle.
//
// The entity-list derivation (oneShotJobNamesFromKCL, reading
// KCLEntities.CronJobs) is fragile — it only sees Jobs that round-trip
// through the typed `forge.CronJob` -> `output.cronjobs` contract, and
// misses a `schedule==""` Job that didn't surface in that list (the
// real-launch gap where OneShotJobs came back empty and forge rolled
// the workloads without blocking on the migrate Job) as well as any raw
// `kind: Job` added via `additional_manifests`. The rendered manifests
// are what kubectl actually applies, so deriving the wait set from them
// closes both holes. Apply unions these with any caller-supplied
// OneShotJobs and de-dupes.
//
// Malformed documents are skipped (callers get a best-effort list).
func RenderedJobNames(manifests string) []string {
	return renderedNamesByKind(manifests, "Job")
}

// Prune deletes every forge-managed Deployment in namespace that is
// NOT in the rendered manifest stream. The managed-by guard comes
// from the kubectl label filter inside ListManagedDeployments — only
// resources carrying `app.kubernetes.io/managed-by=forge` are eligible
// for prune. This invariant protects user-applied Deployments living
// alongside forge-owned ones in the same namespace.
//
// An empty desired set (no Deployments at all in the render) is
// treated as a misuse case (almost certainly the user pointed at the
// wrong env dir) and prune is skipped rather than wiping every
// forge-managed Deployment in the namespace.
//
// Errors during the list or per-Deployment delete are returned to the
// caller (which logs them as warnings rather than failing the whole
// deploy — pruning is a maintenance step, not a correctness gate).
func Prune(ctx context.Context, kctx, manifests, namespace string) error {
	desired := map[string]struct{}{}
	for _, n := range RenderedDeploymentNames(manifests) {
		desired[n] = struct{}{}
	}
	if len(desired) == 0 {
		fmt.Println("Skipping prune (no Deployments in render).")
		return nil
	}
	current, err := ListManagedDeployments(ctx, kctx, namespace)
	if err != nil {
		return fmt.Errorf("list deployments: %w", err)
	}
	var orphans []string
	for _, dep := range current {
		if _, keep := desired[dep]; !keep {
			orphans = append(orphans, dep)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	fmt.Printf("Pruning %d orphan Deployment(s) in %s: %s\n",
		len(orphans), namespace, strings.Join(orphans, ", "))
	for _, name := range orphans {
		delCmd := kubectlCmd(ctx, kctx, "delete", "deployment", name,
			"-n", namespace, "--ignore-not-found=true")
		delCmd.Stdout = os.Stdout
		delCmd.Stderr = os.Stderr
		if err := delCmd.Run(); err != nil {
			fmt.Printf("  Warning: delete %s: %v\n", name, err)
		}
	}
	return nil
}
