package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

// defaultEnvVerifyTimeout bounds the cluster read. Generous enough for a cold
// cloud API server behind a fresh credential exchange, short enough that an
// unreachable cluster fails a CI job rather than hanging it.
const defaultEnvVerifyTimeout = 60 * time.Second

// envVerifyReport is the `--json` output contract.
//
// It carries exactly what the text report carries, in the same five states,
// and `OK` is false in exactly the cases where text mode exits non-zero — the
// house convention (see internal/cli/lint/lint_json.go's header) is that the
// two modes never disagree about the verdict, only about the rendering.
//
// Extensions are ADDITIVE: new fields may be added, but no field is renamed or
// repurposed, so a consumer reading `state` and `tally` keeps working.
type envVerifyReport struct {
	// Env is the environment name as given on the command line.
	Env string `json:"env"`
	// Bound is false for an env that has never been promoted. That is NOT a
	// failure — it has declared nothing, so there is nothing to be wrong
	// about — and it exits 0 with OK true. It is a separate field rather
	// than an empty Release so a consumer can tell "never promoted" from a
	// binding that somehow carries a blank version.
	Bound bool `json:"bound"`
	// Release is the version label the binding names. Empty when unbound.
	Release string `json:"release,omitempty"`
	// PromotedAt is RFC3339 wall-clock for when the env was PROMOTED — NOT
	// when it was deployed. Promotion writes a pointer; deployment moves
	// bytes, and the gap between the two is the entire thing this command
	// exists to measure. A consumer treating this as a deploy time has
	// reintroduced the bug: it would read as "shipped" for an env that was
	// promoted and then never deployed at all.
	PromotedAt string `json:"promoted_at,omitempty"`
	// KubeContext and Namespace are where the cluster was read, resolved
	// declaratively from the env's KCL. Empty when the target could not be
	// resolved, which is itself an UNREACHABLE condition.
	KubeContext string `json:"kube_context,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	// Images is one verdict per DECLARED image. Always non-nil so consumers
	// see `[]` rather than `null`.
	Images []imageVerification `json:"images"`
	// Tally counts the five states. Unreachable stays its own bucket.
	Tally envVerifyTally `json:"tally"`
	// OK is false exactly when text mode exits non-zero: drift, missing, or
	// unreachable. Untagged does NOT flip it — nothing has been proven
	// wrong — matching the text mode's note-and-exit-0 behaviour.
	OK bool `json:"ok"`
	// Detail carries the one-line human reason for a non-OK result, or the
	// explanation of an unbound env. Empty on a clean verify.
	Detail string `json:"detail,omitempty"`
}

// newEnvVerifyCmd is `forge env verify <environment>`.
func newEnvVerifyCmd() *cobra.Command {
	var timeout time.Duration
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "verify <environment>",
		Short: "Prove an environment is RUNNING the release its binding claims",
		Long: `Compare what an environment is actually running against what the binding
ledger says it should run.

WHY THIS EXISTS. ` + "`forge env promote`" + ` writes a binding — env → release, with
the per-image digests frozen at promote time. But ` + "`promoted_at`" + ` is stamped when
the env is PROMOTED, not when it is deployed. Cutting a release is not shipping
it, and until this command nothing in the tooling could tell the two apart: a
binding claiming v1.5.13 proves only that someone ran promote. Confirming prod
had actually moved meant reading live digests by hand, one
` + "`kubectl get deploy -o jsonpath`" + ` per workload. This is that check, as a command.

WHAT IS READ. The cluster, directly — the same kubectl path ` + "`forge env deploy`" + `
writes through, against the context the env's KCL declares
(` + "`forge.K8sCluster.cluster`" + `). Every workload kind that can carry an application
image is inspected (Deployments, StatefulSets, DaemonSets, CronJobs, Jobs), not
just Deployments: forge renders CronJobs for ` + "`kind = \"cron\"`" + `, and a verifier
blind to those would report clean while a drifted cron ran old bytes.

FIVE OUTCOMES, NOT TWO:

  MATCH        the cluster runs the digest the binding declares.
  DRIFT        the cluster runs a DIFFERENT digest. Both are reported.
  MISSING      declared by the binding, running nowhere — never deployed,
               or deleted since.
  UNTAGGED     the workload runs by mutable tag, so there is no digest to
               compare. Nothing is proven either way (a --no-digest deploy).
  UNREACHABLE  the cluster could not be read — no context, auth failure,
               timeout. Says NOTHING about the environment.

An env with NO binding is not a failure. It has never been promoted, so there
is nothing to verify against, and the command says so and exits 0.

EXIT CODES:

  0  everything declared is running (or nothing is declared)
  1  at least one image DRIFTED or is MISSING
  2  the cluster could not be read, and nothing outright drifted

Exit 2 is separate from 1 on purpose. A VPN drop or an expired credential is
not evidence that a release is wrong, and a gate that reports drift and a
network failure with the same code gets switched off the first week it is
wrong about one of them.

--json emits the same verdict as a machine-readable report, with IDENTICAL exit
codes. All five states survive into it as lowercase strings, so "unreachable"
stays distinguishable from both "match" and "drift"; "ok" is false exactly when
text mode exits non-zero. An unbound env reports {"bound": false, "ok": true}.

Examples:
  forge env verify prod                 # did prod actually receive its release?
  forge env verify staging --timeout 2m # slow or distant cluster
  forge env verify prod --json | jq -r '.images[] | select(.state == "drift")'`,
		Args: cobra.ExactArgs(1),
		// The command's findings ARE its output; a cobra usage dump on a
		// drift failure would bury them under the flag list.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvVerify(cmd.Context(), args[0], envVerifyOptions{
				Timeout:  timeout,
				JSON:     jsonOut,
				Lister:   kubectlImageLister{},
				Resolver: kclTargetResolver{},
			})
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", defaultEnvVerifyTimeout, "Maximum time to spend reading the cluster")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON (same exit codes as text mode)")

	return cmd
}

