package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/secrets"
	"github.com/reliant-labs/forge/internal/statefile"
)

func newDeployCmd() *cobra.Command {
	var (
		imageTag      string
		tag           string
		dryRun        bool
		namespace     string
		explain       bool
		targetArch    string
		prune         bool
		rollback      bool
		targets       []string
		skipFrontend  bool
		skipPreflight bool
		noDigest      bool
	)

	cmd := &cobra.Command{
		Use:   "deploy <environment>",
		Short: "Deploy services to the target declared in deploy/kcl/<env>/",
		Long: `Deploy each service to the target declared on its Service.deploy block.

Supported deploy targets (declared in deploy/kcl/<env>/main.k):

  * forge.K8sCluster — Kubernetes deployment via render → kubectl apply
    → wait-rollouts. Forge auto-creates a k3d cluster for dev.
  * forge.External   — generic shell-command escape hatch. Forge runs
    sh -c <deploy_cmd> with ${IMAGE}/${TAG}/${SERVICE} etc. expanded;
    use for Fly.io, Cloud Run, Cloudflare Workers, ECS, Vercel, etc.
  * forge.Compose    — docker compose pull + up -d.

forge.HostDeploy and forge.BuildOnly are skipped by deploy — those are
owned by forge run / forge up and forge build respectively.

Safety (declarative context): the kubectl context is read SOLELY from the
env's KCL — forge.K8sCluster.cluster IS the kubectl context name (e.g.
"gke_<project>_<region>_prod"; defaults to k3d-<project> for dev). Every
kubectl call in the apply/wait/prune/rollback/secrets path runs
--context <declared> per command, so the deploy applies to EXACTLY the
cluster the env declares — independent of whatever context is currently
active. There is NO CLI override and NO fall-back to the current context:
the binding lives in the env file, full stop, so you can't deploy the
wrong env to the wrong cluster. forge fails fast (even under --dry-run) if
the declared cluster has no matching kubectl context — the only remedy is
to fix your kubeconfig or the KCL forge.K8sCluster.cluster.

Use --explain to print the declared context, whether it exists in your
kubeconfig, and the verdict without applying.

Deployability preflight: before the first apply (remote/cloud clusters),
forge verifies against the LIVE target that every Secret KEY the rendered
manifests reference is provisioned and every container image: resolves in
its registry. A missing key (CreateContainerConfigError) or image
(ImagePullBackOff) is reported up front — all at once — and the deploy
refuses to apply, instead of surfacing one pod crash at a time mid-rollout.
The preflight is skipped for local dev clusters and runs under --dry-run as
a pure read-only check. Bypass with --skip-preflight.

Use --target <app> (repeatable) to deploy ONLY the named application(s)
instead of the whole env bundle. It filters by app NAME — service,
operator, or frontend: the K8sCluster apply keeps the targeted app's
workload manifests plus all shared resources (Namespace, the shared
ConfigMap/Secret, RBAC), and the External/Compose dispatch + rollout-wait
are scoped to the named apps. A typo'd target errors with the list of
available app names. Targeting an operator (e.g. workspace-controller)
applies just that operator's Deployment + cluster RBAC.

k8s-only deploy: naming only backend apps via --target is itself the
"k8s without touching the frontend" path — a Firebase frontend isn't in
the --target set, so its build+deploy step never runs. To ship the WHOLE
backend bundle while skipping the frontend (without enumerating every
service), pass --skip-frontend: the k8s apply runs as normal and the
Frontend (e.g. Firebase) build+deploy dispatch is skipped.

Examples:
  forge deploy dev                          # Deploy to dev (local k3d)
  forge deploy staging --image-tag v1.2     # Deploy to staging with specific tag
  forge deploy prod --dry-run               # Preview prod manifests (guard runs)
  forge deploy prod --explain               # Show the declared-cluster guard verdict
  forge deploy dev --namespace custom-ns    # Override namespace
  forge deploy dev --target admin-server    # Deploy only the admin-server app
  forge deploy prod --target workspace-controller # Deploy only that operator
  forge deploy prod --skip-frontend         # Deploy backend k8s, skip Firebase`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if explain {
				return runDeployExplain(cmd.Context(), args[0])
			}
			// --tag and --image-tag are interchangeable; --tag is the
			// canonical name (it matches `forge build --tag`) and
			// --image-tag is retained for backwards compat with
			// pre-converged scripts. When both are set, --tag wins.
			effectiveTag := tag
			if effectiveTag == "" {
				effectiveTag = imageTag
			}
			// --rollback is mutually exclusive with --tag/--image-tag.
			// Rollback's whole purpose is to ship the previously-
			// recorded last-good tag from .forge/state; accepting a
			// caller-supplied tag alongside it would either override
			// the recorded value (defeating the rollback) or be
			// silently ignored (worse: the user thinks they pinned a
			// tag and they didn't).
			if rollback && effectiveTag != "" {
				return errors.New("--rollback and --tag are mutually exclusive")
			}
			return runDeploy(cmd.Context(), args[0], deployOptions{
				imageTag:      effectiveTag,
				dryRun:        dryRun,
				namespace:     namespace,
				targetArch:    targetArch,
				prune:         prune,
				rollback:      rollback,
				targets:       targets,
				skipFrontend:  skipFrontend,
				skipPreflight: skipPreflight,
				noDigest:      noDigest,
			})
		},
	}

	cmd.Flags().StringVar(&imageTag, "image-tag", "", "Image tag (deprecated alias for --tag; default: build-state file, then git describe --tags --always --dirty)")
	cmd.Flags().StringVar(&tag, "tag", "", "Override the image tag (priority: --tag > .forge/state/build-<env>.json > git describe --tags --always --dirty)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print manifests without applying (env-cluster guard still runs)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Override namespace from environment config")
	cmd.Flags().BoolVar(&explain, "explain", false, "Print the declared-cluster guard decision (declared/current/verdict) and exit")
	cmd.Flags().StringVar(&targetArch, "target-arch", "", "Override target GOARCH for cross-compilation (default: forge.yaml deploy.target_arch, then amd64)")
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete forge-managed Deployments in the namespace that the current KCL render no longer produces (opt-in)")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "Roll back the env to the last successfully deployed tag (per service, from .forge/state).")
	cmd.Flags().StringArrayVar(&targets, "target", nil, "Deploy ONLY the named application(s) (service/operator/frontend name; repeatable). Scopes K8sCluster apply to the app's workload + shared resources, and External/Compose dispatch to the named apps. Empty = deploy the whole env bundle (default).")
	cmd.Flags().BoolVar(&skipFrontend, "skip-frontend", false, "Run the k8s apply but skip the Frontend (e.g. Firebase) build+deploy dispatch. The k8s-only path for the whole backend bundle without enumerating every --target.")
	cmd.Flags().BoolVar(&skipPreflight, "skip-preflight", false, "Skip the deploy preflight (verify referenced Secret keys + container images exist on the live target BEFORE applying). Default-on for remote/cloud clusters; bypass at your own risk.")
	cmd.Flags().BoolVar(&noDigest, "no-digest", false, "Deploy by the mutable :tag even when the build state captured an immutable image digest. By default forge pins the manifest to <image>@sha256:... so a re-tagged/cached layer can't ship; this escape hatch restores tag-based references.")

	return cmd
}

// runDeployExplain prints the resolved kubectl-context guard decision
// for an environment without doing anything destructive. Useful when
// debugging why `forge deploy staging` refuses to apply or what context
// staging is expected to live in.
func runDeployExplain(ctx context.Context, envName string) error {
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	cfg := store.Config()
	expected := expectedClusterForEnv(ctx, cfg, envName)
	current := strings.TrimSpace(currentKubectlContext(ctx))

	fmt.Printf("forge deploy %s — declared-cluster guard\n", envName)
	fmt.Printf("  declared context: %s\n", emptyAs(expected, "(not declared)"))
	fmt.Printf("  current context:  %s  (purely informational — NEVER used; the deploy always applies to the DECLARED context)\n", emptyAs(current, "(none — kubectl not configured)"))

	if expected == "" {
		fmt.Printf("  hint:             declare `forge.K8sCluster.cluster` in deploy/kcl/%s/main.k to enable the guard\n", envName)
		fmt.Println("  verdict: ALLOW (no cluster declared — guard skipped, current context used)")
		return printDeployExplainHostSkip(cfg, envName)
	}
	// Declarative model: the deploy applies to the declared context
	// regardless of the active one. The only failure is a declared
	// context that doesn't exist in the kubeconfig.
	available, aerr := kubectlContextNames(ctx)
	if aerr != nil {
		fmt.Printf("  fix:              %v\n", aerr)
		fmt.Println("  verdict: REFUSE (kubectl not configured)")
		return nil
	}
	if verr := declaredContextExistsVerdict(envName, expected, available); verr != nil {
		fmt.Printf("  available:        %s\n", emptyAs(strings.Join(available, ", "), "(none)"))
		fmt.Printf("  fix:              add the context to your kubeconfig, or correct forge.K8sCluster.cluster in the env's KCL\n")
		fmt.Println("  verdict: REFUSE (declared context not in kubeconfig)")
		return nil
	}
	fmt.Println("  verdict: ALLOW (declared context exists; deploy applies there regardless of current)")
	return printDeployExplainHostSkip(cfg, envName)
}

// printDeployExplainHostSkip is a placeholder for the post-orchestration
// shape: once the KCL-side `deploy: "host"` filter lands (deliverable 4)
// this helper renders the host-mode service list for `forge deploy <env>
// --explain`. Kept as a stub so the call site keeps compiling while the
// re-wire is in progress.
func printDeployExplainHostSkip(_ *config.ProjectConfig, _ string) error {
	return nil
}

