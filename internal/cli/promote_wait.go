package cli

// `promote --wait` / `--deploy`: follow a promote through to its rollout
// (control-plane docs/design/hosted-deploy-primitives.md §3.2).
//
// OWNED BY F3. F2 declares the flags and the hook so promote.go is never
// edited again; F3 replaces the two function bodies below, in this file.

import (
	"context"
	"errors"
	"time"
)

// promoteFollowOptions are the flags that make a promote do more than move
// the pointer: ship it (--deploy) and/or wait for the rollout (--wait).
type promoteFollowOptions struct {
	// Wait blocks until the promotion this promote wrote (or the existing
	// one, for a no-op) finishes rolling out, with `forge env wait`'s exit
	// codes.
	Wait bool
	// Deploy runs the client-side deploy of the promotion just written —
	// the bridge for an env that does not converge promotions server-side.
	Deploy bool
	// Timeout is the whole wait budget; zero is the verb's default.
	Timeout time.Duration
	// FailFast exits 1 on the first DEGRADED observation instead of
	// waiting out Timeout.
	FailFast bool
}

// requested reports whether any follow-through flag was given.
func (o promoteFollowOptions) requested() bool {
	return o.Wait || o.Deploy || o.Timeout != 0 || o.FailFast
}

// errPromoteFollowUnsupported is what every follow-through flag returns until
// F3 wires it.
var errPromoteFollowUnsupported = errors.New(
	"--wait, --deploy, --timeout and --fail-fast are not supported by this forge build yet " +
		"(hosted-deploy-primitives task F3); promote without them, then run `forge env deploy <env>`")

// validatePromoteFollow runs BEFORE the plan is computed or anything is
// written, so an unsupported flag refuses the whole promote rather than
// moving the pointer and then failing on the part the caller asked for.
func validatePromoteFollow(o promoteFollowOptions) error {
	if o.requested() {
		return errPromoteFollowUnsupported
	}
	return nil
}

// followPromote runs after a successful write. recorded is the promotion the
// env now resolves to. Unreachable until F3 lifts validatePromoteFollow.
func followPromote(_ context.Context, _ string, _ promotePlan, o promoteFollowOptions) error {
	if o.requested() {
		return errPromoteFollowUnsupported
	}
	return nil
}
