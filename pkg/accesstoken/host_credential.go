package accesstoken

import (
	"fmt"
	"strings"
	"time"
)

// ── The end-tenant host credential ───────────────────────────────────
//
// A HOST is an organisation that runs a product for end tenants of its own:
// it creates them, connects them to outside services and, rarely, deletes
// them. The credential it does that with is its own automation, not a
// person's: a token carrying endtenants:manage, endtenants:delete and
// connections:connect (HostCredentialScopes), and nothing else.
//
// Like a registry-only credential (registryOnlyScopes) it is a promise about
// the WHOLE token, so Validate refuses every shape that would break it:
//
//   - only host scopes: mixing one with any other scope would make the token
//     admissible by every RPC of that other scope;
//   - no acting user: it is a host's automation, and authority attributed to
//     a person would be clipped (and revoked) with that person;
//   - no binding, client id, audience or app grant, and not ephemeral: it
//     is confined by its scopes and its expiry, nothing else;
//   - an expiry, at most MaxHostCredentialTTL from the moment it is minted.
//
// The scopes are their own class (ClassHost): in no bundle, so no bundle's
// expansion (which happens at check time) can widen an existing token; not
// federatable to a CI identity; never on a daemon and never elevated to; and
// no wildcard (Grant.Admissible) resolves to them.
//
// ── EXPIRY ────────────────────────────────────────────────────────────
//
// A Grant carries no issue time, so the cap is measured from the `now`
// Validate is called with, which at a mint is the issue time. The MINTER owns
// the rest: it passes the real clock, checks MaxHostCredentialTTL again where
// it stores the expiry, and rotates by minting the successor BEFORE the
// predecessor lapses. Nothing here limits how many host tokens an org holds
// live at once, so a rotation can overlap.

// MaxHostCredentialTTL is the longest a host credential may live from the
// moment it is minted.
const MaxHostCredentialTTL = 365 * 24 * time.Hour

// HostCredentialScopes are the scopes of the end-tenant host credential, in
// AllScopes order. A credential carries a non-empty subset of them and no
// other scope.
var HostCredentialScopes = []Scope{ScopeEndTenantsManage, ScopeEndTenantsDelete, ScopeConnectionsConnect}

// IsHostCredentialScope reports whether scope is one of HostCredentialScopes.
func IsHostCredentialScope(scope Scope) bool {
	for _, s := range HostCredentialScopes {
		if s == scope {
			return true
		}
	}
	return false
}

// HostCredential reports whether the set carries a host-credential scope,
// which (Grant.Validate) makes the whole credential a host credential.
func (s Set) HostCredential() bool {
	for scope := range s {
		if IsHostCredentialScope(scope) {
			return true
		}
	}
	return false
}

// validateHostCredential holds a host credential to its shape (see the block
// above). now is the time of the mint.
func (g Grant) validateHostCredential(now time.Time) error {
	if !g.Scopes.HostCredential() {
		return nil
	}
	for _, scope := range AllScopes {
		if g.Scopes.Has(scope) && !IsHostCredentialScope(scope) {
			return fmt.Errorf("%w: %s is a host credential and cannot also carry %s; mint a separate token",
				ErrInvalidGrant, JoinScopes(hostScopesIn(g.Scopes)), scope)
		}
	}
	held := JoinScopes(hostScopesIn(g.Scopes))
	switch {
	case strings.TrimSpace(g.ActingUserID) != "":
		return fmt.Errorf("%w: %s is a host organisation's credential and cannot act as a user", ErrInvalidGrant, held)
	case g.Resource != nil:
		return fmt.Errorf("%w: %s cannot be bound to a resource", ErrInvalidGrant, held)
	case g.ClientID != "" || g.Audience != "":
		return fmt.Errorf("%w: %s cannot name an OAuth client or audience", ErrInvalidGrant, held)
	case g.AppGrantID != "":
		return fmt.Errorf("%w: %s cannot carry an app grant", ErrInvalidGrant, held)
	case g.Ephemeral:
		return fmt.Errorf("%w: %s cannot be ephemeral", ErrInvalidGrant, held)
	case g.ExpiresAt == nil:
		return fmt.Errorf("%w: %s must expire, within %s of its minting", ErrInvalidGrant, held, MaxHostCredentialTTL)
	case g.ExpiresAt.After(now.Add(MaxHostCredentialTTL)):
		return fmt.Errorf("%w: %s must expire within %s of its minting", ErrInvalidGrant, held, MaxHostCredentialTTL)
	}
	return nil
}

// hostScopesIn returns the host scopes of s, in AllScopes order.
func hostScopesIn(s Set) []Scope {
	var out []Scope
	for _, scope := range AllScopes {
		if s.Has(scope) && IsHostCredentialScope(scope) {
			out = append(out, scope)
		}
	}
	return out
}