// emptyAs returns alt when s is empty, otherwise s. Cheap helper for
// rendering "not declared" / "(none)" placeholders.
func emptyAs(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// deployOptions bundles the flag values for `forge deploy`. The
// runDeploy function previously took six discrete parameters; growing
// it to seven tipped revive's argument-limit lint. The struct form
// makes the call site self-documenting and keeps the per-flag default
// (e.g. dryRun=false) co-located with the field declaration.
type deployOptions struct {
	imageTag   string
	dryRun     bool
	namespace  string
	targetArch string
	// prune, when true, deletes forge-managed Deployments in the
	// namespace that the just-applied KCL render no longer produces.
	// Opt-in to start: pruning is destructive (deletes resources the
	// user didn't ask to remove) and surprising behaviour during an
	// in-progress KCL refactor would be costly to roll back. The dev
	// loop benefits most — `forge deploy dev` after a host-mode
	// refactor leaves stale Deployments behind otherwise.
	prune bool

	// rollback, when true, switches the dispatch from Deploy to
	// Rollback. Each external/compose group reads its
	// .forge/state/<provider>-<env>-<svc>.json file to find the last
	// good tag and asks the provider to revert there; k8s-cluster
	// groups invoke `kubectl rollout undo`. Missing state files
	// produce a clear per-service error rather than guessing.
	//
	// Mutually exclusive with imageTag — the deploy command rejects
	// the combination at flag-parse time.
	rollback bool

	// targets, when non-empty, scopes the deploy to the named
	// applications (service / operator / frontend names). Two layers
	// honour it: (1) entities.Services / entities.Operators /
	// entities.Frontends are filtered to the targeted names before
	// buildDeployGroups, so External/Compose dispatch and the
	// rollout-wait / host-skip sets cover only the targeted apps;
	// (2) the K8sCluster apply filters the rendered multi-doc manifest
	// stream EXCLUSIVELY to the manifests whose KCL-declared group ∈ targets
	// — nothing implicit, no shared-base auto-keep (see
	// cluster.SelectManifestsByGroup). Empty means "deploy EVERYTHING in the
	// env bundle" (the full declarative reconcile), the unchanged default.
	targets []string

	// skipFrontend, when true, runs the k8s apply but suppresses the
	// Frontend (e.g. Firebase Hosting) build+deploy dispatch. It's the
	// "k8s-only, leave the frontend alone" escape hatch for the WHOLE
	// backend bundle — naming backend apps via --target already excludes
	// frontends, but that forces enumerating every service; --skip-frontend
	// covers the deploy-everything-but-the-frontend case in one flag. No
	// effect on rollback (which never dispatches frontends).
	skipFrontend bool

	// skipPreflight, when true, bypasses the deploy-time deployability
	// preflight (verify every referenced Secret key + container image exists
	// on the LIVE target BEFORE the first apply). The preflight is
	// default-ON for remote/cloud clusters — it turns a deploy-time pod
	// crash (CreateContainerConfigError / ImagePullBackOff, discovered
	// one-at-a-time over a rollout) into one fail-fast error up front. It is
	// naturally skipped for local dev clusters (where images live in the
	// in-cluster registry the checker can't reach the same way) and no-ops
	// when there's nothing to check. Bypass is the escape hatch when you
	// knowingly accept the risk. No effect on rollback.
	skipPreflight bool

	// noDigest, when true, forces deploy to reference the mutable :tag even
	// when the build state captured an immutable content-addressed digest.
	// By default (false) forge prefers the digest — pinning the manifest to
	// <image>@sha256:... so a re-tagged / node-cached layer can never ship
	// (the structural fix for the mutable-tag/exec-format incident). The
	// escape hatch exists for the rare case a tag reference is wanted (e.g.
	// debugging a registry that mishandles digest pulls). No effect when no
	// digest was captured — the tag is used either way.
	noDigest bool
}

func runDeploy(ctx context.Context, envName string, opts deployOptions) error {
	// imageTag is the env-wide mutable tag bound to KCL's `image_tag` (the
	// per-image fallback for vendored / no-digest images and the
	// External/Compose ${TAG} source). plainTag is the same mutable tag.
	// imageDigests is the PER-IMAGE name→digest map: each forge-built image
	// resolves to ITS OWN captured digest, so the KCL render pins
	// `<image>@<digest>` per service rather than stamping one env-wide digest
	// onto every image (the multi-image correctness fix). Empty on the
	// no-digest / local-registry path → every image stays on imageTag.
	imageTag := opts.imageTag
	plainTag := opts.imageTag
	var imageDigests map[string]string
	dryRun := opts.dryRun
	namespace := opts.namespace
	targetArchFlag := opts.targetArch
	prune := opts.prune
	rollback := opts.rollback
	targets := opts.targets
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	cfg := store.Config()

	if !store.Features().DeployEnabled() {
		return config.DisabledFeatureError(config.FeatureDeploy)
	}

	// Resolve KCL directory.
	kclDir := store.K8s().KCLDir
	if kclDir == "" {
		kclDir = "deploy/kcl"
	}
	envDir := filepath.Join(kclDir, envName)
	mainK := filepath.Join(envDir, "main.k")

	// Validate environment exists.
	if _, err := os.Stat(mainK); os.IsNotExist(err) {
		return fmt.Errorf("environment %q not found: %s does not exist\nAvailable environments can be found under %s/", envName, mainK, kclDir)
	}

	projectDir := projectDirForKCL()

	// Arm the parallel-dev-stack render context BEFORE the first render:
	// push the git facts option("worktree")/option("branch") into KCL, back
	// forge.allocate_port with the lock-guarded block registry, and activate
	// the resolve_port store. `forge up` arms the IDENTICAL inputs, so up and
	// deploy resolve the SAME ports for a given key — this is the
	// kill-the-up-vs-deploy-port-drift fix. Deploy commits its render, so the
	// restore hook is unused (an applied render's ports are the truth).
	activateDevStack(projectDir, envName)

	tagSource := "rollback (state file)"
	if !rollback {
		// Resolve image tag via the three-tier precedence chain. Split
		// into its own helper so the precedence logic is testable
		// without stubbing the whole deploy pipeline. Rollback never
		// consults this chain — it reads the per-service state file
		// inside dispatchDeployGroups instead.
		ref, pt, src, terr := resolveDeployImageTag(ctx, projectDir, envName, imageTag, opts.noDigest)
		if terr != nil {
			return terr
		}
		imageTag = ref
		plainTag = pt
		tagSource = src
		// Resolve the PER-IMAGE digest map (image name → sha256). Threaded
		// into every manifest render below so each service pins ITS image's
		// digest.
		//
		// Resolution precedence (highest first):
		//   1. A bound RELEASE (env promoted to a release via `forge promote`).
		//      Pins the digests the release captured — so every env on the same
		//      release deploys byte-identical images (build once, promote). This
		//      is the new layer; it wins because a deliberate promotion is a
		//      stronger signal than whatever the per-env build state happens to
		//      hold.
		//   2. The per-env build state (today's flow): the aggregate +
		//      per-service build-<env>.json digests `forge build --push` wrote.
		//   3. The mutable :tag (resolveDeployImageTag, above) for any image
		//      with no captured digest.
		//
		// FULL BACKWARD COMPAT: when the env has NO release binding, this is
		// byte-identical to before — resolveDeployImageDigests alone. The
		// current cloud + e2e flow binds no release, so it is unaffected.
		// --no-digest disables both digest paths (the tag-only escape hatch).
		digests, boundRel, derr := resolveDeployDigests(projectDir, envName, opts.noDigest)
		if derr != nil {
			return derr
		}
		imageDigests = digests
		if boundRel != "" {
			tagSource = fmt.Sprintf("release %s (promoted; .forge/env-releases.json)", boundRel)
			fmt.Printf("  Release:     %s  (env %q is promoted to it — pinning its digests)\n", boundRel, envName)
		}
	}

	// Resolve namespace.
	if namespace == "" {
		if ns := k8sClusterNamespaceForEnv(ctx, envName); ns != "" {
			namespace = ns
		} else {
			namespace = store.Meta().Name + "-" + envName
		}
	}

	// Read rendered KCL once — used for the deploy-time orchestration
	// AND the "any cluster-shaped service?" check that gates the
	// kubectl-context guard, the namespace banner, and the dev-cluster
	// bootstrap. Missing KCL render is logged and treated as
	// "no filter" (every Deployment in the namespace is awaited),
	// preserving the pre-orchestration behaviour for projects that
	// haven't migrated to the deploy module yet.
	//
	// Note: the warning prints earlier in the deploy sequence than it
	// did pre-extraction (before "Generating manifests..." rather than
	// after "Applying manifests..."). Strictly informational, and only
	// fires on an edge case (kcl JSON parse fails after the YAML render
	// succeeds).
	entities, kerr := RenderKCL(ctx, projectDir, envName)
	if kerr != nil {
		fmt.Printf("Note: KCL entity read skipped (%v) — waiting on every Deployment in namespace.\n", kerr)
	}

	// --target scoping (application-level filter). Filter the rendered
	// Services / Frontends down to the named apps BEFORE everything that
	// derives from the entity set: buildDeployGroups (External/Compose
	// bucketing), the rollout-wait / host-skip / one-shot-Job sets, and
	// the cluster banners. An empty filter is a no-op (every app), the
	// unchanged default. A typo'd target is caught here with the list of
	// available app names rather than producing a silent no-op deploy.
	if len(targets) > 0 && entities != nil {
		if err := validateDeployTargets(entities, targets); err != nil {
			return err
		}
		entities = filterEntitiesByTarget(entities, targets)
	}

	hasK8sServices := kclEntitiesHaveK8sCluster(entities)

	// Loud-by-default namespace mismatch guard: when KCL env_vars hardcode
	// a project-prefixed `*.svc.cluster.local` reference that disagrees
	// with the namespace we're about to deploy into, fail BEFORE any
	// manifest applies. This is the silent CrashLoop the cp-forge-dev
	// smoke test surfaced — pods land in namespace A, env vars point at
	// services in namespace B, every dial returns `no such host` and no
	// step in the pipeline names the misconfiguration. See the helper for
	// the heuristic that distinguishes legitimate cross-namespace refs
	// from typos.
	if hasK8sServices {
		if err := checkNamespaceReferences(entities, store.Meta().Name, namespace); err != nil {
			return err
		}
	}

	fmt.Printf("Deploying project: %s\n", store.Meta().Name)
	fmt.Printf("  Environment: %s\n", envName)
	if rollback {
		fmt.Printf("  Mode:        rollback\n")
	} else {
		fmt.Printf("  Image tag:   %s  (source: %s)\n", imageTag, tagSource)
	}
	// Namespace and kubectl-context belong to the K8sCluster pipeline;
	// suppress both banners for external-only / compose-only projects
	// so the deploy output isn't cluttered with cluster artifacts that
	// don't apply.
	if hasK8sServices {
		fmt.Printf("  Namespace:   %s\n", namespace)
	}
	fmt.Printf("  Dry run:     %v\n", dryRun)
	fmt.Println()

	// Declared external-prerequisite CHECKLIST: print the out-of-band facts
	// this env DEPENDS ON (forge.ExternalSecret / forge.DNSRecord) so the
	// operator sees them every deploy — not just buried in a docstring. The
	// ExternalSecret half is also ENFORCED by the preflight (a missing one
	// BLOCKS); the DNS half can't be authoritatively verified, so the
	// checklist is the only signal for it. Always printed (incl. dry-run /
	// local clusters, where the preflight Secret check is skipped) so the
	// reminder is never lost. No-op when the env declares no prereqs.
	printPrerequisiteChecklist(entities)

	// kubectl-context guard: only meaningful when at least one service
	// in the bundle targets K8sCluster. External-only / compose-only
	// projects don't touch kubectl, so the guard would surface a wrong-
	// context error that has no bearing on what's about to ship.
	if hasK8sServices {
		// Runs under --dry-run too: dry-run is for surfacing mistakes
		// (wrong context!) before they ship, not for papering over
		// them. The context is purely declarative (forge.K8sCluster.cluster)
		// — there is no CLI escape hatch, so the guard always runs.
		if err := verifyKubectlContext(ctx, cfg, envName); err != nil {
			return err
		}
	}

	start := time.Now()

	// Cluster bootstrap. Declarative first: when the env's Bundle declares
	// `clusters = [...]`, reconcile each (create-if-absent, no-op if
	// present) — the multi-cluster generalization of the dev-only ensure
	// below, ownership a reference via Cluster.owner (derived network /
	// registry-inherit). A
	// declared-cluster env works in ANY env name (not just "dev"), so a
	// multi-cluster e2e/preview env stands up its clusters here. When the
	// env declares NO clusters, fall back to the legacy dev-only
	// ensureDevCluster (single cluster from deploy/k3d.yaml) so existing
	// single-cluster dev envs are byte-identical.
	//
	// Skipped under --dry-run (renders only; never touches a cluster /
	// registry), on rollback (reuses the tag already in the cluster), and
	// when the env has no K8sCluster services (external-only env needs no
	// k3d cluster).
	if !dryRun && !rollback && hasK8sServices {
		if entities != nil && len(entities.Clusters) > 0 {
			if err := reconcileDeclaredClusters(ctx, entities.Clusters, projectDir, envName); err != nil {
				return err
			}
		} else if envName == "dev" {
			if err := ensureDevCluster(ctx); err != nil {
				return err
			}
		}
	}

	// Local image build+push: dev only (the local registry path). Remote
	// envs build/push out-of-band (CI). Independent of the cluster
	// bootstrap above so a multi-cluster dev env still pushes to its
	// owner cluster's registry.
	if envName == "dev" && !dryRun && !rollback && hasK8sServices {
		if err := buildAndPushLocal(ctx, cfg, imageTag, targetArchFlag); err != nil {
			return err
		}
	}

	// Resolve per-env config. Non-sensitive scalars are passed to KCL via
	// `-D key=value` so they bind to top-level identifiers in main.k.
	// Sensitive fields are emitted as secret refs by the deploy gen
	// pipeline (deploy/kcl/<env>/config_gen.k) and aren't piped through
	// here. Missing per-env config is non-fatal.
	envCfgKV := map[string]string{}
	if envConfig, lerr := config.LoadEnvironmentConfig(projectDir, envName); lerr == nil {
		for k, v := range envConfig {
			if s, ok := v.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "${") {
				continue // secret refs handled by config_gen.k
			}
			envCfgKV[k] = fmt.Sprint(v)
		}
	}

	fmt.Printf("Generating manifests from %s...\n", mainK)

	// Build deploy groups from the rendered entities. Services bucket
	// by deploy target type: K8sCluster groups by (cluster, ns,
	// registry); External by deploy_cmd; Compose by compose_file.
	// Host / build-only services are skipped (those are forge run /
	// forge build territory).
	groups, gerr := buildDeployGroupsWithOpts(envName, entities, namespace, dryRun)
	if gerr != nil {
		return fmt.Errorf("group services for deploy: %w", gerr)
	}
	// Propagate the resolved image tag to every group. The cluster
	// path uses ImageTag implicitly via cluster.Apply, but the
	// external/compose providers read group.ImageTag for ${TAG}
	// substitution — without this, External deploy_cmd sees an empty
	// tag and downstream scripts (vultr-deploy.sh) error out. Use the
	// PLAIN tag here, never the digest form: a `${IMAGE}:${TAG}` with
	// `${TAG}=@sha256:...` would render a broken `image:@sha256:...`
	// ref. (The K8sCluster manifest render gets the digest via imageTag
	// below; group.ImageTag is the External/Compose ${TAG} only.)
	for i := range groups {
		if groups[i].ImageTag == "" {
			groups[i].ImageTag = plainTag
		}
	}

	// Declarative kubectl context: the deploy target is read from the
	// rendered KCL's `forge.K8sCluster.cluster` (== the kubectl context
	// name), which every K8sCluster group carries on group.Cluster. The
	// per-group dispatch builds its context per group (resolveGroupContext)
	// so a multi-cluster env lands each group on ITS OWN declared cluster.
	// For the env-wide consumers that don't iterate groups — the secrets
	// pre-apply, the empty-groups direct cluster.Apply, and the rollback
	// provider — we use the first declared cluster as the env's context.
	// The context is purely declarative; there is no CLI override.
	//
	// Fail fast FIRST: if a declared cluster has no matching kubectl
	// context, refuse here with the list of available contexts rather than
	// silently applying to whatever's active. This is the guard that makes
	// a wrong-cluster deploy impossible — it always runs (dry-run is for
	// surfacing mistakes, not papering over them). Host-only / compose envs
	// declare no cluster and are skipped.
	if hasK8sServices {
		if err := verifyDeclaredContextsExist(ctx, groups); err != nil {
			return err
		}
	}
	deployContext := declaredEnvContext(groups)
	// declaredEnvContext only sees SERVICE-derived groups, so an operator- or
	// cronjob-only --target (no service groups) leaves it empty. The apply
	// chokepoint (cluster.KubectlApply) HARD-REJECTS an empty context
	// instead of silently using kubectl's current one — the footgun where a
	// flipped current-context lands a deploy in the wrong cluster. A declared
	// cluster is a property of the ENV (forge.K8sCluster.cluster), not of any
	// one service, so resolve it directly here — the same source --explain
	// uses.
	if deployContext == "" {
		deployContext = expectedClusterForEnv(ctx, cfg, envName)
	}

	// Resolve the env's declared platform deps (forge.HelmChart) THIS apply
	// renders into cluster.HelmChartSpec values (helm-as-a-RENDERER). The
	// uniform exclusive --target rule (selectedHelmChartEntities, mirroring
	// cluster.selectHelmChartsByGroup): EVERY declared chart on a bare
	// `forge deploy <env>` (no --target — the full declarative reconcile),
	// or EXACTLY the charts whose Name ∈ --target. cluster.Apply renders
	// each selected chart (helm template --skip-crds), stamps
	// app.kubernetes.io/name, and applies CRDs-first/Established-gated before
	// the chart's controllers. The CRD-bundle network fetch only runs for the
	// charts actually selected here.
	var helmSpecs []cluster.HelmChartSpec
	if entities != nil {
		selected := selectedHelmChartEntities(entities.HelmCharts, targets)
		if len(selected) > 0 {
			helmSpecs, err = helmChartSpecsFromEntities(ctx, selected)
			if err != nil {
				return err
			}
		}
	}

	// Platform dependencies (cert-manager, Envoy Gateway, …) are NOT
	// install-if-missing'd here. They are declarative renderables (KCL
	// forge.HelmChart) applied EXPLICITLY via `forge deploy <env>
	// --target=<platform-name>` — helm-as-a-RENDERER folds the chart's
	// manifests (and forge-supplied CRDs, CRD-first + Established-gated)
	// into the same apply pipeline. A `--target=<platform>` apply leaves
	// the cluster with the CRDs Established + controllers Deployed, so this
	// app deploy finds them present. The preflight's CRD gate still BLOCKS
	// if a referenced CRD is absent — the fix is to apply the platform dep
	// first, not a hidden per-deploy install.

	// Deployability preflight: BEFORE the first apply (including the dotenv
	// Secret projection below), verify against the LIVE target that every
	// Secret KEY the rendered manifests reference is provisioned and every
	// container image resolves. A missing key (CreateContainerConfigError)
	// or missing image (ImagePullBackOff) otherwise only surfaces as a pod
	// crash mid-rollout, discovered one-at-a-time. This prints ALL the gaps
	// at once and refuses to apply. Default-ON for remote/cloud clusters;
	// the Secret check is skipped for local dev clusters (forge applies the
	// dotenv Secrets itself moments later, so they don't exist yet) and
	// local-registry images are skipped (they live in the in-cluster
	// registry the checker can't reach). Runs under --dry-run too (pure
	// read-only check). --skip-preflight bypasses it.
	if hasK8sServices && !rollback && !opts.skipPreflight {
		// Resolve the target cluster's declared node arch for the preflight
		// arch gate from the env's KCL deploy.Cluster.platform — the explicit
		// per-env SSOT the cloud envs set to "amd64". We deliberately do NOT
		// fall back to forge.yaml deploy.target_arch here: that field carries
		// an implicit "amd64" default, and seeding the gate from a value the
		// author never declared would risk false-failing an env that simply
		// hasn't opted into platform pinning yet (the WARN-don't-block
		// contract). Empty (no platform declared — e.g. the local e2e env)
		// leaves the gate inert.
		targetArch := kclFirstClusterPlatform(entities)
		if err := runDeployPreflight(ctx, deployPreflightInput{
			mainK:           mainK,
			imageTag:        imageTag,
			namespace:       namespace,
			env:             envName,
			envCfgKV:        envCfgKV,
			deployCtx:       deployContext,
			targets:         targets,
			targetArch:      targetArch,
			imageDigests:    imageDigests,
			requiredSecrets: requiredSecretsForPreflight(entities),
			secretSupply:    secretSupplyForPreflight(entities),
		}); err != nil {
			return err
		}
	}

	// k8s Secret projection: for a dotenv secret_provider, render the
	// declared cluster secret refs into plaintext Secret manifests and
	// apply them BEFORE the Deployments roll out (so each Deployment's
	// secretKeyRef resolves on first schedule). Skipped on rollback —
	// rollback reuses the tag (and the Secret) already in the cluster.
	if !rollback {
		if err := applyK8sSecretsFromProvider(ctx, entities, groups, namespace, deployContext, envName, dryRun); err != nil {
			return err
		}
	}

	if rollback {
		// Rollback path: dispatch each group to its provider's
		// Rollback. The dispatcher reads per-service state files for
		// external/compose providers and surfaces a clear error when
		// a service has no previous deploy on record.
		hostSkip := hostDeploymentSkipSetFromKCL(cfg, entities)
		oneShotJobs := oneShotJobNamesFromKCL(entities)
		// Rollback's K8sCluster provider doesn't drive cluster.Apply
		// (kubectl rollout undo lives in the provider's Rollback) so
		// the builder is unused for rollback groups, but the registry
		// still needs the provider registered. We reuse the deploy-
		// shaped builder for symmetry with the deploy path.
		// Rollback reuses the tag already in the cluster, so imageDigests is
		// nil here (the digest-map resolution is gated behind `if !rollback`);
		// the builder threads it through unused for symmetry. Rollback
		// applies no platform deps (it reverts workloads to the last tag),
		// so helmSpecs is nil here.
		builder := applyOptsBuilderFromContext(mainK, imageTag, namespace, envName, envCfgKV, dryRun, prune, hostSkip, oneShotJobs, targets, groups, entities, imageDigests, nil)
		registry := deploytarget.NewRegistry()
		// Rollback's per-group context is resolved by the provider purely
		// from each group's declared cluster (forge.K8sCluster.cluster) —
		// no override, no current-context fallback.
		registry.Register(deploytarget.K8sClusterProvider{ApplyOptsBuilder: builder})
		if err := rollbackDeployGroups(ctx, registry, groups, projectDir); err != nil {
			return err
		}
		fmt.Printf("\nRollback completed in %s.\n", time.Since(start).Truncate(time.Millisecond))
		return nil
	}

	// When no K8sCluster groups are present, the rendered set carries
	// only external / compose / host / build-only — nothing to apply
	// via the cluster pipeline. Skip the check above (no namespace)
	// and let dispatchDeployGroups handle the per-provider paths or
	// no-op trivially.
	// A frontend-only env (e.g. just a Firebase Hosting frontend, no
	// services / operators / cronjobs) has nothing for the cluster
	// pipeline to apply. Skip the empty-groups cluster.Apply below so
	// such projects don't need kubectl configured at all — the frontend
	// dispatch further down does the real work.
	frontendOnly := len(groups) == 0 && !hasK8sServices && hasFirebaseFrontend(entities) &&
		entities != nil && len(entities.Operators) == 0 && len(entities.CronJobs) == 0
	if len(groups) == 0 && !frontendOnly {
		// Nothing to dispatch — historical behaviour was to still run
		// cluster.Apply against the env's main.k in case host-only
		// entities still produced manifests (CronJobs etc.). Preserve
		// that with one direct call.
		if err := cluster.Apply(ctx, cluster.ApplyOpts{
			MainK:        mainK,
			ImageTag:     imageTag,
			ImageDigests: imageDigests,
			Namespace:    namespace,
			Env:          envName,
			Context:      deployContext,
			EnvConfigKV:  envCfgKV,
			DryRun:       dryRun,
			DryRunFramed: true,
			Prune:        prune,
			HostSkip:     hostDeploymentSkipSetFromKCL(cfg, entities),
			OneShotJobs:  oneShotJobNamesFromKCL(entities),
			Targets:      targets,
			HelmCharts:   helmSpecs,
		}); err != nil {
			return err
		}
	} else if len(groups) > 0 {
		// Dispatch each group through its provider. The K8sCluster
		// provider wraps cluster.Apply via the builder closure so the
		// per-call envelope (mainK / image tag / env config / dry-run
		// / prune / host-skip / one-shot jobs) flows through verbatim.
		hostSkip := hostDeploymentSkipSetFromKCL(cfg, entities)
		oneShotJobs := oneShotJobNamesFromKCL(entities)
		builder := applyOptsBuilderFromContext(mainK, imageTag, namespace, envName, envCfgKV, dryRun, prune, hostSkip, oneShotJobs, targets, groups, entities, imageDigests, helmSpecs)
		registry := deploytarget.NewRegistry()
		registry.Register(deploytarget.K8sClusterProvider{ApplyOptsBuilder: builder})
		if err := dispatchDeployGroups(ctx, registry, groups, ""); err != nil {
			return err
		}
	}

	// Frontend deploy dispatch — frontends declaring a first-class deploy
	// target (today: forge.FirebaseHosting) are built + shipped after the
	// service groups. Runs under --dry-run too so the assemble/firebase
	// plan surfaces before any side effect. No-op when no frontend
	// declares a deploy target — the unchanged default for k8s/host/none
	// frontends.
	//
	// --skip-frontend short-circuits the dispatch entirely: the k8s apply
	// above already ran, and the user explicitly asked to leave the
	// frontend (and its ../web/dist rebuild) untouched. (Naming only
	// backend apps via --target also excludes frontends, because the
	// target filter empties entities.Frontends; --skip-frontend is the
	// "whole backend, no frontend" variant that doesn't require listing
	// every service.)
	if opts.skipFrontend {
		if hasFirebaseFrontend(entities) {
			fmt.Println("\nSkipping frontend deploy (--skip-frontend).")
		}
	} else if err := dispatchFrontendDeploys(ctx, entities, projectDir, envName, envCfgKV, dryRun); err != nil {
		return err
	}

	if dryRun {
		return nil
	}

	fmt.Printf("\nDeploy completed in %s.\n", time.Since(start).Truncate(time.Millisecond))
	return nil
}

