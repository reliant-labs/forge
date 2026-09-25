package accesstoken

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ResourceKind names what a token may be BOUND to. The set is closed.
//
// A binding confines a token to one resource: a daemon credential that works
// for that daemon only, a share link that opens one port, a connector
// credential confined to one grant. It bounds a leaked token's blast radius to
// the one thing it was issued for, which is why it is carried over on merit
// from reliant's per-daemon PATs.
type ResourceKind string

const (
	// ResourceDaemon binds to one daemon id.
	ResourceDaemon ResourceKind = "daemon"
	// ResourcePort binds to one proxied port of one daemon. The id is
	// PortResourceID(daemonID, port).
	ResourcePort ResourceKind = "port"
	// ResourceConnector binds to one MCP connector grant id.
	ResourceConnector ResourceKind = "connector"
)

// AllResourceKinds is the closed set; stores pin their CHECK to it.
var AllResourceKinds = []ResourceKind{ResourceDaemon, ResourcePort, ResourceConnector}

// Resource is a binding: a kind and an id.
type Resource struct {
	Kind ResourceKind
	ID   string
}

// PortResourceID renders the id of a port binding. One spelling, here, so a
// minting site and a checking site cannot disagree about the separator.
func PortResourceID(daemonID string, port int32) string {
	return fmt.Sprintf("%s:%d", daemonID, port)
}

// scopeResourceRules says, per scope, which binding it requires or allows.
//
//	required — the scope is meaningless (or dangerous) unbound: a port
//	           credential with no port would open every port.
//	allowed  — the scope may be bound or unbound. An unbound daemon:connect
//	           is a user registering a new daemon; a bound one is a managed
//	           daemon that may authenticate as that daemon only.
//
// A scope with no entry may not be bound at all: a deploy token bound to a
// port is a contradiction, and refusing it keeps a binding's meaning exact.
var scopeResourceRules = map[Scope]struct {
	kind     ResourceKind
	required bool
}{
	ScopeDaemonConnect: {kind: ResourceDaemon, required: false},
	ScopeProxyPort:     {kind: ResourcePort, required: true},
	ScopeMCPConnector:  {kind: ResourceConnector, required: true},
}

// actingUserScopes are the scopes that ACT AS A PERSON: their authority is the
// acting user's, so a token carrying one without an acting user would be
// authority attributed to nobody. Org-level automation (deploy, token, secret)
// needs no acting user; that is the org CI token.
var actingUserScopes = []Scope{
	ScopeReliantAPI,
	ScopeDaemonConnect,
	ScopeLLMInvoke,
	ScopeMCPConnector,
}

