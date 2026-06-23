// Package operatorkit owns the controller-manager runtime behind the
// generated App.RunOperators method in forge projects.
//
// # Pattern
//
// The generated pkg/app/bootstrap.go used to open-code the
// controller-runtime manager setup (kubeconfig resolution, leader
// election, scheme registration, controller setup, manager start).
// Following the "generated files are tables, not programs" rule, the
// generated RunOperators is now a single delegation to [Run] with one
// dumb [Controller] row per operator:
//
//	func (a *App) RunOperators(ctx context.Context, logger *slog.Logger, healthProbeAddr string) error {
//	    return operatorkit.Run(ctx, logger, operatorkit.Options{
//	        LeaderElectionID:       "example.com/myproj-leader",
//	        HealthProbeBindAddress: healthProbeAddr,
//	    }, []operatorkit.Controller{
//	        {Name: "scaler", AddToScheme: scaler.AddToScheme,
//	            SetupWithManager: a.Operators.Scaler.SetupWithManager},
//	    })
//	}
//
// operatorkit lives in its own package (rather than appkit proper) so
// projects without operators never compile controller-runtime and its
// Kubernetes dependency tree — the generated import is conditional on
// the project having operators.
//
// # Behavioural fingerprint
//
// All observable strings from the pre-table generated RunOperators are
// preserved verbatim:
//
//   - warn "operators disabled: no Kubernetes cluster reachable" when
//     kubeconfig resolution fails (vanilla docker-compose dev, fresh
//     laptop, CI without a kind/k3d cluster) — the binary continues
//     without operators rather than crashing, matching how NATS
//     degrades.
//   - "creating controller manager: <wrapped error>".
//   - "adding <name> scheme: <wrapped error>".
//   - "setting up controller %q: <wrapped error>".
//   - info "registered operator controller" / "starting controller
//     manager".
package operatorkit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// Leader-election timing + client-rate defaults.
//
// controller-runtime's stock leader-election timings (LeaseDuration 15s,
// RenewDeadline 10s, RetryPeriod 2s) are tuned for a fast, dedicated API
// server and an HA controller where a quick failover is worth a hair-trigger
// self-termination: when the acting leader can't renew its lease within
// RenewDeadline, the manager invokes its FailureProcess and the PROCESS EXITS
// ("leader election lost" → "component failed — terminating process"). On a
// contended/single-node API server a brief latency spike (slow networkpolicy
// PUT, an HTTP/2 connection drop, a TLS-handshake timeout) blows past a 10s
// RenewDeadline and kills an otherwise-healthy controller mid-reconcile,
// stalling every CR it owns until the relist completes.
//
// For the common forge shape — a SINGLE-replica controller — fast failover
// buys nothing (there is no standby to fail over to) while the tight deadline
// costs a spurious crash. So we triple the timings to a tolerance band that
// rides out transient API slowness, and keep them env-overridable for ops
// tuning. These are the conventional hardened values for single-replica
// controllers on shared/edge clusters.
const (
	defaultLeaseDuration = 45 * time.Second
	defaultRenewDeadline = 30 * time.Second
	defaultRetryPeriod   = 5 * time.Second

	// Client-go's stock rest.Config limits (QPS 5 / Burst 10 raw; 20 / 30
	// once controller-runtime applies its own defaults) throttle the
	// controller's OWN requests client-side under reconcile fan-out — the
	// "client-side throttling, request waited …" / priority-and-fairness
	// stalls that compound a slow API server into renew-deadline misses.
	// Raise the ceiling so a burst of reconciles isn't self-queued; the
	// server's APF still protects the API server from genuine overload.
	defaultClientQPS   float32 = 50
	defaultClientBurst int     = 100
)

// Controller is one generated operator row: the CRD scheme installer
// and the controller's manager hookup, both referenced straight off the
// generated operator package / constructed instance.
type Controller struct {
	// Name is the operator's forge.yaml name — used in error messages
	// and registration logs.
	Name string
	// AddToScheme registers the operator's CRD types on the manager's
	// scheme. Optional (nil is skipped) for controllers that only watch
	// built-in types.
	AddToScheme func(s *runtime.Scheme) error
	// SetupWithManager registers the controller with the manager.
	SetupWithManager func(mgr ctrl.Manager) error
}

// Options carries the per-project manager configuration the generated
// row table supplies.
type Options struct {
	// LeaderElectionID is the lease name used for leader election —
	// the generated table passes "<module>-leader". The LEADER_ELECTION_ID
	// env var, when set, overrides this so distinct processes can take
	// distinct leases (env > this default).
	LeaderElectionID string

	// HealthProbeBindAddress, when non-empty, binds a /healthz +
	// /readyz listener on that address for the controller-runtime
	// manager. The generated RunOperators forwards it from
	// serverkit.Config.OperatorHealthProbeAddr. Empty leaves the
	// manager without a probe listener (the default — vanilla forge
	// projects don't bind one).
	HealthProbeBindAddress string
}