// envTarget is WHERE an environment runs: the kubectl context and the
// namespace. Either may be empty, which means the env's KCL declares no
// cluster — a host-only or compose env, which has nothing to verify.
type envTarget struct {
	KubeContext string
	Namespace   string
}

// envTargetResolver answers "where does this env run".
//
// A seam rather than a direct call because resolving it for real renders the
// env's KCL, which needs a whole project on disk. A unit test asserting the
// DRIFT verdict should not have to construct one — but it must still exercise
// the same code path production does, so the production command injects the
// real resolver rather than leaving this nil and branching on it.
type envTargetResolver interface {
	Resolve(ctx context.Context, projectDir, envName string) envTarget
}

// kclTargetResolver is the production resolver.
//
// The kubectl context is DECLARATIVE: forge.K8sCluster.cluster in the env's
// KCL IS the context name. It is never the ambient current-context — a command
// whose entire output is a claim about a named environment must not silently
// read whichever cluster someone last switched to, or the claim is about an
// unknown cluster.
type kclTargetResolver struct{}

func (kclTargetResolver) Resolve(ctx context.Context, projectDir, envName string) envTarget {
	// A load failure is not fatal: expectedClusterForEnv reads the config
	// only for its dev-env fallback, and a broken forge.yaml has already
	// failed whatever command loaded it first.
	cfg, _ := config.LoadProjectDir(projectDir)
	return envTarget{
		KubeContext: expectedClusterForEnv(ctx, cfg, envName),
		Namespace:   k8sClusterNamespaceForEnv(ctx, envName),
	}
}

// envVerifyOptions carries the flags and the injected seams into the run
// function.
type envVerifyOptions struct {
	Timeout time.Duration
	// JSON switches the RENDERING only. Every verdict, every state and
	// every exit code is computed before either renderer runs, so the two
	// modes cannot disagree about what was found.
	JSON bool
	// Lister reads the cluster.
	Lister clusterImageLister
	// Resolver says where the env runs.
	Resolver envTargetResolver
	// Bindings is the ledger holding what the env is SUPPOSED to run. The
	// third injected seam, for the same reason as the other two: this
	// command's whole job is comparing a declaration against reality, and a
	// test of that comparison should be able to state the declaration
	// directly instead of staging a file on disk to imply it.
	Bindings bindingStore
}