// RequiresActingUser reports whether a scope acts as a person.
func RequiresActingUser(scope Scope) bool {
	for _, s := range actingUserScopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ErrEscalation reports a mint that would grant authority the grantor does not
// hold. Distinct from ErrInvalidGrant because it is an authority problem
// (PermissionDenied), not a malformed request (InvalidArgument).
var ErrEscalation = errors.New("accesstoken: a token cannot grant scopes it does not hold")

// ErrInvalidGrant reports a malformed mint request.
var ErrInvalidGrant = errors.New("accesstoken: invalid grant")

// Grant is everything a mint decides. Every store validates it with Validate
// before inserting, so the rules below have one implementation.
type Grant struct {
	// OrgID is the one organization the token acts on, for its whole life.
	OrgID string
	// Name is the operator's label. Descriptive only.
	Name string
	// Scopes is the authority granted.
	Scopes Set
	// ActingUserID, when set, is the person the token acts as. Required by
	// any scope in actingUserScopes; absent for an org automation token.
	ActingUserID string
	// Resource, when set, confines the token to one resource.
	Resource *Resource
	// Ephemeral marks a token tied to a session's lifetime (a desktop
	// daemon's run), revoked when that session ends. It must be bound and
	// must expire: an ephemeral token that outlives everything is a
	// contradiction.
	Ephemeral bool
	// ExpiresAt is optional for non-ephemeral tokens. Checked at every
	// authentication; never defaulted.
	ExpiresAt *time.Time

	// GrantorScopes is the authority of the CALLER when the caller is itself
	// a token. NIL MEANS A HUMAN CALLER (or trusted internal service) whose
	// authority was checked by the handler before this; it is absence, not a
	// synthetic "every scope" value that could leak into another check.
	GrantorScopes Set
}

// Validate enforces every structural rule of a grant at time `now`.
func (g Grant) Validate(now time.Time) error {
	if strings.TrimSpace(g.OrgID) == "" {
		return fmt.Errorf("%w: org id is required", ErrInvalidGrant)
	}
	if strings.TrimSpace(g.Name) == "" {
		return fmt.Errorf("%w: a token needs a name", ErrInvalidGrant)
	}
	if len(g.Scopes) == 0 {
		return fmt.Errorf("%w: a token needs at least one scope", ErrInvalidGrant)
	}

	// The escalation rule. Nil grantor = human / internal caller.
	if g.GrantorScopes != nil && !g.GrantorScopes.Covers(g.Scopes) {
		return fmt.Errorf("%w: missing %s", ErrEscalation, JoinScopes(g.GrantorScopes.Missing(g.Scopes)))
	}

	for _, scope := range AllScopes {
		if g.Scopes.Has(scope) && RequiresActingUser(scope) && strings.TrimSpace(g.ActingUserID) == "" {
			return fmt.Errorf("%w: scope %s acts as a user and needs an acting user", ErrInvalidGrant, scope)
		}
	}

	if err := g.validateResource(); err != nil {
		return err
	}

	if g.Ephemeral {
		if g.Resource == nil {
			return fmt.Errorf("%w: an ephemeral token must be bound to a resource", ErrInvalidGrant)
		}
		if g.ExpiresAt == nil {
			return fmt.Errorf("%w: an ephemeral token must expire", ErrInvalidGrant)
		}
	}
	if g.ExpiresAt != nil && !g.ExpiresAt.After(now) {
		return fmt.Errorf("%w: expiry must be in the future", ErrInvalidGrant)
	}
	return nil
}

func (g Grant) validateResource() error {
	if g.Resource != nil {
		if !isResourceKind(g.Resource.Kind) {
			return fmt.Errorf("%w: unknown resource kind %q", ErrInvalidGrant, g.Resource.Kind)
		}
		if strings.TrimSpace(g.Resource.ID) == "" {
			return fmt.Errorf("%w: a resource binding needs an id", ErrInvalidGrant)
		}
	}
	for _, scope := range AllScopes {
		if !g.Scopes.Has(scope) {
			continue
		}
		rule, bindable := scopeResourceRules[scope]
		switch {
		case !bindable && g.Resource != nil:
			return fmt.Errorf("%w: scope %s cannot be bound to a resource", ErrInvalidGrant, scope)
		case bindable && g.Resource == nil && rule.required:
			return fmt.Errorf("%w: scope %s must be bound to a %s", ErrInvalidGrant, scope, rule.kind)
		case bindable && g.Resource != nil && g.Resource.Kind != rule.kind:
			return fmt.Errorf("%w: scope %s binds to a %s, not a %s", ErrInvalidGrant, scope, rule.kind, g.Resource.Kind)
		}
	}
	return nil
}

func isResourceKind(k ResourceKind) bool {
	for _, known := range AllResourceKinds {
		if known == k {
			return true
		}
	}
	return false
}

// Principal is what an authenticated token resolves to. Every validator —
// control-plane's in-process one, reliant's introspecting one, a self-host
// store — yields this shape.
type Principal struct {
	TokenID string
	OrgID   string
	// ActingUserID is empty for an org automation token. A consumer that
	// needs a person (a reliant API call, an LLM request) refuses an empty
	// one; it never substitutes the creator.
	ActingUserID string
	Scopes       Set
	// Resource is the binding, or nil for an unbound token.
	Resource  *Resource
	Ephemeral bool
	ExpiresAt *time.Time
}

// BoundTo reports whether the token is bound to exactly this resource.
func (p *Principal) BoundTo(kind ResourceKind, id string) bool {
	return p != nil && p.Resource != nil && p.Resource.Kind == kind && p.Resource.ID == id
}

// MayActOn reports whether the token may act on this resource: an unbound
// token may (its authority is its scopes and acting user), a bound one only on
// its own resource. Required-binding scopes never reach the unbound case,
// because Validate refuses to mint them unbound.
func (p *Principal) MayActOn(kind ResourceKind, id string) bool {
	if p == nil {
		return false
	}
	if p.Resource == nil {
		return true
	}
	return p.BoundTo(kind, id)
}
