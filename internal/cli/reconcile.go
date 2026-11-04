// Package cli — the shared reconcile spine under `forge env up` and
// `forge env deploy`.
//
// Both commands bring an environment toward its declared end-state. WHICH
// entity kinds each acts on is structural, not a value threaded through the
// call:
//
//	forge env deploy — the cluster apply (in-cluster workloads, compose
//	                   deploy targets, the External dispatch) plus the
//	                   Firebase frontend publish.
//	forge env up     — that same cluster apply, plus the concurrent infra
//	                   pre-warm, plus the host-process and frontend
//	                   dev-server phases. It always runs the whole loop.
//
// `up` used to carry a `scope` value masking phases off per --cluster-only /
// --host-only. Those flags are gone: `--target <name>` scopes a run by naming
// the entities it acts on, which narrows the work inside each phase without
// forge deciding that a cluster-declared service is "really" a host process.
// Placement stays where it is declared — a service that should run on the
// host during dev says so with a `host` block in its KCL.
//
// What remains genuinely variable is:
//
//	lifecycle — what a run does once everything is started:
//	              * once      — reconcile and RETURN. `forge env deploy` is
//	                            always `once`; so is `forge env up --background`
//	                            (start host/frontend processes, persist
//	                            their PIDs, print the summary, return —
//	                            stop later via `forge env down`).
//	              * supervise — start the long-lived host/frontend
//	                            processes, HOLD on a signal channel, and
//	                            cascade-teardown on SIGINT / SIGTERM / SIGHUP.
//	                            `forge env up`'s default, TTY or not
//	                            (resolveUpLifecycle).
//
// In this vocabulary:
//
//	forge env deploy <env> = lifecycle=once, opts=<from flags>
//	forge env up <env>     = lifecycle=supervise (once with --background), opts={skipFrontend: true}
//
// The surgical knobs (tag / prune / dry-run / context override /
// targets / skip-frontend) live on deployOptions — the cluster reconcile's
// option surface, shared by both commands. `up`'s cluster step passes its
// deployOptions through the SAME named entry point deploy uses
// (reconcileCluster), so there is no blank-`deployOptions{}` literal standing
// in for "deploy with no options."
package cli

// reconcileLifecycle is the post-start behaviour: return immediately (once)
// or hold + teardown on signal (supervise). `forge env up` resolves its flags
// to one of these two (resolveUpLifecycle) at the call site before the host
// phase, so no un-resolved lifecycle ever flows downstream.
type reconcileLifecycle int

const (
	// lifecycleOnce reconciles and returns. `forge env deploy` always;
	// `forge env up --background` after persisting PIDs + printing the summary.
	lifecycleOnce reconcileLifecycle = iota
	// lifecycleSupervise holds the foreground on a signal channel and
	// cascade-tears-down the host/frontend processes on SIGINT, SIGTERM or
	// SIGHUP. `forge env up` without --background, TTY or not.
	lifecycleSupervise
)

// resolveUpLifecycle is the lifecycle decision for `forge env up`: it
// supervises unless --background asked it to detach.
//
// It used to supervise only with a TTY and detach otherwise, so that an agent
// or CI step running `forge env up <env>` would not hang on the hold. That
// traded one agent problem for a worse one: the stack outlived the command
// that started it, in process groups nothing tracked, so the harness running
// the agent could neither see nor stop it — the shape a harness's "never
// detach long work" rule exists to forbid. A harness runs a long-lived
// command in the background and stops it with a signal; supervising does
// exactly what that needs (SIGTERM, SIGINT or SIGHUP tears the stack down).
// Wanting the stack to outlive the command is now said out loud, with
// --background.
//
// --watch forced supervise without a TTY. That is now the default, so it no
// longer changes anything; the flag stays so scripts that pass it keep working.
func resolveUpLifecycle(background bool) reconcileLifecycle {
	if background {
		return lifecycleOnce
	}
	return lifecycleSupervise
}