// runEnvVerify resolves the binding, reads the cluster, prints a per-image
// report, and returns an error carrying the right exit code.
func runEnvVerify(ctx context.Context, envName string, opts envVerifyOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultEnvVerifyTimeout
	}
	if opts.Lister == nil {
		opts.Lister = kubectlImageLister{}
	}
	if opts.Resolver == nil {
		opts.Resolver = kclTargetResolver{}
	}

	projectDir := projectDirForKCL()
	if opts.Bindings == nil {
		store, err := bindingStoreFor(ctx, projectDir, envName)
		if err != nil {
			return err
		}
		opts.Bindings = store
	}
	binding, bound, err := opts.Bindings.Current(ctx, envName)
	if err != nil {
		return fmt.Errorf("read the promotion ledger for %s (%s): %w", envName, opts.Bindings.Location(), err)
	}

	// NO BINDING IS NOT A FAILURE. An env that has never been promoted has
	// declared nothing, so there is nothing to be wrong about. Exiting 1
	// here would make the command red for a perfectly healthy env that
	// simply does not use releases, and a permanently-red gate is a deleted
	// gate. Say plainly what the state is and exit 0.
	if !bound {
		const unboundDetail = "no release binding — the environment has never been promoted, so nothing is declared and there is nothing to verify"
		if opts.JSON {
			// Still a complete, valid report. `bound: false` is the
			// machine-readable marker; `ok` stays true because this is
			// a healthy state, not a failure. Images is non-nil so a
			// consumer ranging over it sees `[]`, not `null`.
			return writeEnvVerifyJSON(envVerifyReport{
				Env:    envName,
				Bound:  false,
				Images: []imageVerification{},
				OK:     true,
				Detail: unboundDetail,
			})
		}
		fmt.Printf("Environment %s has no release binding — nothing is declared, so there is nothing to verify.\n", envName)
		fmt.Printf("  Bind one with: forge env promote <version> --to %s\n", envName)
		return nil
	}
	if len(binding.Resolved) == 0 {
		// A binding with no resolved digests is a defective binding: it
		// names a release but froze no artifacts, so it cannot be checked
		// against anything. Reporting "0 drifted" would read as success.
		return fmt.Errorf("environment %s is bound to release %s but the binding resolved NO image digests — "+
			"there is nothing to verify against.\n"+
			"  Re-promote it with: forge env promote %s --to %s",
			envName, binding.Release, binding.Release, envName)
	}

	if !opts.JSON {
		fmt.Printf("Verifying environment %s against release %s (%d image(s))\n", envName, binding.Release, len(binding.Resolved))
		if !binding.PromotedAt.IsZero() {
			// Printed with the caveat attached. The timestamp is the single
			// most misread field in the ledger — it looks like a deploy time
			// and is not — so the report states what it actually means rather
			// than leaving the reader to assume.
			fmt.Printf("  promoted %s (promote time, NOT deploy time — that gap is what this command checks)\n", formatLedgerTime(binding.PromotedAt))
		}
	}

	target := opts.Resolver.Resolve(ctx, projectDir, envName)
	kubeContext, namespace := target.KubeContext, target.Namespace

	var results []imageVerification
	switch {
	case kubeContext == "" || namespace == "":
		// Cannot even address the cluster. This is UNREACHABLE, not drift:
		// nothing has been learned about what the env is running.
		//
		// The two reasons a target does not resolve need DIFFERENT
		// messages, and conflating them sends the reader to the wrong
		// place. An env bound in the ledger but absent from this checkout
		// (a release cut on a branch that has the env, verified from one
		// that does not) is not a KCL problem at all — telling that reader
		// to add a field to deploy/kcl/<env>/main.k names a file they will
		// not find, which is how a diagnostic costs more time than it saves.
		mainK := filepath.Join(projectDir, "deploy", "kcl", envName, "main.k")
		if _, statErr := os.Stat(mainK); statErr != nil {
			results = unreachableVerifications(binding.Resolved, fmt.Errorf(
				"environment %s is bound in the ledger but not declared in this checkout (%s does not exist) — "+
					"verify from a checkout that declares it", envName, mainK))
			break
		}
		missing := "forge.K8sCluster.cluster"
		if namespace == "" && kubeContext != "" {
			missing = "forge.K8sCluster.namespace"
		} else if namespace == "" {
			missing = "forge.K8sCluster.cluster and .namespace"
		}
		results = unreachableVerifications(binding.Resolved, fmt.Errorf(
			"could not determine where %s runs — no %s declared in %s (a host-only or compose env has no cluster to verify)",
			envName, missing, mainK))
	default:
		if !opts.JSON {
			fmt.Printf("  cluster  %s (namespace %s)\n", kubeContext, namespace)
		}
		readCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		running, lerr := opts.Lister.ListWorkloadImages(readCtx, kubeContext, namespace)
		if lerr != nil {
			results = unreachableVerifications(binding.Resolved, lerr)
		} else {
			results = verifyEnvImages(running, binding.Resolved)
		}
	}
	tally := tallyEnvVerifications(results)

	// The verdict is decided ONCE, here, before either renderer runs. Both
	// modes then report this same decision, which is what makes "--json
	// exits identically to text mode" a structural property rather than
	// two switches somebody has to keep in agreement.
	var failure error
	switch {
	case tally.Drift > 0 || tally.Missing > 0:
		failure = exitCodeError{code: 1, msg: fmt.Sprintf(
			"environment %s does not match its binding: %d image(s) drifted, %d missing (declared release %s)",
			envName, tally.Drift, tally.Missing, binding.Release)}
	case tally.Unreachable > 0:
		failure = exitCodeError{code: 2, msg: fmt.Sprintf(
			"environment %s could not be checked: %d image(s) unreachable (cluster, context or credentials), %d matched, 0 drifted",
			envName, tally.Unreachable, tally.Match)}
	}

	if opts.JSON {
		report := envVerifyReport{
			Env:         envName,
			Bound:       true,
			Release:     binding.Release,
			PromotedAt:  formatLedgerTime(binding.PromotedAt),
			KubeContext: kubeContext,
			Namespace:   namespace,
			Images:      results,
			Tally:       tally,
			OK:          failure == nil,
		}
		if failure != nil {
			report.Detail = failure.Error()
		}
		if report.Images == nil {
			report.Images = []imageVerification{}
		}
		if err := writeEnvVerifyJSON(report); err != nil {
			return err
		}
		// The report is already on stdout; returning the same sentinel
		// gives cobra the exit code text mode would have produced.
		return failure
	}

	fmt.Println()

	printEnvVerifyImages(results)

	fmt.Printf("\n%d match, %d drifted, %d missing, %d untagged, %d unreachable\n",
		tally.Match, tally.Drift, tally.Missing, tally.Untagged, tally.Unreachable)

	if failure != nil {
		return failure
	}

	if tally.Untagged > 0 {
		fmt.Printf("\nNote: %d image(s) run by mutable tag and could not be digest-checked. Deploy without --no-digest to pin them.\n", tally.Untagged)
	}
	return nil
}

