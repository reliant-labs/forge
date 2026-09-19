package deploytarget

import "context"

// The providers that decline to observe, and the two different reasons.
//
// Both gaps here are STRUCTURAL and permanent. No future version of
// forge can observe either one, and the reason is the same in both
// cases: forge holds no identity of its own on the other side.
//
//   - External runs an opaque `sh -c`. forge does not know whether that
//     command talked to Fly.io, systemd or a wet string, so there is
//     genuinely nothing to read back.
//   - Firebase owns its own release history and forge keeps no artifact
//     of its own there, which is the same reason its Rollback returns
//     ErrProviderNotImplemented rather than guessing.
//
// There is deliberately no third category. Compose and HostInfra were
// once listed here as NOT YET BUILT, and both are now implemented
// (observe_compose.go, observe_hostinfra.go) — which is what that label
// was for. A gap mislabeled as temporary is the thing to avoid; a gap
// that stays temporary forever is the same defect wearing a nicer word.
//
// Declining explicitly, rather than leaving these out of the Provider
// interface, is the whole point of the rule. A provider that cannot
// observe must REPORT UNKNOWN, never green, and never silently vanish
// from a report: a reconciler that quietly skipped the tiers forge had
// not got to yet would show a clean board for an environment it had
// measured half of. TestEveryRegisteredProviderObservesOrDeclares
// enforces exactly that, for every provider in the registry.

// Observe is not supported for External targets, and never will be.
//
// The deploy contract for this provider is a user-supplied shell command.
// forge substitutes tokens into it and checks the process exit code; it
// does not know what the command did, what system it talked to, or how
// to ask that system anything. A `health_cmd` is the user's own hook for
// this, but it is a boolean the user wrote — not an observation forge can
// interpret into a digest, a replica count or a verdict it is entitled to
// report as its own.
func (p ExternalProvider) Observe(_ context.Context, group ServiceGroup) (Observed, error) {
	return unsupported(p.Name(),
		"external targets run an opaque `sh -c` deploy command, so forge has nothing to read back; "+
			"what is running is known only to the system that command talked to",
		observableNames(group))
}

// Observe is not supported for Firebase Hosting targets.
//
// Structural, and the same gap that makes Rollback return
// ErrProviderNotImplemented here: forge ships the assembled tree to
// Firebase and Firebase owns the release history afterwards. forge keeps
// no artifact of its own (contrast StaticSite, whose per-digest release
// prefix is exactly what makes both rollback and observation real), so
// there is no forge-side identity to compare a live site against.
func (p FirebaseProvider) Observe(_ context.Context, group ServiceGroup) (Observed, error) {
	return unsupported(p.Name(),
		"Firebase owns the release history and forge keeps no artifact of its own for this target, "+
			"so there is no forge-side identity to compare the live site against",
		observableNames(group))
}
