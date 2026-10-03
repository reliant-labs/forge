package hostedimage

// What a DENIED push to the platform registry means, said in terms of the
// declaration rather than the protocol.
//
// WHY THIS EXISTS. The org is declared in KCL and ENFORCED by the registry's
// realm, which mints a token scoped to the org on the authenticated row and
// refuses any repository outside that subtree. That design is what makes a
// declared org safe — forge cannot talk itself into the wrong one — but it
// puts the two halves of the diagnosis in different places: the author wrote
// an org in a file, and a server that will not name the org it expected
// returned `401 Unauthorized`.
//
// Left alone, that surfaces as `denied: requested access to the resource is
// denied`, which reads as a credential problem. An author then re-runs
// `forge registry login`, succeeds, pushes again, and is denied again — with
// nothing anywhere connecting the refusal to the one line they can edit. So
// forge, which is the only party that knows what was DECLARED, says it.
//
// IT IS A HINT, AND IT IS HONEST ABOUT THAT. forge does not know the token's
// org — deliberately, since knowing it would mean trusting a value the client
// could get wrong, which is the thing the realm exists to prevent. So the
// wording names the declared org and the two possibilities, and does not
// assert which one holds. A message that guessed would be wrong exactly when
// the credential really had expired.

import (
	"errors"
	"fmt"
	"strings"

	"oras.land/oras-go/v2/registry/remote/errcode"
)

// IsDenied reports whether err is a registry refusal to let this credential
// write here — as opposed to a network fault, a missing credential store, or
// a bad reference.
//
// Both transports forge pushes over are covered, because both reach the same
// registry and the author cannot be expected to care which one carried their
// image:
//
//   - oras (bundles, static-site releases) returns a typed
//     *errcode.ErrorResponse, so the status is read rather than matched.
//   - `docker push` is a subprocess whose exit status is just "failed", so
//     the only signal is the daemon's own message on the pipe. Matched on the
//     OCI distribution error CODE (`denied`, `UNAUTHORIZED`), which is part
//     of the spec rather than of docker's phrasing.
func IsDenied(err error, output string) bool {
	if err == nil {
		return false
	}
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == 401 || resp.StatusCode == 403
	}
	lower := strings.ToLower(output + " " + err.Error())
	return strings.Contains(lower, "denied") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "authentication required")
}

// DeniedHint is the sentence to append to a refused push: what was declared,
// and the two things that can make the registry refuse it.
//
// organization "" means the env declared none, which is a different problem
// with a different fix, so it gets a different sentence rather than a hint
// about a mismatch that cannot be the cause.
func DeniedHint(reference, organization string) string {
	if strings.TrimSpace(organization) == "" {
		return fmt.Sprintf("The registry refused the push to %s, and this env declares no organization — "+
			"so forge composed no push base and the reference above is not under any org's subtree.\n"+
			"  fix: set `organization = \"<your org id>\"` on control_plane in the env's main.k", reference)
	}
	return fmt.Sprintf("The registry refused the push to %s.\n"+
		"  This env declares organization = %q, and the registry admits a credential only within its OWN "+
		"organization's subtree — so either that is not the organization your credential belongs to, or the "+
		"credential has expired.\n"+
		"  fix: check `organization` in the env's control_plane declaration against the organization you are "+
		"logged in as, then re-run `forge registry login <env>`", reference, organization)
}
