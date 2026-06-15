// Package cluster owns the render-KCL → kubectl-apply → wait-rollouts
// pipeline that `forge deploy`, `forge cluster reload`, and the
// deploy phase of `forge up` all execute. Before this package existed,
// the pipeline was duplicated across three call sites:
//
//   - runDeploy           (internal/cli/deploy.go)
//   - runDevClusterReload (internal/cli/dev_cluster.go)
//   - upDeployCluster     (internal/cli/up.go)
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
	"context"
	"fmt"
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

	// OneShotJobs is the list of Job names rendered from CronJobs with
	// empty Schedule. Each is waited on with `kubectl wait
	// --for=condition=complete` so the caller gets a definitive
	// done/fail signal before Apply returns. Scheduled CronJobs are
	// NOT waited on — they run on their own cadence and the deploy is
	// done as soon as the manifest is applied.
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
	if err := KubectlApply(ctx, manifests); err != nil {
		// Reload uses the shorter "kubectl apply:" wrap; the framed
		// deploy/up path uses the longer "kubectl apply failed:" form.
		if opts.Quiet {
			return fmt.Errorf("kubectl apply: %w", err)
		}
		return fmt.Errorf("kubectl apply failed: %w", err)
	}

	if opts.Prune {
		if err := Prune(ctx, manifests, opts.Namespace); err != nil {
			fmt.Printf("Warning: prune: %v\n", err)
		}
	}

	if !opts.Quiet {
		fmt.Println("Waiting for rollouts...")
	}
	deployments, lerr := ListManagedDeployments(ctx, opts.Namespace)
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
			if err := WaitRollout(ctx, dep, opts.Namespace); err != nil {
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

	for _, name := range opts.OneShotJobs {
		fmt.Printf("Waiting for one-shot Job %q to complete...\n", name)
		if err := WaitJobComplete(ctx, name, opts.Namespace); err != nil {
			fmt.Printf("  Warning: job %s: %v\n", name, err)
		} else {
			fmt.Printf("  %s: complete\n", name)
		}
	}

	return nil
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
	dir := filepath.Dir(mainK)              // <root>/deploy/kcl/<env>
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

// KubectlApply pipes the rendered YAML document stream into
// `kubectl apply --server-side -f -`. Stdout/stderr are inherited so
// the user sees the per-resource `created`/`configured`/`unchanged`
// lines kubectl emits.
func KubectlApply(ctx context.Context, manifests string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "--server-side", "-f", "-")
	cmd.Stdin = strings.NewReader(manifests)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
// Diagnostics are best-effort — any kubectl invocation failure is
// swallowed so the wait error itself remains the primary signal.
func WaitRollout(ctx context.Context, name, namespace string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "rollout", "status",
		"deployment/"+name,
		"-n", namespace,
		"--timeout=60s",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		diagnoseFailedRollout(ctx, name, namespace)
		return err
	}
	return nil
}

// diagnoseFailedRollout prints the most useful kubectl diagnostics for
// a Deployment that didn't reach Ready in time. Indented under a clear
// banner so the existing "Warning: rollout for X" line precedes it.
//
// Order matters: pod status first (most likely culprit: ImagePullBackOff,
// CrashLoopBackOff), then events (admission / scheduling failures),
// then log tail (only when the container actually started — catches
// "missing env var" startup crashes).
func diagnoseFailedRollout(ctx context.Context, deploy, namespace string) {
	fmt.Printf("\n  ── Diagnostics for %s ──────────────────\n", deploy)

	// Pod status — phase, reason, message. The most useful single line.
	pods := exec.CommandContext(ctx, "kubectl", "get", "pods",
		"-n", namespace,
		"-l", "app="+deploy,
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
	events := exec.CommandContext(ctx, "kubectl", "get", "events",
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
	logs := exec.CommandContext(ctx, "kubectl", "logs",
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
func WaitJobComplete(ctx context.Context, name, namespace string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "wait",
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
func ListManagedDeployments(ctx context.Context, namespace string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "deployments",
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

	docs := strings.Split(manifests, "\n---\n")
	var kept []string
	present := map[string]struct{}{} // every app name seen on a labelled doc
	matchedAny := false              // did any doc carry a targeted app label?

	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		app := manifestAppLabel(trimmed)
		if app == "" {
			// Shared / infra resource — always keep.
			kept = append(kept, trimmed)
			continue
		}
		present[app] = struct{}{}
		if _, ok := want[app]; ok {
			kept = append(kept, trimmed)
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

	return strings.Join(kept, "\n---\n"), nil
}

// manifestAppLabel reads metadata.labels["app.kubernetes.io/name"] from
// a single YAML manifest document. Returns "" when the doc has no such
// label (shared/infra resource) or doesn't parse — callers treat "" as
// "shared, keep".
func manifestAppLabel(doc string) string {
	var m struct {
		Metadata struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		return ""
	}
	return m.Metadata.Labels[appNameLabel]
}

// RenderedDeploymentNames extracts the `metadata.name` of every
// `kind: Deployment` document in a `---`-separated YAML stream. Used by
// Prune to compute the desired set against which the namespace's
// actual forge-managed Deployments are diffed.
//
// Malformed documents are skipped (callers get a best-effort list).
func RenderedDeploymentNames(manifests string) []string {
	docs := strings.Split(manifests, "\n---\n")
	var out []string
	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		var m struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(trimmed), &m); err != nil {
			continue
		}
		if m.Kind == "Deployment" && m.Metadata.Name != "" {
			out = append(out, m.Metadata.Name)
		}
	}
	return out
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
func Prune(ctx context.Context, manifests, namespace string) error {
	desired := map[string]struct{}{}
	for _, n := range RenderedDeploymentNames(manifests) {
		desired[n] = struct{}{}
	}
	if len(desired) == 0 {
		fmt.Println("Skipping prune (no Deployments in render).")
		return nil
	}
	current, err := ListManagedDeployments(ctx, namespace)
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
		delCmd := exec.CommandContext(ctx, "kubectl", "delete", "deployment", name,
			"-n", namespace, "--ignore-not-found=true")
		delCmd.Stdout = os.Stdout
		delCmd.Stderr = os.Stderr
		if err := delCmd.Run(); err != nil {
			fmt.Printf("  Warning: delete %s: %v\n", name, err)
		}
	}
	return nil
}