// dispatchFrontendDeploys ships every frontend declaring a first-class
// deploy target. Today that's exclusively forge.FirebaseHosting: build
// the frontend (npm install + npm run build) with the frontend's
// env_vars injected as build-time env, assemble public_dir + any bundle
// dirs into a staging tree honoring base_path, write firebase.json +
// .firebaserc, then `firebase deploy`.
//
// Frontends without a deploy block — and frontends whose deploy target
// isn't Firebase — are skipped, preserving the pre-feature behaviour for
// every existing project. envCfgKV (the per-env -D config) is layered
// UNDER the frontend's KCL env_vars so an explicit env_var wins, and is
// only injected when the env var name was actually declared on the
// frontend (we don't leak the whole env config into the JS build).
func dispatchFrontendDeploys(ctx context.Context, entities *KCLEntities, projectDir, envName string, envCfgKV map[string]string, dryRun bool) error {
	if entities == nil {
		return nil
	}
	var fes []deploytarget.FirebaseFrontend
	for _, f := range entities.Frontends {
		if f.Deploy == nil || f.Deploy.Type != "firebase" || f.Deploy.Firebase == nil {
			continue
		}
		fes = append(fes, frontendToFirebase(f))
	}
	if len(fes) == 0 {
		return nil
	}

	fmt.Printf("\nDeploying %d frontend(s) to Firebase Hosting...\n", len(fes))
	// Dispatch the Firebase frontends through the registry like every
	// other deploy target: build a frontend-bearing group and route it
	// via the provider's Name(). The registry re-registers a
	// ProjectDir-configured FirebaseProvider (the K8sClusterProvider
	// ApplyOptsBuilder pattern) so the provider resolves frontend paths
	// against the project root.
	registry := deploytarget.NewRegistry()
	registry.Register(deploytarget.FirebaseProvider{ProjectDir: projectDir})
	group := deploytarget.ServiceGroup{
		Env:        envName,
		ProviderID: deploytarget.FirebaseProvider{}.Name(),
		Frontends:  fes,
		DryRun:     dryRun,
	}
	return dispatchDeployGroups(ctx, registry, []deploytarget.ServiceGroup{group}, "")
}

