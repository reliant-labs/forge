package authn

import (
	"context"
	"net/http"
	"strings"

	"github.com/reliant-labs/forge/pkg/auth"
)

// Channel names the part of the request a credential arrived in.
//
// [auth.Claims.Credential] says WHAT proved the caller — a token, an API
// key, a server-side session id. Channel says WHERE the caller put it, which
// is a different fact, and transport metadata only: any client can send any
// header, cookies included. [Method.IsBrowserSession] combines the two into
// the one check a sensitive action needs.
type Channel uint8

const (
	// ChannelUnknown: the interceptor could not tell where the credential
	// came from — a Policy.ExtractToken that read a channel inference does
	// not recognize, or a Policy.ExtractCredential that said so.
	ChannelUnknown Channel = iota
	// ChannelAuthorization: "Authorization: Bearer <credential>" — another
	// service, a CLI, a script, a test.
	ChannelAuthorization
	// ChannelAPIKeyHeader: the API-key header ("X-API-Key" by default).
	ChannelAPIKeyHeader
	// ChannelCookie: a cookie — in a scaffolded project, the HttpOnly
	// session cookie native sign-in sets. On its own it is not proof of a
	// browser: see [Method.IsBrowserSession].
	ChannelCookie
)

// String returns "unknown", "authorization", "api_key_header" or "cookie".
func (c Channel) String() string {
	switch c {
	case ChannelAuthorization:
		return "authorization"
	case ChannelAPIKeyHeader:
		return "api_key_header"
	case ChannelCookie:
		return "cookie"
	}
	return "unknown"
}

// Method is how the request's caller authenticated: the channel the
// credential arrived in and the kind of credential it was. Read it with
// [AuthMethod].
//
// The interceptor records it at the same point it stashes the claims,
// from the request itself and from the credential kind forge's
// authenticator stamped, so nothing a token carries can set it, and
// Policy.Enrich cannot change it.
type Method struct {
	// Channel is where the credential arrived.
	Channel Channel
	// Credential is the kind of credential that proved the caller: the
	// validated claims' [auth.Credential] kind, as it stood after Enrich.
	Credential auth.CredentialKind
}

// ViaCookie reports whether the credential arrived in a cookie. It is
// transport metadata, not proof of a person: a client outside a browser can
// set any Cookie header, so a program holding a user's bearer token can put
// it in a cookie. Gate a sensitive action on [Method.IsBrowserSession].
func (m Method) ViaCookie() bool { return m.Channel == ChannelCookie }

// IsBrowserSession reports whether the caller is a person's browser
// session: the credential arrived in a cookie AND was a server-side session
// id ([auth.CredentialSession]) that forge's authenticator resolved against
// the session store (forge/pkg/session). No other credential passes it — not
// a bearer token replayed in a cookie, not an API key, not an unknown kind —
// because only a session id is never issued to anything but the browser
// that signed in, so a program holding the same user's token cannot
// impersonate it. A project that does not keep server-side sessions has no
// browser session to prove, and IsBrowserSession is false for every caller:
// it fails closed.
//
// It is false for the zero Method, so a request with no recorded method —
// an allow-listed procedure, an anonymous caller, a passthrough interceptor
// — never passes it.
//
// It is not a CSRF defence: it inherits whatever the cookie's SameSite
// attribute gives and nothing more (credentialFrom's comment in the
// scaffolded pkg/middleware/middleware.go).
func (m Method) IsBrowserSession() bool {
	return m.ViaCookie() && m.Credential == auth.CredentialSession
}

// methodContextKey is the private context key the method stash uses, so the
// interceptor and [ContextWithAuthMethod] are the only writers.
type methodContextKey struct{}

// AuthMethod returns how the request's caller authenticated. It returns the
// zero Method and false when the context carries none: an allow-listed
// procedure, a claim-less request Policy.AnonymousOK let through, or a
// passthrough interceptor (Policy.ExternalAuth), which authenticates
// nothing.
//
// The interceptor records it whenever it admits a caller, also when
// Policy.ContextWithClaims names a project's own claims stash.
func AuthMethod(ctx context.Context) (Method, bool) {
	m, ok := ctx.Value(methodContextKey{}).(Method)
	return m, ok
}

// ContextWithAuthMethod returns a new context carrying m, read back by
// [AuthMethod]. The interceptor uses it; call it yourself in a handler test
// (as [ContextWithClaims] is called there) or in an interceptor of your own
// that authenticates in this one's place.
func ContextWithAuthMethod(ctx context.Context, m Method) context.Context {
	return context.WithValue(ctx, methodContextKey{}, m)
}

// inferChannel names the channel a legacy Policy.ExtractToken read token
// from, by finding token in the request — so a project whose extractor
// predates Policy.ExtractCredential gets AuthMethod with no edit.
//
// The candidates are tried in credentialFrom's presence order: the
// Authorization header, then the API-key header, then the cookies. When two
// carry the same value the earlier wins, so a token that is both a bearer
// and a cookie value is reported as Authorization: a script can always set
// the header, and when in doubt the answer must not be the cookie. A token
// found in none of them — an extractor reading a header inference does not
// know — is ChannelUnknown, which no cookie check passes.
//
// Cookie is reported only when the cookie is the ONLY place the token
// appears. Finding the value in a cookie does not prove the extractor read
// it there: one reading X-Session would accept the token from that header
// while a caller copied it into a cookie as well. An ExtractToken sees
// nothing but the headers, so when no header but Cookie contains the token,
// the cookie is where the extractor found it; otherwise the answer is
// Unknown. A project that wants the channel stated rather than inferred
// sets Policy.ExtractCredential.
func inferChannel(header http.Header, token string) Channel {
	for _, v := range header.Values("Authorization") {
		v = strings.TrimSpace(v)
		if bearer, ok := strings.CutPrefix(v, "Bearer "); ok && strings.TrimSpace(bearer) == token {
			return ChannelAuthorization
		}
		if v == token {
			return ChannelAuthorization
		}
	}
	for _, v := range header.Values(defaultAPIKeyHeader) {
		if strings.TrimSpace(v) == token {
			return ChannelAPIKeyHeader
		}
	}
	for _, c := range (&http.Request{Header: header}).Cookies() {
		if c.Value == token {
			if tokenOutsideCookies(header, token) {
				return ChannelUnknown
			}
			return ChannelCookie
		}
	}
	return ChannelUnknown
}

// tokenOutsideCookies reports whether any header but Cookie carries token,
// whole or as part of its value.
func tokenOutsideCookies(header http.Header, token string) bool {
	for name, vs := range header {
		if http.CanonicalHeaderKey(name) == "Cookie" {
			continue
		}
		for _, v := range vs {
			if strings.Contains(v, token) {
				return true
			}
		}
	}
	return false
}

// defaultAPIKeyHeader is the API-key header forge scaffolds and
// forge/pkg/auth's APIKeyConfig defaults to. A project that renamed it gets
// ChannelUnknown for keys on its own header from inference;
// Policy.ExtractCredential names the channel exactly.
const defaultAPIKeyHeader = "X-API-Key"
