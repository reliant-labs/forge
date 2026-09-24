package accesstoken

import "strings"

// Scope names one authority a token may carry, spelled <product>:<action>.
//
// SCOPES ARE THE ONLY THING THAT DISTINGUISHES ONE CREDENTIAL FROM ANOTHER.
// A CI deploy token, a daemon's connection credential, an LLM gateway key and
// a connector credential differ in their scope set (and binding), not in their
// table, prefix, hashing or authentication path.
//
// THE SET IS CLOSED. A scope that cannot be spelled cannot be granted, which is
// a stronger guarantee than a handler remembering to refuse it. A store backs
// this with a database CHECK over the same list; each deployment's test pins
// its CHECK to AllScopes so neither can gain a member alone.
type Scope string

const (
	// ScopeDeployRead reads the org's deploy state.
	ScopeDeployRead Scope = "deploy:read"
	// ScopeDeployWrite mutates it (releases, environments, publish,
	// promote, rollback). Implies deploy:read at the point of use.
	ScopeDeployWrite Scope = "deploy:write"

	// ScopeTokenRead lists the org's machine credentials — an inventory of
	// its automation, so a separate authority from deploy:read.
	ScopeTokenRead Scope = "token:read"
	// ScopeTokenWrite mints and revokes credentials, bounded by Covers: a
	// holder can never grant a scope it does not hold. Without this scope a
	// token cannot mint at all, which is the general form of "a PAT cannot
	// mint a PAT".
	ScopeTokenWrite Scope = "token:write"

	// ScopeReliantAPI acts as the token's acting user against the reliant
	// API — the CLI / automation credential. Requires an acting user.
	ScopeReliantAPI Scope = "reliant:api"

	// ScopeDaemonConnect authenticates a daemon's stream to the gateway.
	// When bound to a daemon resource it authenticates as that daemon only.
	ScopeDaemonConnect Scope = "daemon:connect"

	// ScopeLLMInvoke calls the LLM gateway, billed to the acting user.
	ScopeLLMInvoke Scope = "llm:invoke"

	// ScopeProxyPort reaches one proxied daemon port. Always bound to a
	// port resource: an unbound port credential would open every port.
	ScopeProxyPort Scope = "proxy:port"

	// ScopeMCPConnector is a third-party MCP client's credential, confined to
	// one connector grant. Always bound to a connector resource.
	ScopeMCPConnector Scope = "mcp:connector"

	// ScopeSecretRead lists the org's managed secrets' METADATA (names,
	// versions). Secret VALUES are never readable through a token: they are
	// materialized into workloads, not returned to callers.
	ScopeSecretRead Scope = "secret:read"
	// ScopeSecretWrite sets and deletes managed secret values. Implies
	// secret:read at the point of use.
	ScopeSecretWrite Scope = "secret:write"
)

// AllScopes is the closed set, grouped by product, least authority first.
var AllScopes = []Scope{
	ScopeDeployRead,
	ScopeDeployWrite,
	ScopeTokenRead,
	ScopeTokenWrite,
	ScopeReliantAPI,
	ScopeDaemonConnect,
	ScopeLLMInvoke,
	ScopeProxyPort,
	ScopeMCPConnector,
	ScopeSecretRead,
	ScopeSecretWrite,
}

// ParseScope maps a wire string onto a known Scope. An unknown string is an
// error, never a silently dropped entry: dropping "deploy:wrIte" would mint a
// token that authenticates and then refuses every write.
func ParseScope(s string) (Scope, bool) {
	for _, known := range AllScopes {
		if string(known) == s {
			return known, true
		}
	}
	return "", false
}

// Set is the authority a token carries. Build it with NewSet, the single place
// an unknown scope is rejected.
type Set map[Scope]struct{}

// NewSet builds a Set, refusing unknown and empty input.
//
// Refusing unknown scopes is the fail-closed direction on the READ path: a row
// written by a newer server may hold a scope this binary cannot evaluate, and
// proceeding with the understood subset would make a token's authority depend
// on which server version answered. An empty set is refused because a token
// with no authority that authenticates reads as an outage.
func NewSet(scopes []string) (Set, error) {
	if len(scopes) == 0 {
		return nil, &UnknownScopeError{Reason: "a token must carry at least one scope"}
	}
	out := make(Set, len(scopes))
	for _, raw := range scopes {
		scope, ok := ParseScope(raw)
		if !ok {
			return nil, &UnknownScopeError{Scope: raw, Reason: "not a scope this server recognises"}
		}
		out[scope] = struct{}{}
	}
	return out, nil
}

// SetOf builds a Set from known Scope constants. For call sites that name
// scopes in code, where an unknown value is a compile-time impossibility.
func SetOf(scopes ...Scope) Set {
	out := make(Set, len(scopes))
	for _, s := range scopes {
		out[s] = struct{}{}
	}
	return out
}

// UnknownScopeError names the scope that could not be evaluated.
type UnknownScopeError struct {
	Scope  string
	Reason string
}

func (e *UnknownScopeError) Error() string {
	if e.Scope == "" {
		return "accesstoken: " + e.Reason
	}
	return "accesstoken: scope " + e.Scope + ": " + e.Reason
}

// Has reports exact membership.
func (s Set) Has(scope Scope) bool {
	_, ok := s[scope]
	return ok
}

// Permits reports whether this set authorizes an action requiring `want`.
//
// WRITE IMPLIES READ, WITHIN ONE PRODUCT, AT THE POINT OF USE ONLY. The stored
// grant is not widened; a caller holding deploy:write may perform deploy reads
// because every write path reads first. No implication crosses products —
// deploy:write never implies token:read.
func (s Set) Permits(want Scope) bool {
	if s.Has(want) {
		return true
	}
	switch want {
	case ScopeDeployRead:
		return s.Has(ScopeDeployWrite)
	case ScopeTokenRead:
		return s.Has(ScopeTokenWrite)
	case ScopeSecretRead:
		return s.Has(ScopeSecretWrite)
	default:
		return false
	}
}

// Covers reports whether this set may GRANT every scope in `requested`.
//
// THE RULE: a token may never mint a token carrying scopes it does not itself
// hold. Authority can be narrowed on delegation and never widened.
//
// It uses EXACT MEMBERSHIP, not Permits. Permits' write-implies-read is a
// statement about what an action needs; routing grants through it would make
// every future usability implication a minting power.
//
// An empty request is not covered — a subset test that returns true for the
// empty set is how a caller concludes a token may grant anything.
func (s Set) Covers(requested Set) bool {
	if len(requested) == 0 {
		return false
	}
	for scope := range requested {
		if !s.Has(scope) {
			return false
		}
	}
	return true
}

// Missing returns the scopes in `requested` this set does not hold, in
// AllScopes order, so a refusal can name what was over-reached.
func (s Set) Missing(requested Set) []Scope {
	out := make([]Scope, 0, len(requested))
	for _, scope := range AllScopes {
		if requested.Has(scope) && !s.Has(scope) {
			out = append(out, scope)
		}
	}
	return out
}

// Strings returns the scopes as wire strings in AllScopes order — stable, so
// two reads of one unchanged token serialize identically.
func (s Set) Strings() []string {
	out := make([]string, 0, len(s))
	for _, scope := range AllScopes {
		if s.Has(scope) {
			out = append(out, string(scope))
		}
	}
	return out
}

// String renders the set for diagnostics. Scopes are not secret.
func (s Set) String() string { return strings.Join(s.Strings(), ",") }

// JoinScopes renders a scope slice for an error message.
func JoinScopes(scopes []Scope) string {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		out = append(out, string(scope))
	}
	return strings.Join(out, ", ")
}