// hasFirebaseFrontend reports whether any rendered frontend declares a
// Firebase Hosting deploy target. Used to recognise a frontend-only env
// (skip the cluster pipeline) and gates nothing else.
func hasFirebaseFrontend(e *KCLEntities) bool {
	if e == nil {
		return false
	}
	for _, f := range e.Frontends {
		if f.Deploy != nil && f.Deploy.Type == "firebase" {
			return true
		}
	}
	return false
}

// frontendToFirebase maps a rendered FrontendEntity (with a Firebase
// deploy block) onto the deploytarget.FirebaseFrontend the provider
// consumes. The frontend's env_vars become the build-time env injected
// into the JS build (NEXT_PUBLIC_* / VITE_*); only inline Value entries
// are forwarded — secret/configmap-projected vars have no host build-time
// value to inject.
func frontendToFirebase(f FrontendEntity) deploytarget.FirebaseFrontend {
	fb := f.Deploy.Firebase
	buildEnv := map[string]string{}
	for _, ev := range f.EnvVars {
		if ev.Value != "" {
			buildEnv[ev.Name] = ev.Value
		}
	}
	bundles := make([]deploytarget.FirebaseBundleSpec, 0, len(fb.Bundle))
	for _, b := range fb.Bundle {
		bundles = append(bundles, deploytarget.FirebaseBundleSpec{Src: b.Src, Dest: b.Dest})
	}
	return deploytarget.FirebaseFrontend{
		Name:      f.Name,
		Path:      f.Path,
		DevRunner: f.DevRunner,
		BuildEnv:  buildEnv,
		Spec: deploytarget.FirebaseHostingSpec{
			Project:   fb.Project,
			Site:      fb.Site,
			Target:    fb.Target,
			PublicDir: fb.PublicDir,
			BasePath:  fb.BasePath,
			Bundle:    bundles,
			Rewrites:  fb.Rewrites,
		},
	}
}

