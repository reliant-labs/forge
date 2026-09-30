// Package cloudcred is the PUBLIC seam for writing a credential into forge's
// credentials file on forge's behalf.
//
// ── WHY THIS EXISTS: THE DEPENDENCY IS INVERTED ───────────────────────
//
// A user who is logged in to Reliant should find forge already logged in to
// Reliant cloud. The obvious way to get that — teach forge to go and find
// reliant's credential — is the one thing forge must never do: internal/cloud's
// package header forbids importing control-plane or reliant, because a user
// deploying to hosted infra without the reliant harness must never have to
// install reliant first.
//
// So the arrow points the other way. forge publishes the FORMAT and the
// LOCATION of its credential store; reliant, which already embeds forge, writes
// the user's token into it at login and removes it at logout. forge reads its
// own file exactly as it always has and never learns that reliant exists —
// `forge login` keeps working untouched for standalone users, and a credential
// deposited by a host application is indistinguishable from one forge stored
// itself.
//
// This package is deliberately tiny: Save, Delete, Path. Everything harder —
// precedence, expiry, the endpoint keying — already lives in pkg/credentials
// and internal/cloud, and duplicating any of it here would create a second
// answer to "which credential did that command use".
//
// ── THE CLIENT ID ─────────────────────────────────────────────────────
//
// Entries are keyed by (endpoint, client). A host application writes under its
// OWN client id, not forge's, so depositing a credential can never silently
// overwrite one a human created with `forge login`, and removing it on logout
// cannot delete theirs. Resolution tries forge's own entry first and falls back
// to a host's — see internal/cloud.ResolveCredential.
//
// ── THE CALLER SUPPLIES THE LOCATION ──────────────────────────────────
//
// Save and Delete take an explicit file path; nothing here reads $FORGE_HOME
// or $XDG_CONFIG_HOME. forge/pkg compiles into every generated binary, so a
// package that reaches for ambient environment makes its behaviour depend on
// the process it happens to be linked into — which is exactly the drift that
// would let a host deposit a credential where forge does not look. Resolve the
// path at your application edge with Location(...).Path() and pass it in.
package cloudcred

import (
	"fmt"
	"time"

	"github.com/reliant-labs/forge/pkg/credentials"
)

// Location names the directories the credentials file is resolved from. It is
// forge's own precedence (pkg/credentials.Dirs), re-exported so a host
// application needs one import and cannot disagree with forge about where the
// file lives.
//
// Fill it at your application edge from $FORGE_HOME, $XDG_CONFIG_HOME and the
// user's home directory, then call Path.
type Location = credentials.Dirs

// HostClientID is the client id a HOST APPLICATION deposits under.
//
// One shared id rather than one per host: the entry means "the embedding
// application vouched for this user at this endpoint", and forge has no reason
// to distinguish which one. A second host on the same machine writing the same
// endpoint is the same user logging in to the same control plane, so the last
// write winning is correct rather than a collision.
const HostClientID = "host-app"

// Credential is what a host application knows about the token it is
// depositing. Only Token is required; the rest is display and diagnostics.
type Credential struct {
	// Token is the bearer credential, e.g. an `rlat_` access token.
	Token string
	// TokenPrefix is the safe-to-print display prefix, so a human can match
	// this entry against a token listed in the web UI.
	TokenPrefix string
	// Scopes as the issuer reported them.
	Scopes []string
	// ExpiresAt is the issuer-reported expiry; nil means it does not expire.
	ExpiresAt *time.Time
	// Issuer is the authorization server that minted the token, when it
	// differs from the endpoint the token is presented to.
	Issuer string
}

// Save deposits cred for endpoint under HostClientID in the credentials file
// at path, preserving every other entry — including any credential
// `forge login` stored for the same endpoint.
//
// Idempotent: calling it again for the same endpoint replaces the entry, which
// is what makes it safe to call on every login and every token refresh without
// first checking whether one is already there.
func Save(path, endpoint string, cred Credential) error {
	if cred.Token == "" {
		return fmt.Errorf("cloudcred: refusing to store an empty token for %s", endpoint)
	}
	return credentials.Store(path, endpoint, HostClientID, credentials.Credential{
		Token:       cred.Token,
		TokenPrefix: cred.TokenPrefix,
		Scopes:      cred.Scopes,
		ExpiresAt:   cred.ExpiresAt,
		CreatedAt:   time.Now().UTC(),
		Issuer:      cred.Issuer,
	})
}

// Delete removes the host-deposited credential for endpoint from the file at
// path and reports whether one existed. A credential a human stored with
// `forge login` at the same endpoint is NOT touched: they are separate entries
// under separate client ids, and logging out of the host application must not
// log the user out of forge.
func Delete(path, endpoint string) (bool, error) {
	return credentials.Remove(path, endpoint, HostClientID)
}