// Run creates a controller manager, registers every controller's
// scheme and setup, and starts the manager. It blocks until ctx is
// cancelled or an error occurs; the caller runs it in a goroutine.
//
// When no Kubernetes cluster is reachable, kubeconfig resolution fails
// and Run logs a warning and returns nil — the process continues
// without operators instead of crashing.
func Run(ctx context.Context, logger *slog.Logger, opts Options, controllers []Controller) error {
	if logger == nil {
		logger = slog.Default()
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		logger.Warn("operators disabled: no Kubernetes cluster reachable", "error", err)
		return nil
	}

	// Out-of-cluster leader election needs an explicit lease namespace —
	// controller-runtime infers it from the ServiceAccount mount in-cluster
	// and hard-errors otherwise ("unable to find leader election
	// namespace"). A host-mode process with a working kubeconfig (the
	// dev-loop shape: admin binary on the laptop, operators deployed
	// in-cluster) would otherwise get PAST the no-cluster degrade above
	// and then die in NewManager. Treat "reachable cluster, but not
	// in-cluster and no LEADER_ELECTION_NAMESPACE" the same way as
	// no-cluster: warn and continue without operators. Setting
	// LEADER_ELECTION_NAMESPACE opts a host process back in (e.g. running
	// an operator from source against a dev cluster).
	leaderNS := os.Getenv("LEADER_ELECTION_NAMESPACE")
	if leaderNS == "" && !runningInCluster() {
		logger.Warn("operators disabled: not running in-cluster and LEADER_ELECTION_NAMESPACE is unset; set it to run operators from a host process")
		return nil
	}

	// Probe-address precedence: explicit Options value (forwarded from
	// serverkit.Config.OperatorHealthProbeAddr) > HEALTH_PROBE_BIND_ADDRESS
	// env var (the conventional controller-runtime deploy knob — k8s
	// manifests set it next to METRICS_BIND_ADDRESS) > none. No hard
	// default: vanilla forge projects shouldn't surprise-bind a port.
	probeAddr := opts.HealthProbeBindAddress
	if probeAddr == "" {
		probeAddr = os.Getenv("HEALTH_PROBE_BIND_ADDRESS")
	}

	leaderID := resolveLeaderElectionID(opts.LeaderElectionID)

	// Lift the client's own request ceiling before NewManager derives its
	// clients from cfg. Only set when unconfigured so an explicit kubeconfig /
	// caller value still wins. See defaultClientQPS for rationale.
	if cfg.QPS == 0 {
		cfg.QPS = envFloat32("OPERATOR_CLIENT_QPS", defaultClientQPS)
	}
	if cfg.Burst == 0 {
		cfg.Burst = envInt("OPERATOR_CLIENT_BURST", defaultClientBurst)
	}

	leaseDuration := envDuration("OPERATOR_LEASE_DURATION", defaultLeaseDuration)
	renewDeadline := envDuration("OPERATOR_RENEW_DEADLINE", defaultRenewDeadline)
	retryPeriod := envDuration("OPERATOR_RETRY_PERIOD", defaultRetryPeriod)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		LeaderElection:   true,
		LeaderElectionID: leaderID,
		// Empty in-cluster — controller-runtime infers the namespace from
		// the ServiceAccount mount. Non-empty only via the env opt-in above.
		LeaderElectionNamespace: leaderNS,
		// Hardened leader-election timings so a transient API-server latency
		// spike doesn't trip RenewDeadline and self-terminate a healthy
		// single-replica controller. See defaultLeaseDuration for rationale.
		LeaseDuration:          &leaseDuration,
		RenewDeadline:          &renewDeadline,
		RetryPeriod:            &retryPeriod,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		return fmt.Errorf("creating controller manager: %w", err)
	}

	// Wire the default /healthz + /readyz checks so the listener
	// configured above actually answers 200. Without these, the manager
	// binds the port but every probe gets 404, defeating the listener's
	// purpose. The "ping" check is the conventional always-ok signal —
	// the manager keeps its own internal state.
	if probeAddr != "" {
		if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
			return fmt.Errorf("adding healthz check: %w", err)
		}
		if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
			return fmt.Errorf("adding readyz check: %w", err)
		}
	}

	// Register CRD schemes first, then controllers — a controller's
	// SetupWithManager may depend on a sibling operator's types.
	for _, c := range controllers {
		if c.AddToScheme == nil {
			continue
		}
		if err := c.AddToScheme(mgr.GetScheme()); err != nil {
			return fmt.Errorf("adding %s scheme: %w", c.Name, err)
		}
	}

	for _, c := range controllers {
		if err := c.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setting up controller %q: %w", c.Name, err)
		}
		logger.Info("registered operator controller", "controller", c.Name)
	}

	logger.Info("starting controller manager")
	return mgr.Start(ctx)
}

// resolveLeaderElectionID applies the lease-name precedence:
// LEADER_ELECTION_ID env var > the generated Options.LeaderElectionID
// default. Without the env override the lease name is hardcoded per
// project, so two processes that both run the manager (e.g. a catch-all
// API server and the dedicated operator) contend for the SAME lease even
// when the deployment sets a distinct LEADER_ELECTION_ID. Honouring the
// env lets distinct processes take distinct leases. Empty/unset keeps the
// generated default unchanged.
func resolveLeaderElectionID(optsID string) string {
	if envID := os.Getenv("LEADER_ELECTION_ID"); envID != "" {
		return envID
	}
	return optsID
}

// envDuration returns the duration parsed from the named env var, or def when
// the var is unset or unparseable. Used for the ops-tunable leader-election
// timing overrides — a typo'd value falls back to the hardened default rather
// than silently zeroing a timing (which controller-runtime would then replace
// with its own short stock default).
func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// envFloat32 returns the float32 parsed from the named env var, or def when the
// var is unset or unparseable.
func envFloat32(name string, def float32) float32 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil && f > 0 {
			return float32(f)
		}
	}
	return def
}

// envInt returns the int parsed from the named env var, or def when the var is
// unset or unparseable.
func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// runningInCluster reports whether the process is running inside a
// Kubernetes pod, using the same signal controller-runtime's leader
// election uses to infer the lease namespace: the ServiceAccount
// namespace mount. Checking the mount (rather than KUBERNETES_SERVICE_HOST)
// matches what NewManager will actually succeed or fail on.
func runningInCluster() bool {
	_, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	return err == nil
}