// printEnvVerifyImages renders the per-image block of the text report — the
// human-readable twin of the Images array --json emits. It is the whole of
// what text mode says about individual images, so it carries no verdict and
// returns nothing: runEnvVerify decides pass/fail once, before either renderer
// runs, and this only describes what was found.
func printEnvVerifyImages(results []imageVerification) {
	for _, r := range results {
		fmt.Printf("  %-12s %s\n", r.State, r.Image)
		// Drift prints both digests on their own labelled lines. This is
		// the case the command exists for, and the reader needs to copy
		// both values out; burying them in a prose sentence makes that
		// harder for no gain.
		if r.State == imageDrift {
			fmt.Printf("      declared  %s\n", r.Declared)
			fmt.Printf("      running   %s\n", r.Running)
		} else if r.Running != "" && r.State != imageMatch {
			fmt.Printf("      running   %s\n", r.Running)
		}
		if len(r.Workloads) > 0 && r.State != imageMatch {
			fmt.Printf("      workloads %s\n", joinWorkloads(r.Workloads))
		}
		if r.Detail != "" {
			fmt.Printf("      %s\n", r.Detail)
		}
	}
}

// writeEnvVerifyJSON emits the report to stdout, indented, per the house
// convention (flag `--json`, indented json.Encoder to stdout).
func writeEnvVerifyJSON(report envVerifyReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// joinWorkloads renders the workload list, capping it so an env with fifty
// replicas of a shared image does not bury the digests the reader came for.
func joinWorkloads(names []string) string {
	const max = 4
	if len(names) <= max {
		return joinComma(names)
	}
	return fmt.Sprintf("%s (+%d more)", joinComma(names[:max]), len(names)-max)
}

func joinComma(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
