// Package cli — the shared reconcile spine under `forge up` and
// `forge deploy`.
//
// Both commands bring an environment toward its declared end-state; they
// differ on exactly two axes, made explicit here so the up-vs-deploy
// relationship is legible instead of buried in a blank-options call seam:
//
//	scope     — WHICH entity kinds a run acts on:
//	              * cluster      — kubectl-apply the in-cluster workloads
//	                               (Deployments / Jobs / operators), the
//	                               compose deploy targets, and (for deploy)
//	                               the External dispatch.
//	              * composeInfra — pre-warm the docker-compose infra
//	                               (postgres / nats / temporal) concurrent
//	                               with the build phase. up-only; deploy's
//	                               cluster apply already drives compose
//	                               targets through the same provider.
//	              * host         — start every `deploy: "host"` service as a
//	                               host process (go-run / air / binary /
//	                               delve). up-only.
//	              * frontend     — start every declared frontend's dev
//	                               server (`npm run dev`). up-only. (deploy
//	                               instead publishes Firebase frontends — a
//	                               fixed, structural step of the deploy
//	                               pipeline, not a scope field.)
//
//	lifecycle — what a run does once everything is started:
//	              * once      — reconcile and RETURN. `forge deploy` is
//	                            always `once`; so is the non-TTY `forge up`
//	                            (start host/frontend processes, persist
//	                            their PIDs, print the summary, return —
//	                            stop later via `forge up stop`).
//	              * supervise — start the long-lived host/frontend
//	                            processes, HOLD on a signal channel, and
//	                            cascade-teardown on Ctrl-C. The interactive
//	                            `forge up` lifecycle.
//	              * auto      — defer the once-vs-supervise choice to a
//	                            TTY check at runtime (resolveUpLifecycle).
//	                            `forge up`'s default when neither --watch
//	                            nor --background is given.
//
// In this vocabulary:
//
//	forge deploy <env>  = scope{cluster} (+ structural Firebase publish),
//	                      lifecycle=once, opts=<from flags>
//	forge up --env=<e>  = scope=all,                     lifecycle=auto,
//	                      opts={}
//
// The surgical knobs (tag / rollback / prune / dry-run / context override /
// targets / skip-frontend) live on deployOptions — the cluster reconcile's
// option surface, shared by both commands. `up`'s cluster step passes a
// scope-derived (today: zero-value) deployOptions through the SAME named
// entry point deploy uses (reconcileCluster), so there is no longer a
// blank-`deployOptions{}` literal standing in for "deploy with no options."
package cli

// reconcileScope names which entity kinds a reconcile run acts on. A run
// touches only the kinds whose field is true. `forge up` derives its scope
// from the --cluster-only / --host-only flags via upScope (fullUpScope
// masked to one side of the split); `forge deploy`'s scope (cluster apply +
// Firebase frontend publish) is fixed and expressed structurally by the
// deploy pipeline rather than threaded as a value.
type reconcileScope struct {
	// cluster applies the in-cluster workloads + External/Compose deploy
	// targets (the runDeploy pipeline). Both commands set this.
	cluster bool
	// composeInfra pre-warms the docker-compose infra concurrent with the
	// build phase. up-only — deploy reaches compose targets through the
	// cluster apply's provider dispatch instead.
	composeInfra bool
	// host starts `deploy: "host"` services as host processes. up-only.
	host bool
	// frontend starts each declared frontend's dev server. up-only.
	frontend bool
}

// fullUpScope is `forge up`'s whole-dev-loop scope: build+deploy the cluster
// (with the concurrent compose pre-warm) and run host services + frontend
// dev servers. frontendShip is off: `up` runs dev servers, it does not
// publish to Firebase. upScope masks this down per --cluster-only/--host-only.
func fullUpScope() reconcileScope {
	return reconcileScope{cluster: true, composeInfra: true, host: true, frontend: true}
}

// upScope derives a `forge up` run's scope from its --cluster-only /
// --host-only flags — the single source of truth for which phases runUp
// executes, replacing the scattered hostOnly/clusterOnly conditionals.
// Starts from fullUpScope and masks one side of the cluster/host split:
//
//   - --cluster-only → drop the host + frontend dev phases (CI lanes that
//     only want the apply).
//   - --host-only    → drop the cluster build/deploy + compose pre-warm
//     (iterate host services against an already-deployed cluster).
//
// The two flags are mutually exclusive (rejected at flag-parse time), so at
// most one mask applies; neither set yields the full scope. Pure so the
// derivation is unit-tested, mirroring resolveUpLifecycle.
func upScope(clusterOnly, hostOnly bool) reconcileScope {
	s := fullUpScope()
	if hostOnly {
		s.cluster = false
		s.composeInfra = false
	}
	if clusterOnly {
		s.host = false
		s.frontend = false
	}
	return s
}

// reconcileLifecycle is the post-start behaviour: return immediately (once)
// or hold + teardown on signal (supervise). `forge up`'s "auto" default —
// supervise in a TTY, once otherwise — is not a third enum value; it is
// resolved to one of these two by resolveUpLifecycle at the call site before
// the host phase, so no un-resolved lifecycle ever flows downstream.
type reconcileLifecycle int

const (
	// lifecycleOnce reconciles and returns. `forge deploy` always; the
	// non-TTY `forge up` after persisting PIDs + printing the summary.
	lifecycleOnce reconcileLifecycle = iota
	// lifecycleSupervise holds the foreground on a signal channel and
	// cascade-tears-down the host/frontend processes on Ctrl-C. The
	// interactive `forge up`.
	lifecycleSupervise
)

// resolveUpLifecycle is the pure TTY-aware lifecycle decision for
// `forge up`. It is the LLM-first fix: an agent / CI invocation of
// `forge up --env=<env>` must NOT hang on the interactive Ctrl-C hold, so
// the DEFAULT (neither --watch nor --background) supervises only when a TTY
// is present and otherwise returns after detaching — the same end-state as
// --background.
//
// Precedence (explicit flags beat the TTY default):
//
//   - --background wins outright: the user explicitly asked to detach and
//     return, so lifecycle=once regardless of --watch or the TTY. (When
//     both --watch and --background are set, --background is the documented
//     winner — "detach and return" is the more conservative, non-blocking
//     outcome, and matches the flag's long-standing "return immediately"
//     contract.)
//   - --watch forces supervise (hold + Ctrl-C teardown) even without a
//     TTY, for a human who pipes `forge up` output through a tool.
//   - Otherwise the TTY decides: a terminal → supervise (today's
//     interactive behaviour); no terminal → once (return after start).
//
// Split out from runUp so the decision is unit-tested by injecting isTTY
// and the two flags.
func resolveUpLifecycle(isTTY, watch, background bool) reconcileLifecycle {
	switch {
	case background:
		return lifecycleOnce
	case watch:
		return lifecycleSupervise
	case isTTY:
		return lifecycleSupervise
	default:
		return lifecycleOnce
	}
}
