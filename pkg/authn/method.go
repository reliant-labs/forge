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
// key. Channel says WHERE the caller put it, which is a different fact: the
// same token authenticates identically whether it rode an Authorization
// header or a session cookie, but only a browser page sends a cookie it
// cannot read. See [Method.ViaCookie].
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
	// session cookie native sign-in sets.
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

// ViaCookie reports whether the credential arrived in a cookie.
//
// That is the "a person is using the app" signal. A browser page's ONLY
// channel is the session cookie: it is HttpOnly, so the page's scripts can
// neither read it nor copy it into a header, and the browser attaches it
// only to requests from the app itself. Software holding a bearer token —
// a CLI, an integration, an agent with a token — presents it in the
// Authorization header instead, so requiring ViaCookie on a sensitive
// action (approving something, managing credentials) refuses that same
// user's token held by a program.
//
// What it does not say:
//
//   - It is not a CSRF defence. It inherits whatever the cookie's
//     SameSite attribute gives and nothing more; the scaffolded
//     pkg/middleware/middleware.go (credentialFrom's comment) explains why
//     a Connect call with a SameSite=Lax session cookie is not a CSRF
//     opening, and the login broker's origin check covers same-site
//     siblings.
//   - A client outside a browser can set any Cookie header it likes, so a
//     cookie whose value is the same token the caller could hold as a
//     bearer proves less than one whose value is an opaque server-side
//     session id. Check Credential too when the project issues one.
//   - An API key presented in a cookie is still a key: see
//     [Method.IsBrowserSession].
func (m Method) ViaCookie() bool { return m.Channel == ChannelCookie }

// IsBrowserSession reports whether the caller is a person's browser
// session: the credential arrived in a cookie ([Method.ViaCookie]) and was
// not an API key. It is false for the zero Method, so a request with no
// recorded method — an allow-listed procedure, an anonymous caller, a
// passthrough interceptor — never passes it.
func (m Method) IsBrowserSession() bool {
	return m.ViaCookie() && m.Credential != auth.CredentialAPIKey
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
			return ChannelCookie
		}
	}
	return ChannelUnknown
}

// defaultAPIKeyHeader is the API-key header forge scaffolds and
// forge/pkg/auth's APIKeyConfig defaults to. A project that renamed it gets
// ChannelUnknown for keys on its own header from inference;
// Policy.ExtractCredential names the channel exactly.
const defaultAPIKeyHeader = "X-API-Key"