// validateDeployTargets checks every name passed to --target against
// the set of deployable app names in the rendered KCL (services +
// operators + frontends). A target that matches nothing is almost
// always a typo; erroring here — with the list of available app names —
// is far friendlier than silently deploying nothing (group filter
// empties out) or applying a shared-only manifest bundle. Reuses
// inTargetSet's membership semantics indirectly via a name set.
//
// Operators are first-class --target subjects: each renders as a
// Deployment + cluster RBAC (ServiceAccount / ClusterRole /
// ClusterRoleBinding) all carrying `app.kubernetes.io/name = <op>`, so
// naming an operator scopes the K8sCluster apply to that operator's
// workload exactly the way a Service target does
// (cluster.SelectManifestsByGroup is group-label-driven, not kind-driven).
// Without operators in this set, `forge deploy <env> --target <op>`
// errored "unknown --target" because operators were absent from the
// available-groups list — you couldn't deploy just an operator.
//
// A --target is always a SERVICE group (service / operator / frontend /
// helm-chart name). The env-shared manifests (Namespace, ConfigMap,
// RuntimeClass, NetworkPolicy, bundle-level additional_manifests) belong to
// no service — they carry no group, apply only on a bare deploy, and are
// not addressable by `--target`, so they are deliberately NOT in this set.
func validateDeployTargets(e *KCLEntities, targets []string) error {
	avail := map[string]struct{}{}
	for _, s := range e.Services {
		avail[s.Name] = struct{}{}
	}
	for _, o := range e.Operators {
		avail[o.Name] = struct{}{}
	}
	for _, f := range e.Frontends {
		avail[f.Name] = struct{}{}
	}
	// Platform deps (forge.HelmChart) are first-class --target subjects:
	// `forge deploy <env> --target=<chart>` renders + applies that chart's
	// group. A chart NAME is a valid target the same way a service name is —
	// its rendered manifests carry it as their app.kubernetes.io/name group.
	for _, h := range e.HelmCharts {
		avail[h.Name] = struct{}{}
	}
	var unknown []string
	for _, t := range targets {
		if _, ok := avail[t]; !ok {
			unknown = append(unknown, t)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	names := make([]string, 0, len(avail))
	for n := range avail {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("unknown --target %s; available apps in env: %s",
		strings.Join(unknown, ", "), strings.Join(names, ", "))
}

// filterEntitiesByTarget returns a shallow copy of e with Services,
// Operators, and Frontends narrowed to the names in targets (reusing
// inTargetSet from up.go for the membership test). The remaining entity
// slices — cronjobs, gateways, routes — are carried through UNCHANGED:
// those carry their own group label, and the K8sCluster apply's exclusive
// cluster.SelectManifestsByGroup is what actually drops the non-targeted
// ones from the rendered stream.
//
// Narrowing Operators here (not just Services/Frontends) is what makes
// an operator a first-class --target. It scopes the entity-derived sets
// that key off the operator slice — chiefly the frontendOnly gate in
// runDeploy (which checks len(entities.Operators)) — so `forge deploy
// <env> --target <operator>` doesn't accidentally look like a
// frontend-only env. The K8sCluster apply does the load-bearing scoping
// at the manifest level: with the operator name in opts.Targets,
// cluster.SelectManifestsByGroup keeps EXACTLY that operator's Deployment +
// RBAC (all carry the operator's group) and drops every other group —
// other apps AND the env-shared manifests (unless their group is also
// targeted). Operators have no External/Compose/host dispatch, so there's
// nothing else to scope.
func filterEntitiesByTarget(e *KCLEntities, targets []string) *KCLEntities {
	out := *e // shallow copy; slices below are rebuilt, the rest shared
	var svcs []ServiceEntity
	for _, s := range e.Services {
		if inTargetSet(targets, s.Name) {
			svcs = append(svcs, s)
		}
	}
	var ops []OperatorEntity
	for _, o := range e.Operators {
		if inTargetSet(targets, o.Name) {
			ops = append(ops, o)
		}
	}
	var fes []FrontendEntity
	for _, f := range e.Frontends {
		if inTargetSet(targets, f.Name) {
			fes = append(fes, f)
		}
	}
	out.Services = svcs
	out.Operators = ops
	out.Frontends = fes
	return &out
}

// kclEntitiesHaveK8sCluster returns true when at least one service in
// the rendered KCL declares `deploy: cluster`. Used to gate the
// kubectl-context guard, the "Namespace:" banner, and the dev-cluster
// bootstrap so external-only / compose-only projects don't print
// cluster-flavored boilerplate or refuse a deploy because kubectl
// isn't configured.
//
// Returns false when entities is nil (KCL render failed) — the
// conservative behaviour is to fall back to the no-cluster shape and
// let cluster.Apply fail with its own clearer error if a K8s service
// turns out to be in the bundle.
func kclEntitiesHaveK8sCluster(entities *KCLEntities) bool {
	if entities == nil {
		return false
	}
	for _, svc := range entities.Services {
		if svc.Deploy.Type == "cluster" {
			return true
		}
	}
	return false
}

// oneShotJobNamesFromKCL returns the names of every CronJob entity with
// an empty Schedule — these render as one-shot Jobs and the deploy
// waits on `condition=complete` before returning. Scheduled CronJobs
// (non-empty Schedule) are excluded; they run on their own cadence.
func oneShotJobNamesFromKCL(e *KCLEntities) []string {
	if e == nil {
		return nil
	}
	var out []string
	for _, cj := range e.CronJobs {
		if cj.Schedule == "" {
			out = append(out, cj.Name)
		}
	}
	return out
}

// hostDeploymentSkipSetFromKCL returns the set of Deployment names that
// the deploy's rollout wait should skip — services declared `deploy: host`
// in the rendered KCL. Each host service name expands to two keys:
//
//   - the bare name ("admin-server"), matching per-service-binary mode
//   - the project-prefixed name ("<project>-admin-server"), matching
//     shared-binary mode where KCL renders `<project>-<svc>` Deployments
//
// Returning both is cheap and lets the caller iterate over actually-
// applied Deployment names without re-deriving the project-prefix rule.
// Empty entity set → empty skip set (legacy behaviour preserved).
func hostDeploymentSkipSetFromKCL(cfg *config.ProjectConfig, e *KCLEntities) map[string]struct{} {
	out := map[string]struct{}{}
	if cfg == nil || e == nil {
		return out
	}
	for _, name := range e.HostServiceNames() {
		out[name] = struct{}{}
		out[cfg.Name+"-"+name] = struct{}{}
	}
	return out
}

// resolveDeployImageTag is the precedence chain `forge deploy <env>`
// runs to pick the image tag for the KCL manifest render. Returns the
// resolved tag and a human-readable description of where it came from
// (printed in the deploy summary so users can debug a surprising
// choice without re-deriving the priority order in their head).
//
// Priority:
//
//  1. flagOverride — `--tag` (or its `--image-tag` alias) on the CLI.
//     CI pipelines that pin a release number land here.
//  2. .forge/state/build-<env>.json — what `forge build --push` last
//     pushed for this env. This is the load-bearing path that closes
//     the build/deploy tag divergence: build records the exact tag it
//     pushed, deploy reads it back, the working-tree state between the
//     two phases stops mattering.
//  3. resolveImageTag — git-derived fallback (`git describe --tags
//     --always --dirty`). The standalone-deploy path: no preceding
//     build, no override — recompute the tag the way build would.
//
// DIGEST PINNING: when the build-state record carries a content-addressed
// Digest (captured by `forge build --push`), the returned reference is the
// IMMUTABLE digest form `@sha256:...` instead of the mutable :tag — unless
// noDigest is set. The KCL `_image_ref` seam recognises an `@`-prefixed
// image_tag and pins `<image>@sha256:...`, dropping the env tag. A digest
// can't go stale and can't be re-pointed, so the node-cache / re-tag-didn't-
// take failure class is structurally impossible. Fallback is automatic: no
// captured digest (third-party images, non-pushed builds, the local-registry
// e2e path) → the tag is used exactly as before.
//
// Errors:
//
//   - flagOverride bypasses every error path (the user told us
//     exactly what to use).
//   - A present-but-unreadable state file is a hard error — silently
//     falling back would mask a real bug.
//   - A missing state file is fine; the function falls through to git.
//   - A git-derivation failure on the fallback path is wrapped with a
//     "pass --tag to override" hint so users have an escape hatch.
//
// Return values: (imageRef, plainTag, source, err). imageRef is what the
// KCL manifest render pins — the digest form `@sha256:...` when a digest is
// preferred, else the plain tag. plainTag is ALWAYS the mutable tag, used by
// the External/Compose providers for their `${TAG}` substitution (a digest
// there would break `${IMAGE}:${TAG}`). For every tag-only path the two are
// identical.
func resolveDeployImageTag(ctx context.Context, projectDir, envName, flagOverride string, noDigest bool) (imageRef, plainTag, source string, err error) {
	if flagOverride != "" {
		return flagOverride, flagOverride, "explicit --tag flag", nil
	}
	// Per-env record first, then the env-agnostic "default" written by a
	// plain `forge build` (no --env). This is what makes
	// `forge build --docker && forge deploy prod` work without forcing a
	// matching --env / --tag on both.
	for _, key := range buildStateLookupEnvs(envName) {
		st, berr := ReadBuildState(projectDir, key)
		if berr != nil {
			return "", "", "", fmt.Errorf("read build state: %w (delete .forge/state/build-%s.json to recompute from git)", berr, key)
		}
		if st == nil {
			continue
		}
		// Stale-image guard: the recorded build may be from an older
		// commit than the one currently checked out. Deploying it would
		// silently ship code the user already moved past — a real-money
		// footgun on prod. When the working tree is CLEAN and the build's
		// source commit differs from HEAD, refuse by default and point at
		// the two escape hatches (rebuild, or --tag to deploy the recorded
		// tag anyway). Dirty trees skip this check: there's no single HEAD
		// the build can be "behind," and warnIfNonReproducible already
		// flags the dirty build.
		if serr := checkBuildStateFreshness(ctx, projectDir, st); serr != nil {
			return "", "", "", serr
		}
		warnIfNonReproducible(st)
		// imageRef is ALWAYS the mutable tag now — never an env-wide digest.
		// Digest pinning moved to resolveDeployImageDigests, which builds a
		// PER-IMAGE name→digest map (control-plane→d…, reliant→12…,
		// workspace-base→89…) from the per-service build states; the KCL
		// `_image_ref` seam then pins each service to ITS image's digest.
		// Returning one digest here was the bug: it stamped a single image's
		// digest onto every service (reliant pinned to control-plane's digest
		// → manifest unknown). The env tag is the per-image fallback (vendored
		// images, no-digest builds) and the External/Compose ${TAG} source.
		src := fmt.Sprintf(".forge/state/build-%s.json (built %s)", key, st.PushedAt)
		return st.Tag, st.Tag, src, nil
	}
	t, terr := resolveImageTag(ctx, envName)
	if terr != nil {
		return "", "", "", fmt.Errorf("failed to determine image tag: %w\nUse --tag to specify one manually", terr)
	}
	return t, t, "git describe --tags --always --dirty", nil
}

// resolveDeployImageDigests builds the PER-IMAGE name→digest map the KCL
// manifest render pins each service's image to. This is the correctness fix
// for the multi-image deploy: instead of one env-wide digest stamped onto
// every service (which pinned reliant + workspace-base to the control-plane
// digest → `manifest unknown`), each image resolves to ITS OWN captured
// digest.
//
// The map is keyed on the BARE image name (`control-plane`, `reliant`,
// `workspace-base`), the same key the KCL `_image_ref` seam looks up against
// `svc.image`. Multiple services may share one image (reliant-api-server /
// reliant-temporal-worker / daemon-gateway all run `reliant`); they all carry
// the same digest, so a later write is a harmless overwrite.
//
// Sources, all best-effort (a missing/unreadable file is skipped, never
// fatal — deploy still works on the tag for any image with no digest):
//
//   - The aggregate `.forge/state/build-<env>.json` (and the `default`
//     fallback) the docker PROJECT build and the external-build deploy-side
//     writer produce — carries the control-plane image's digest.
//   - Every per-service `.forge/state/build-<env>-<service>.json` the
//     external-build dispatcher writes — carries each external image's own
//     digest (reliant, workspace-base).
//
// Returns nil when noDigest is set (the --no-digest escape hatch) or nothing
// captured a digest — the render then leaves every image on its tag,
// byte-identical to the pre-digest behaviour (the local-registry e2e path
// captures no digests, so it is unaffected).
func resolveDeployImageDigests(projectDir, envName string, noDigest bool) (map[string]string, error) {
	if noDigest {
		return nil, nil
	}
	out := map[string]string{}

	// Aggregate build state(s): env-specific, then the env-agnostic default.
	for _, key := range buildStateLookupEnvs(envName) {
		st, err := ReadBuildState(projectDir, key)
		if err != nil {
			// A present-but-unreadable aggregate is already a hard error in
			// resolveDeployImageTag (the tag path runs first), so here we
			// treat it as "no digest from this source" rather than double-
			// reporting — the tag resolver surfaces the real error.
			continue
		}
		if st != nil && st.Image != "" && st.Digest != "" {
			out[st.Image] = st.Digest
		}
	}

	// Per-service external-build states: build-<env>-<service>.json. Glob the
	// state dir for this env's per-service files and read each one. These
	// carry the per-image digests the aggregate can't (the aggregate is a
	// single last-writer-wins file).
	stateDir := filepath.Join(projectDir, statefile.DirRel)
	pattern := filepath.Join(stateDir, "build-"+statefile.SafeSegment(envName)+"-*.json")
	matches, _ := filepath.Glob(pattern)
	for _, path := range matches {
		// Derive the service segment from the filename so we can read the
		// typed per-service state via buildtarget.ReadState (one decode path,
		// SafeSegment-aware). filename: build-<env>-<service>.json
		base := filepath.Base(path)
		prefix := "build-" + statefile.SafeSegment(envName) + "-"
		if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".json") {
			continue
		}
		service := strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".json")
		st, err := buildtarget.ReadState(projectDir, envName, service)
		if err != nil || st == nil {
			continue
		}
		if st.Image != "" && st.Digest != "" {
			out[st.Image] = st.Digest
		}
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// resolveDeployDigests is the per-image digest resolver with the release layer
// folded in. It returns the image-name → digest map the KCL render pins each
// service to, plus the bound release version (empty when the env has no
// binding) so the caller can surface it in the deploy banner.
//
// Precedence (highest first):
//
//  1. A bound RELEASE (env promoted via `forge promote`). The release's
//     resolved digests OVERRIDE the per-env build state per image: a deliberate
//     promotion is the strongest signal, and pinning the release's digests is
//     what makes every env on the same release deploy byte-identical images
//     (build once, promote). This is the new layer.
//  2. The per-env build state (today's flow): resolveDeployImageDigests reads
//     the aggregate + per-service build-<env>.json digests `forge build --push`
//     wrote.
//  3. The mutable :tag (resolveDeployImageTag, the caller's separate step) for
//     any image still carrying no digest.
//
// FULL BACKWARD COMPAT: an env with NO release binding gets exactly
// resolveDeployImageDigests' result and a "" release — byte-identical to before
// the release layer existed. The current cloud + e2e flow binds no release, so
// it is unaffected. noDigest disables both digest paths (the tag-only escape
// hatch) AND the release lookup.
func resolveDeployDigests(projectDir, envName string, noDigest bool) (digests map[string]string, boundRelease string, err error) {
	base, err := resolveDeployImageDigests(projectDir, envName, noDigest)
	if err != nil {
		return nil, "", err
	}
	if noDigest {
		return base, "", nil
	}
	binding, bound, berr := boundReleaseForEnv(projectDir, envName)
	if berr != nil {
		return nil, "", fmt.Errorf("read env-release binding for %q: %w", envName, berr)
	}
	if !bound {
		return base, "", nil
	}
	// Layer the release's resolved digests over the per-env build state. A
	// per-image override (not a wholesale replace) keeps any image present in
	// the build state but absent from the release still resolvable — though in
	// practice a release is authoritative for everything it pins.
	if base == nil {
		base = map[string]string{}
	}
	for image, digest := range binding.Resolved {
		base[image] = digest
	}
	return base, binding.Release, nil
}

// buildStateLookupEnvs is the ordered fallback of build-state keys to try
// for a deploy env: the env itself, then the "default" record a plain
// `forge build` writes. "default" is not retried for itself.
func buildStateLookupEnvs(envName string) []string {
	if envName == "" || envName == "default" {
		return []string{"default"}
	}
	return []string{envName, "default"}
}

// checkBuildStateFreshness refuses to deploy a build whose recorded
// source commit is behind the current git HEAD, so `forge deploy` never
// silently ships a stale image after a fresh commit/push (fr-02d44d2b03).
//
// The check fires ONLY when all of these hold, to keep it a precise
// footgun-guard rather than a nag:
//
//   - The build recorded a source commit (st.Commit non-empty). Older
//     state files predating commit-stamping skip the check.
//   - There are no uncommitted changes to TRACKED files. Such a tree
//     has no single HEAD the build can be measured against, so the
//     comparison would be meaningless (warnIfNonReproducible covers the
//     dirty-build reproducibility angle separately). Untracked files
//     (editor dirs, build artifacts, gitignored caches) are deliberately
//     ignored — they don't change which commit HEAD points at, and a
//     stray `.idea/` directory must not silently disable a real-money
//     stale-deploy guard.
//   - HEAD is resolvable and differs from st.Commit.
//
// Escape hatch: pass `--tag <tag>` to deploy a specific tag directly —
// that path bypasses build-state (and therefore this check) entirely.
// Git being unavailable is treated as "can't prove staleness" → allow.
func checkBuildStateFreshness(ctx context.Context, projectDir string, st *BuildState) error {
	if st == nil || st.Commit == "" {
		return nil
	}
	head, clean, ok := gitHEADAndClean(ctx, projectDir)
	if !ok || !clean {
		// No HEAD to compare against, or a dirty tree (handled by the
		// dirty warning) — don't block.
		return nil
	}
	if head == st.Commit {
		return nil
	}
	return fmt.Errorf(
		"refusing to deploy stale image: tag %q was built from %s, but HEAD is %s.\n"+
			"  The recorded build predates your current commit — deploying it would ship old code.\n"+
			"  Fix: rebuild from HEAD (forge build --docker ...), then deploy; or pass --tag %s to deploy the recorded image anyway.",
		st.Tag, shortSHA(st.Commit), shortSHA(head), st.Tag)
}

// gitHEADAndClean returns the current HEAD commit, whether the working
// tree has no uncommitted TRACKED changes, and whether git was usable at
// all, evaluated in dir (the project root). ok=false means git failed
// (not a repo, no git binary) — callers treat that as "can't prove
// anything" and fall through rather than erroring. dir scopes the check
// to the project so the result doesn't depend on the process CWD (and so
// tests can run against a throwaway repo).
//
// "clean" uses --untracked-files=no on purpose: untracked files don't
// move HEAD, so a stray editor/artifact directory must not flip the
// staleness guard off. Genuine uncommitted edits to tracked files DO
// count as not-clean (the build's recorded commit can't represent them).
func gitHEADAndClean(ctx context.Context, dir string) (head string, clean, ok bool) {
	rev := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	rev.Dir = dir
	out, err := rev.Output()
	if err != nil {
		return "", false, false
	}
	head = strings.TrimSpace(string(out))
	stat := exec.CommandContext(ctx, "git", "status", "--porcelain", "--untracked-files=no")
	stat.Dir = dir
	st, serr := stat.Output()
	if serr != nil {
		return head, false, false
	}
	clean = strings.TrimSpace(string(st)) == ""
	return head, clean, true
}

// warnIfNonReproducible prints a heads-up when the build being deployed
// came from a dirty working tree or an untagged commit — the single
// "discipline" nudge toward tag-then-build, with no enforcement.
func warnIfNonReproducible(st *BuildState) {
	switch {
	case st.Dirty:
		fmt.Printf("  Warning: deploying %q built from a DIRTY working tree (commit %s) — not reproducible.\n", st.Tag, shortSHA(st.Commit))
	case st.GitTag == "":
		fmt.Printf("  Warning: deploying %q built from an UNTAGGED commit (%s) — tag the release for a reproducible version.\n", st.Tag, shortSHA(st.Commit))
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "unknown"
	}
	return sha
}

func gitShortSHA(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func ensureDevCluster(ctx context.Context) error {
	fmt.Println("Checking k3d cluster...")
	out, err := exec.CommandContext(ctx, "k3d", "cluster", "list", "-o", "json").Output()
	if err != nil {
		return fmt.Errorf("k3d not available: %w\nInstall k3d: https://k3d.io", err)
	}

	// If no clusters exist or our cluster isn't found, the user needs to create one.
	if len(out) == 0 || string(out) == "[]" || string(out) == "[]\n" {
		fmt.Println("No k3d clusters found. Creating dev cluster...")
		k3dConfig := filepath.Join("deploy", "k3d.yaml")
		var createCmd *exec.Cmd
		if _, err := os.Stat(k3dConfig); err == nil {
			createCmd = exec.CommandContext(ctx, "k3d", "cluster", "create", "--config", k3dConfig)
		} else {
			// Fallback create path (no project-level deploy/k3d.yaml).
			// Write a temp registries.yaml that mirrors the canonical
			// `localhost:5050 → registry.localhost:5000` mapping, so
			// in-cluster pulls succeed for images pushed to the
			// host-visible `localhost:5050`. Without this, `docker push
			// localhost:5050/<image>` lands in the registry but pods
			// ImagePullBackOff because `localhost:5050` doesn't resolve
			// from inside the node container. The project-templated
			// `deploy/k3d.yaml` carries the same mirrors inline via the
			// k3d Simple config's `registries.config` block — see
			// internal/templates/deploy/k3d.yaml.tmpl.
			regsPath, regsErr := writeFallbackRegistriesYAML()
			if regsErr != nil {
				return fmt.Errorf("write fallback registries.yaml: %w", regsErr)
			}
			defer func() { _ = os.Remove(regsPath) }()
			createCmd = exec.CommandContext(ctx, "k3d", "cluster", "create", "dev",
				"--registry-create", "dev-registry:0.0.0.0:5050",
				"--registry-config", regsPath,
				"--servers", "1",
				"--no-lb",
			)
		}
		createCmd.Stdout = os.Stdout
		createCmd.Stderr = os.Stderr
		if err := createCmd.Run(); err != nil {
			return fmt.Errorf("failed to create k3d cluster: %w", err)
		}
	} else {
		fmt.Println("  k3d cluster found.")
		fmt.Println("  Tip: if in-cluster image pulls fail with `localhost:5050`,")
		fmt.Println("       the cluster pre-dates forge's auto-mirror config. See")
		fmt.Println("       the deploy skill ('Pre-existing k3d cluster mirror fix').")
	}
	return nil
}

// fallbackRegistriesYAML is the canonical containerd mirror config
// written when forge creates a k3d cluster without a project-level
// `deploy/k3d.yaml` to drive it. Kept as a top-level const so the
// content is reviewable in one place.
const fallbackRegistriesYAML = `mirrors:
  "registry.localhost:5000":
    endpoint:
      - http://registry.localhost:5000
  "registry.localhost:5050":
    endpoint:
      - http://registry.localhost:5000
  "localhost:5050":
    endpoint:
      - http://registry.localhost:5000
`

// writeFallbackRegistriesYAML writes the canonical mirror config to a
// temp file and returns the path. Caller is responsible for removing
// it after `k3d cluster create` returns.
func writeFallbackRegistriesYAML() (string, error) {
	f, err := os.CreateTemp("", "forge-k3d-registries-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(fallbackRegistriesYAML); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func buildAndPushLocal(ctx context.Context, cfg *config.ProjectConfig, tag, targetArchFlag string) error {
	registry := "localhost:5050"
	if reg := k8sClusterRegistryForEnv(ctx, "dev"); reg != "" {
		registry = reg
	}

	// Build and push the single project image from root Dockerfile.
	dockerfile := "Dockerfile"
	if _, err := os.Stat(dockerfile); os.IsNotExist(err) {
		fmt.Printf("  Skipping %s (no Dockerfile)\n", cfg.Name)
		return nil
	}

	imageRef := fmt.Sprintf("%s/%s:%s", registry, cfg.Name, tag)

	// Skip the rebuild if the image is already present at the tag (e.g.
	// the user just ran `forge build --push` against the same registry).
	// `docker manifest inspect` is cheap (HEAD against the registry) and
	// avoids an O(minutes) docker build + push on the hot path.
	if imageExistsInRegistry(ctx, imageRef) {
		fmt.Printf("  %s already present — skipping rebuild.\n", imageRef)
		return nil
	}

	fmt.Printf("  Building and pushing %s...\n", imageRef)

	// Resolve cross-compile target: --target-arch flag > forge.yaml
	// deploy.target_arch > "amd64" (k8s host default). When the resolved
	// target equals the host arch, no --platform flag is emitted.
	crossArch := resolveDeployArch(cfg.Deploy.TargetArch, targetArchFlag)

	buildArgs := []string{"build"}
	if crossArch != "" {
		buildArgs = append(buildArgs, "--platform=linux/"+crossArch)
		fmt.Printf("  [build] cross-compiling for linux/%s (host: %s/%s)\n",
			crossArch, runtime.GOOS, runtime.GOARCH)
	}
	buildArgs = append(buildArgs, "-t", imageRef)
	// Apply docker.build_contexts from forge.yaml so sibling-checkout
	// replace directives resolve in the deploy-time rebuild too. Shares
	// the build.go helper so the path-resolution + scheme passthrough
	// semantics stay in lockstep across `forge build --docker` and
	// `forge deploy`.
	buildArgs = appendBuildContexts(buildArgs, cfg, "")
	buildArgs = append(buildArgs, "-f", dockerfile, ".")
	buildCmd := exec.CommandContext(ctx, "docker", buildArgs...)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("docker build for %s failed: %w", cfg.Name, err)
	}

	pushCmd := exec.CommandContext(ctx, "docker", "push", imageRef)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("docker push for %s failed: %w", cfg.Name, err)
	}

	return nil
}

// resolveDeployArch picks the target GOARCH for a deploy build. The
// dispatch order is: explicit --target-arch override, then forge.yaml's
// deploy.target_arch, then the "amd64" default (which reflects the
// empirical reality that most k8s nodes are amd64). Returns the empty
// string when the resolved target equals runtime.GOARCH — the empty
// return signals callers that no --platform flag is needed.
//
// Unlike resolveBuildArch in build.go, this function always falls back
// to amd64 (i.e. deploy is treated as the docker-context case in
// build.go). `forge deploy` always builds an image destined for a
// cluster node, so the "no cross-compile, use host arch" outcome
// happens only when host == target.
func resolveDeployArch(cfgArch, flagArch string) string {
	target := flagArch
	if target == "" {
		target = cfgArch
	}
	if target == "" {
		target = "amd64"
	}
	if target == runtime.GOARCH {
		return ""
	}
	return target
}

// imageExistsInRegistry returns true when `docker manifest inspect` can
// resolve the given image:tag, i.e. it's already in the registry. Used
// by buildAndPushLocal to short-circuit the redundant deploy-time build
// when `forge build --push` has already pushed the same tag. Any error
// (manifest absent, registry unreachable, manifest API disabled) yields
// false so we fall through to the normal build+push path.
//
// For local/HTTP registries we use --insecure so the check works against
// the dev k3d registry (localhost:5051), which doesn't speak TLS.
func imageExistsInRegistry(ctx context.Context, ref string) bool {
	args := []string{"manifest", "inspect"}
	if isInsecureRegistry(ref) {
		args = append(args, "--insecure")
	}
	args = append(args, ref)
	cmd := exec.CommandContext(ctx, "docker", args...)
	return cmd.Run() == nil
}

// isInsecureRegistry reports whether the image ref points at a registry
// that should be treated as HTTP. We treat localhost / 127.0.0.1 /
// registry.localhost as insecure — these are the dev-cluster k3d
// registries forge sets up. Anything else (ghcr.io, gcr.io, AR…) is
// HTTPS by default.
func isInsecureRegistry(ref string) bool {
	host, _, _ := strings.Cut(ref, "/")
	hostOnly, _, _ := strings.Cut(host, ":")
	switch hostOnly {
	case "localhost", "127.0.0.1", "registry.localhost":
		return true
	}
	return false
}

// deployPreflightInput bundles what runDeployPreflight needs to render the
// env's manifests and run the deployability checks against the live target.
type deployPreflightInput struct {
	mainK     string
	imageTag  string
	namespace string
	env       string
	envCfgKV  map[string]string
	deployCtx string
	targets   []string
	// imageDigests is the per-image name→digest map (image NAME →
	// "sha256:..."). Threaded into the manifest render so the preflight
	// checks the SAME `<image>@<digest>` refs the apply will ship — a
	// preflight that pinned every image to one env-wide digest would pass
	// or fail on the wrong refs. Empty on the no-digest path.
	imageDigests map[string]string
	// targetArch is the DECLARED node architecture of the target cluster
	// (GOARCH form, from the env's KCL deploy.Cluster.platform). Threads into
	// PreflightOpts.TargetArch to drive the arch gate. EMPTY when the env
	// hasn't declared a platform — the gate is inert (WARN-don't-block), so
	// envs that predate the platform field (incl. local e2e) aren't
	// false-failed.
	targetArch string
	// requiredSecrets are the env's DECLARED external Secret prerequisites
	// (forge.ExternalSecret) — out-of-band Secrets the deploy depends on but
	// forge does NOT create. Threaded into PreflightOpts.RequiredSecrets so a
	// declared-required-but-absent Secret/key BLOCKS the deploy (and a
	// value_group byte mismatch is caught). Empty => no declared prereqs.
	requiredSecrets []cluster.RequiredSecret
	// secretSupply is the env's bundle-internal Secret SUPPLY for the
	// render-time back-propagation gate: the Secrets the bundle PROVIDES via a
	// forge.KubeconfigSecret mint, a forge.ExternalSecret promise, or a
	// rendered secret_provider Secret. Threaded into PreflightOpts.SecretSupply
	// so a workload mounting a Secret NOTHING declares fails at render time
	// (the silent FailedMount fix). Rendered-stream Secrets are collected from
	// the manifests directly, so this carries only the entity-derived supply.
	secretSupply []cluster.SecretSupply
}

// requiredSecretsForPreflight projects the env's declared external Secret
// prerequisites (forge.ExternalSecret) onto the cluster-package
// RequiredSecret shape the preflight consumes. Keeps the cluster package
// decoupled from the cli entity types. nil entities / no declarations =>
// nil (the preflight check stays inert).
func requiredSecretsForPreflight(entities *KCLEntities) []cluster.RequiredSecret {
	if entities == nil || len(entities.RequiredSecrets) == 0 {
		return nil
	}
	out := make([]cluster.RequiredSecret, 0, len(entities.RequiredSecrets))
	for _, s := range entities.RequiredSecrets {
		out = append(out, cluster.RequiredSecret{
			Name:       s.Name,
			Namespace:  s.Namespace,
			Keys:       s.Keys,
			ValueGroup: s.ValueGroup,
		})
	}
	return out
}

// secretSupplyForPreflight projects the env's bundle-internal Secret SUPPLY
// onto the cluster-package SecretSupply shape the render-time back-propagation
// gate consumes — keeping the cluster package decoupled from the cli entity
// types (the same pattern requiredSecretsForPreflight uses). It enumerates the
// Secrets the bundle PROVIDES that aren't rendered as a `kind: Secret`
// document (those the gate collects from the manifest stream itself):
//
//   - forge.KubeconfigSecret mints (entities.KubeconfigSecrets) — forge mints
//     each fresh every up and applies it as a k8s Secret. The control-plane bug
//     that motivated this gate was a mounted Secret with NO matching
//     KubeconfigSecret; declaring one is the supply that satisfies the demand.
//   - forge.ExternalSecret promises (entities.RequiredSecrets) — the author's
//     explicit out-of-band promise the Secret exists. Counts as SATISFIED here;
//     the LIVE preflight separately verifies it's actually provisioned.
//   - rendered secret_provider Secrets (SecretProvider.Secrets, Type=="rendered")
//     — forge renders + applies these per cluster; recorded as supply for
//     completeness even though they typically also appear in the manifest
//     stream.
//
// nil entities / no supply => nil (only rendered-stream Secrets then count).
func secretSupplyForPreflight(entities *KCLEntities) []cluster.SecretSupply {
	if entities == nil {
		return nil
	}
	var out []cluster.SecretSupply
	for _, k := range entities.KubeconfigSecrets {
		out = append(out, cluster.SecretSupply{
			Name:      k.Name,
			Namespace: k.Namespace,
			Kind:      cluster.SupplyKubeconfigSecret,
		})
	}
	for _, s := range entities.RequiredSecrets {
		out = append(out, cluster.SecretSupply{
			Name:      s.Name,
			Namespace: s.Namespace,
			Kind:      cluster.SupplyExternalSecret,
		})
	}
	if entities.SecretProvider != nil {
		for _, s := range entities.SecretProvider.Secrets {
			out = append(out, cluster.SecretSupply{
				Name: s.Name,
				Kind: cluster.SupplyRenderedManifest,
			})
		}
	}
	return out
}

// runDeployPreflight renders the env's manifest bundle and runs the
// cluster.Preflight deployability checks (referenced Secret keys present in
// the live cluster + container images present in the registry) BEFORE the
// first apply. Returns a grouped, actionable error when anything is missing
// — nothing is applied. A no-op when the bundle has nothing to check.
//
// The Secret check is gated to REMOTE clusters: on a local dev cluster
// forge applies the dotenv-projected Secrets itself moments after this
// runs, so they don't exist yet and checking them would false-fail the
// inner loop. Local-registry images are skipped via cluster.LocalImageRef
// for the same don't-break-dev-loop reason. Image checks DO still run on a
// local cluster for remote-registry images (e.g. ghcr.io refs in a local
// test), since those are reachable and a miss is real.
func runDeployPreflight(ctx context.Context, in deployPreflightInput) error {
	// Render the same manifest stream the apply will consume. Applying the
	// --target filter keeps the preflight scoped to exactly the apps about
	// to be applied (a targeted single-app deploy shouldn't fail on another
	// app's missing image).
	manifests, err := cluster.RenderManifests(ctx, in.mainK, in.imageTag, in.namespace, in.env, in.envCfgKV, in.imageDigests)
	if err != nil {
		return fmt.Errorf("preflight: render manifests: %w", err)
	}
	if len(in.targets) > 0 {
		// Same exclusive --target filter the apply uses (keep iff the
		// manifest's KCL-declared group ∈ targets) so the preflight checks
		// EXACTLY the manifests about to be applied.
		manifests = cluster.SelectManifestsByGroup(manifests, in.targets)
	}

	opts := cluster.PreflightOpts{
		Manifests:    manifests,
		Namespace:    in.namespace,
		Images:       cluster.DockerImageChecker{},
		ImageArch:    cluster.DockerImageArchChecker{},
		TargetArch:   in.targetArch,
		SkipImageRef: cluster.LocalImageRef,
		// Render-time secret back-propagation supply. Set UNCONDITIONALLY (not
		// gated behind the remote-cluster block below): the back-propagation
		// gate is a pure, no-cluster check that runs on every deploy — incl.
		// local dev clusters and --dry-run — so a workload mounting a Secret
		// nothing declares fails fast at render time rather than rotting on
		// FailedMount. Rendered-stream Secrets are collected from the manifests;
		// this carries the KubeconfigSecret / ExternalSecret / rendered-provider
		// supply.
		SecretSupply: in.secretSupply,
	}
	// Secret + ConfigMap checks only against a REMOTE cluster (see
	// docstring). An empty or local context leaves opts.Secrets /
	// opts.ConfigMaps nil → cluster.Preflight skips those checks entirely.
	// Both are gated identically: on a local dev cluster forge applies the
	// projected Secret AND ConfigMap moments after this runs, so neither
	// exists yet and checking would false-fail the inner loop.
	if in.deployCtx != "" && !isLocalCluster(in.deployCtx) {
		opts.Context = in.deployCtx
		opts.Secrets = cluster.KubectlSecretGetter{}
		opts.ConfigMaps = cluster.KubectlConfigMapGetter{}
		// CRD / served-kind gate: verify the cluster serves every NON-CORE kind
		// the bundle renders (e.g. GRPCRoute needs the Gateway API channel)
		// BEFORE the apply. Without it a missing CRD fails `no matches for kind`
		// mid-rollout, AFTER other resources already applied. Gated to remote
		// clusters like the Secret check — the local dev cluster's CRDs are
		// reconciled by the same up/deploy flow.
		opts.ServedKinds = cluster.KubectlServedKinds{}
		// Declared external Secret prerequisites (forge.ExternalSecret): each
		// in its OWN declared namespace, verified against the live target. A
		// declared-required-but-absent Secret/key BLOCKS — the converse of
		// "render green then ACME hangs silently". The byte-match group compare
		// needs the live values, hence the value getter. Gated to remote
		// clusters like the Secret check (a local dev cluster's out-of-band
		// Secrets are provisioned by the same up flow).
		opts.RequiredSecrets = in.requiredSecrets
		opts.SecretValues = cluster.KubectlSecretValueGetter{}
		// Verify images using the CLUSTER's pull credentials (the bundle's
		// imagePullSecrets), not just the local docker daemon: when the local
		// daemon lacks creds for a private registry, an auth-denied lookup is a
		// FALSE block for an image the cluster can pull. The resolver reads the
		// pull Secrets' .dockerconfigjson from the target cluster so an
		// auth-denied lookup is retried from the cluster's perspective for a
		// TRUE verdict. Gated to remote clusters (same as the Secret check) —
		// local k3d images are skipped via LocalImageRef anyway.
		opts.PullCreds = cluster.KubectlPullCredsResolver{}
	}
	return cluster.Preflight(ctx, opts)
}

// expectedClusterForEnv returns the expected kubectl context name for
// an environment. Resolution priority:
//  1. The rendered KCL's first K8sCluster.cluster for env <envName>
//  2. For dev: k3d-<project-name>
//  3. Empty string — no expectation declared (skip the guard)
//
// Reads the rendered KCL via RenderKCL using a background context so
// the lookup remains usable from the explain path where we don't carry
// a request context. Failures fall through to the dev default / empty
// — the env-cluster guard is a recommendation, not a hard dependency.
func expectedClusterForEnv(ctx context.Context, cfg *config.ProjectConfig, envName string) string {
	if cluster := firstK8sClusterField(ctx, envName, "cluster"); cluster != "" {
		return cluster
	}
	if envName == "dev" && cfg != nil {
		// Dev's default is the k3d cluster forge deploy dev creates.
		return "k3d-" + cfg.Name
	}
	return ""
}

// firstK8sClusterField reads the rendered KCL for env and returns the
// requested field ("cluster" / "namespace" / "registry" / "domain")
// from the first service whose Deploy is K8sCluster-shaped. Returns ""
// when KCL can't be rendered, no service is cluster-shaped, or the
// requested field is empty across every service.
func firstK8sClusterField(ctx context.Context, envName, field string) string {
	if envName == "" {
		return ""
	}
	entities, err := RenderKCL(ctx, projectDirForKCL(), envName)
	if err != nil || entities == nil {
		return ""
	}
	for _, svc := range entities.Services {
		if svc.Deploy.Type != "cluster" || svc.Deploy.Cluster == nil {
			continue
		}
		c := svc.Deploy.Cluster
		switch field {
		case "cluster":
			if c.Cluster != "" {
				return c.Cluster
			}
		case "namespace":
			if c.Namespace != "" {
				return c.Namespace
			}
		case "registry":
			if c.Registry != "" {
				return c.Registry
			}
		case "domain":
			if c.Domain != "" {
				return c.Domain
			}
		}
	}
	// Fallback for the manifests-only render shape: a project whose
	// main.k emits only `manifests` (no `output = forge.render(_bundle)`
	// entity echo) yields no cluster-shaped service entity above, so the
	// loop finds nothing. The namespace is still recoverable from the
	// rendered objects' metadata.namespace; the cluster (kubectl context)
	// is not — it isn't a field on any k8s object — so only "namespace"
	// has a manifest fallback. A project in this shape that wants the
	// declared-context guard (and any k8s write at all) must echo `output`
	// so forge.K8sCluster.cluster is recoverable; there is no CLI escape
	// hatch, and the apply chokepoint refuses an empty context.
	if field == "namespace" && entities.ManifestNamespace != "" {
		return entities.ManifestNamespace
	}
	return ""
}

// k8sClusterNamespaceForEnv reads the rendered KCL and returns the
// first K8sCluster.namespace declared for env. Returns "" when no
// cluster-shaped service is declared or the field is unset.
func k8sClusterNamespaceForEnv(ctx context.Context, envName string) string {
	return firstK8sClusterField(ctx, envName, "namespace")
}

// k8sClusterRegistryForEnv reads the rendered KCL and returns the
// first K8sCluster.registry declared for env. Returns "" when no
// cluster-shaped service is declared or the field is unset.
func k8sClusterRegistryForEnv(ctx context.Context, envName string) string {
	return firstK8sClusterField(ctx, envName, "registry")
}

// verifyKubectlContext is the DECLARATIVE env-cluster guard. The env's
// KCL declares its target cluster (`forge.K8sCluster.cluster`), and that
// name IS the kubectl context the deploy applies to (threaded per-command
// via --context). So the guard no longer cares what context is currently
// ACTIVE — it deliberately does NOT refuse on a current-vs-expected
// mismatch (that would block a valid deploy: we apply to the declared
// cluster regardless of the active context). Instead it fails fast when
// the declared cluster has no matching kubectl context, listing the
// available contexts — the check that makes a wrong-cluster deploy
// impossible while never depending on the globally-switched active
// context (the cross-cluster contamination incident).
//
// There is no CLI override: the declared cluster is the only source. An
// env with no declared cluster skips the guard (host-only / compose) —
// those envs run no kubectl writes, so there's nothing to guard.
func verifyKubectlContext(ctx context.Context, cfg *config.ProjectConfig, envName string) error {
	expected := expectedClusterForEnv(ctx, cfg, envName)
	if expected == "" {
		// No expectation declared for this env. Print a one-liner
		// reminder so users know they can lock it down, but don't
		// block the deploy (backwards-compatible default).
		fmt.Printf("Note: no forge.K8sCluster.cluster declared in deploy/kcl/%s/main.k — declared-cluster guard skipped.\n", envName)
		return nil
	}

	available, err := kubectlContextNames(ctx)
	if err != nil {
		return err
	}
	if err := declaredContextExistsVerdict(envName, expected, available); err != nil {
		return err
	}
	fmt.Printf("kubectl context: %s (declared by env %s; applied per-command via the declared cluster)\n", expected, envName)
	return nil
}

// kubectlContextNames returns the set of context names declared in the
// active kubeconfig (`kubectl config get-contexts -o name`). Returns an
// error when kubectl isn't installed / configured — the caller turns
// that into a clear deploy-time failure rather than silently applying
// to whatever's active.
func kubectlContextNames(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "config", "get-contexts", "-o", "name").Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl config get-contexts: %w (is kubectl installed and configured?)", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// declaredContextExistsVerdict is the pure core of the declarative
// fail-fast guard: the declared cluster name IS the kubectl context the
// deploy will apply to, so if it isn't present in the kubeconfig we
// refuse with a clear error listing the available contexts. Lifted out so
// unit tests exercise the missing-context path without shelling to
// kubectl. An empty declared value (env declares no cluster) is a no-op.
func declaredContextExistsVerdict(envName, declared string, available []string) error {
	if declared == "" {
		return nil
	}
	for _, c := range available {
		if c == declared {
			return nil
		}
	}
	return fmt.Errorf(
		"env %q declares cluster %q but no such kubectl context exists.\n"+
			"  available contexts: %s\n"+
			"\n"+
			"refusing to deploy (the declared cluster is the kubectl context — this is what makes wrong-cluster deploys impossible). Fix with one of:\n"+
			"  - add the context to your kubeconfig (e.g. `gcloud container clusters get-credentials ...`)\n"+
			"  - correct forge.K8sCluster.cluster in the env's KCL to match an existing context",
		envName, declared, emptyAs(strings.Join(available, ", "), "(none)"))
}

// verifyDeclaredContextsExist is the post-build, MULTI-CLUSTER completion
// of the declarative guard: every K8sCluster group declares its target
// cluster (group.Cluster, from KCL `forge.K8sCluster.cluster`), and that
// name IS the kubectl context we'll apply that group to. If any declared
// context isn't present in the kubeconfig we refuse the deploy listing the
// available contexts — instead of silently landing on whatever context is
// currently active. (The single-cluster case is already caught earlier by
// verifyKubectlContext, before the image build; this covers a rare env
// whose groups span multiple clusters.)
//
// There is no CLI override. Groups without a declared cluster (host-only
// / compose, dev env with blank cluster) are skipped — those run no
// kubectl writes, so there's nothing to guard.
func verifyDeclaredContextsExist(ctx context.Context, groups []deploytarget.ServiceGroup) error {
	// Collect the distinct declared clusters across the K8sCluster
	// groups (a multi-cluster env applies each group to its own).
	declared := map[string]struct{}{}
	for _, g := range groups {
		if g.ProviderID == "k8s-cluster" && g.Cluster != "" {
			declared[g.Cluster] = struct{}{}
		}
	}
	if len(declared) == 0 {
		return nil
	}
	available, err := kubectlContextNames(ctx)
	if err != nil {
		return err
	}
	// Deterministic order so the error names the same cluster every run.
	var declaredList []string
	for c := range declared {
		declaredList = append(declaredList, c)
	}
	sort.Strings(declaredList)
	for _, c := range declaredList {
		if verr := declaredContextExistsVerdict("(deploy)", c, available); verr != nil {
			return verr
		}
	}
	return nil
}

// applyK8sSecretsFromProvider renders + applies plaintext k8s Secret
// manifests from a dotenv secret_provider, BEFORE the Deployments roll
// out so each Deployment's secretKeyRef resolves on first schedule.
//
// Sequence:
//  1. Build the provider and fail-fast validate that every declared
//     cluster secret ref resolves (no-op for external/none providers —
//     forge can't see those values, they're provisioned out-of-band).
//  2. For a dotenv provider ONLY: guard that every targeted k8s cluster
//     is a recognized LOCAL dev cluster. dotenv renders PLAINTEXT
//     Secrets; shipping those into a remote/prod cluster is a footgun,
//     so we refuse and point the user at forge.ExternalSecrets {}.
//  3. Render the Secret manifests and apply them via the same
//     cluster.KubectlApply path the Deployments use.
//
// external/none providers produce no manifests (RenderK8sSecrets returns
// nil), so this is a no-op for them beyond the validation gate.
func applyK8sSecretsFromProvider(ctx context.Context, entities *KCLEntities, groups []deploytarget.ServiceGroup, namespace, kubeContext, envName string, dryRun bool) error {
	// RenderedSecrets is a distinct provider shape: explicit named Secrets
	// (name + per-key source) applied PER CLUSTER — each Secret lands ONLY
	// in the cluster(s) whose services reference it, never projected across
	// the trust boundary. Handled by its own per-group path.
	if entities != nil && entities.SecretProvider != nil && entities.SecretProvider.Type == "rendered" {
		return applyRenderedSecretsPerGroup(ctx, entities, groups, namespace, envName, dryRun)
	}

	prov, err := secretProviderFromEntities(entities, projectDirForKCL())
	if err != nil {
		return fmt.Errorf("secret provider: %w", err)
	}
	// Fail-fast: declared cluster refs must resolve (no-op for
	// external/none).
	dotenvPath := ""
	if entities != nil && entities.SecretProvider != nil {
		dotenvPath = entities.SecretProvider.Path
	}
	if err := secrets.ValidateDeclaredRefs(prov, secretRefsForK8sServices(entities), dotenvPath); err != nil {
		return err
	}
	if prov.Kind() != "dotenv" {
		return nil
	}

	// GUARD: dotenv renders PLAINTEXT Secrets — local clusters only.
	// Reject if any k8s-cluster group targets a non-local cluster.
	for _, g := range groups {
		if g.ProviderID != "k8s-cluster" {
			continue
		}
		if !isLocalCluster(g.Cluster) {
			return fmt.Errorf(
				"secret_provider 'dotenv' renders plaintext Secrets and is for LOCAL clusters only; target cluster %q is not local. "+
					"Use secret_provider = forge.ExternalSecrets {} (Secrets provisioned out-of-band) for remote clusters.",
				g.Cluster)
		}
	}

	mans := secrets.RenderK8sSecrets(prov, secretRefsForK8sServices(entities), namespace)
	if len(mans) == 0 {
		return nil
	}

	// Marshal the []map[string]any into the `---`-separated YAML document
	// stream cluster.KubectlApply consumes (identical shape to
	// RenderManifests' output that the Deployment apply uses).
	stream, merr := marshalManifestStream(mans)
	if merr != nil {
		return fmt.Errorf("render k8s secrets: %w", merr)
	}

	if dryRun {
		fmt.Println("\n--- Generated Secret Manifests (dry-run) ---")
		fmt.Println(stream)
		fmt.Println("--- End Secret Manifests ---")
		return nil
	}

	// The secret manifests are namespace-scoped, but the Namespace object
	// itself lives in the MAIN manifest stream applied AFTER this — so on a
	// fresh cluster the namespace doesn't exist yet and the secret apply
	// fails "namespaces \"…\" not found". Ensure it first (idempotent; the
	// later full apply re-applies it with labels). See cluster.EnsureNamespace.
	if err := cluster.EnsureNamespace(ctx, kubeContext, namespace); err != nil {
		return fmt.Errorf("ensure namespace %q before secrets: %w", namespace, err)
	}

	fmt.Printf("Applying %d secret manifest(s) into namespace %s...\n", len(mans), namespace)
	if err := cluster.KubectlApply(ctx, kubeContext, stream); err != nil {
		return fmt.Errorf("apply k8s secrets: %w", err)
	}
	return nil
}

// applyRenderedSecretsPerGroup renders + applies a RenderedSecrets
// provider's declared Secrets, scoping each Secret to ONLY the cluster(s)
// whose services reference it. This is the trust-safe, multi-cluster
// generalization of the env-wide dotenv apply: a Secret declared for the
// control-plane cluster never lands in the workload cluster (and vice
// versa) — each cluster gets only the Secrets its own services declare.
//
// Sourcing: `from="dotenv"` keys resolve from `.env.<env>` (gitignored);
// `from="literal"` keys are inlined but ONLY in dev/e2e (the Go guard in
// secrets.RenderDeclaredSecrets mirrors the KCL check). Local-cluster
// only — like DotenvSecrets, this renders PLAINTEXT Secrets, so a
// non-local target cluster is refused.
func applyRenderedSecretsPerGroup(ctx context.Context, entities *KCLEntities, groups []deploytarget.ServiceGroup, namespace, envName string, dryRun bool) error {
	declared := declaredSecretsFromEntities(entities)
	if len(declared) == 0 {
		return nil
	}

	// Dotenv source for `from="dotenv"` keys: the gitignored `.env.<env>`.
	dotenvPath := filepath.Join(projectDirForKCL(), ".env."+envName)
	dot, derr := secrets.NewProvider(&secrets.ProviderConfig{Type: "dotenv", Path: dotenvPath})
	if derr != nil {
		return fmt.Errorf("rendered secrets dotenv source: %w", derr)
	}

	// Index declared Secrets by name for the per-group lookup.
	byName := make(map[string]secrets.DeclaredSecret, len(declared))
	for _, d := range declared {
		byName[d.Name] = d
	}

	for _, g := range groups {
		if g.ProviderID != "k8s-cluster" {
			continue
		}
		// GUARD: PLAINTEXT Secrets — local clusters only.
		if !isLocalCluster(g.Cluster) {
			return fmt.Errorf(
				"secret_provider 'rendered' renders plaintext Secrets and is for LOCAL clusters only; target cluster %q is not local. "+
					"Use secret_provider = forge.ExternalSecrets {} for remote clusters.",
				g.Cluster)
		}

		// Which declared Secrets do THIS group's services reference? Only
		// those land in this group's cluster — never project a Secret
		// across the trust boundary.
		refNames := referencedSecretNamesForGroup(entities, g)
		var groupSecrets []secrets.DeclaredSecret
		for name := range refNames {
			if d, ok := byName[name]; ok {
				groupSecrets = append(groupSecrets, d)
			}
		}
		if len(groupSecrets) == 0 {
			continue
		}

		ns := g.Namespace
		if ns == "" {
			ns = namespace
		}
		mans, rerr := secrets.RenderDeclaredSecrets(groupSecrets, dot, envName, ns)
		if rerr != nil {
			return rerr
		}
		if len(mans) == 0 {
			continue
		}
		stream, merr := marshalManifestStream(mans)
		if merr != nil {
			return fmt.Errorf("render rendered secrets: %w", merr)
		}
		if dryRun {
			fmt.Printf("\n--- Rendered Secret Manifests for cluster %s (dry-run) ---\n", g.Cluster)
			fmt.Println(stream)
			fmt.Println("--- End Rendered Secret Manifests ---")
			continue
		}
		if err := cluster.EnsureNamespace(ctx, g.Cluster, ns); err != nil {
			return fmt.Errorf("ensure namespace %q in %q before rendered secrets: %w", ns, g.Cluster, err)
		}
		fmt.Printf("Applying %d rendered Secret(s) into %s/%s...\n", len(mans), g.Cluster, ns)
		if err := cluster.KubectlApply(ctx, g.Cluster, stream); err != nil {
			return fmt.Errorf("apply rendered secrets to %s: %w", g.Cluster, err)
		}
	}
	return nil
}

// declaredSecretsFromEntities maps the cli RenderedSecretEntity set to the
// secrets-package DeclaredSecret shape (keeping the secrets package
// decoupled from cli). Returns nil when the provider isn't "rendered".
func declaredSecretsFromEntities(entities *KCLEntities) []secrets.DeclaredSecret {
	if entities == nil || entities.SecretProvider == nil || entities.SecretProvider.Type != "rendered" {
		return nil
	}
	var out []secrets.DeclaredSecret
	for _, s := range entities.SecretProvider.Secrets {
		keys := make(map[string]secrets.DeclaredSecretKey, len(s.Keys))
		for k, src := range s.Keys {
			keys[k] = secrets.DeclaredSecretKey{From: src.From, Key: src.Key, Value: src.Value}
		}
		out = append(out, secrets.DeclaredSecret{Name: s.Name, Keys: keys})
	}
	return out
}

// referencedSecretNamesForGroup returns the set of Secret names the
// group's services reference via their env-var secret_refs. This is the
// scoping key: a declared Secret lands in a cluster ONLY when one of that
// cluster's services names it. The group carries service names; the
// entities carry each service's secret_refs.
func referencedSecretNamesForGroup(entities *KCLEntities, g deploytarget.ServiceGroup) map[string]struct{} {
	inGroup := map[string]struct{}{}
	for _, rs := range g.Services {
		inGroup[rs.Name] = struct{}{}
	}
	names := map[string]struct{}{}
	for i := range entities.Services {
		s := &entities.Services[i]
		if _, ok := inGroup[s.Name]; !ok {
			continue
		}
		for _, ref := range secretRefsForService(s) {
			if ref.SecretName != "" {
				names[ref.SecretName] = struct{}{}
			}
		}
	}
	return names
}

// marshalManifestStream serialises a list of manifest maps into the
// `---`-separated multi-doc YAML stream `kubectl apply -f -` consumes —
// the same shape cluster.extractManifests produces for the Deployment
// apply, so KubectlApply handles both identically.
func marshalManifestStream(mans []map[string]any) (string, error) {
	var sb strings.Builder
	for i, m := range mans {
		if i > 0 {
			sb.WriteString("---\n")
		}
		b, err := yaml.Marshal(m)
		if err != nil {
			return "", fmt.Errorf("marshal manifest %d: %w", i, err)
		}
		sb.Write(b)
	}
	return sb.String(), nil
}

// isLocalCluster reports whether a cluster name / kubectl context is
// clearly a local dev cluster — the only place plaintext dotenv Secrets
// are safe to project. Recognizes the k3d / kind context prefixes and
// the docker-desktop / minikube / rancher-desktop / colima / orbstack
// local-runtime markers. An empty name is treated as non-local so a
// missing cluster declaration can't silently bypass the guard.
func isLocalCluster(name string) bool {
	if name == "" {
		return false
	}
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(n, "k3d-") || strings.HasPrefix(n, "kind-") {
		return true
	}
	for _, marker := range []string{"docker-desktop", "minikube", "rancher-desktop", "colima", "orbstack"} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}
