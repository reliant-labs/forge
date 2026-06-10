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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
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
	// the generated table passes "<module>-leader".
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

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		LeaderElection:         true,
		LeaderElectionID:       opts.LeaderElectionID,
		HealthProbeBindAddress: opts.HealthProbeBindAddress,
	})
	if err != nil {
		return fmt.Errorf("creating controller manager: %w", err)
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
