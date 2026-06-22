//go:build ignore

// role_resolver.go is YOUR authorization identity seam — forge scaffolded it
// once and will not rewrite it.
//
// forge's descriptor-driven authorization works like this:
//
//   - Role policy is declared in the PROTO: (forge.v1.method).required_roles /
//     .authz_public per RPC, with (forge.v1.service).default_roles as the
//     service-wide floor. The starter service ships an example.
//   - ONE shared interceptor (forge/pkg/authz.RoleInterceptor) reads that
//     policy off the descriptor once at startup and enforces it on every RPC.
//   - The ONLY thing your app implements is identity→roles: given a request,
//     which roles does the caller hold? That is the RoleResolver below.
//
// The completeness lint (`forge generate`) fails the build if any method is
// not explicitly authorized, and the library fails CLOSED at runtime if a
// method ever slips through — so a method can never silently default to open.
//
// See the forge `auth` skill for the full model.
package middleware

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/authz"
)

// RoleResolver maps an authenticated request to the roles its caller holds.
// It is the one seam forge's authz library asks your app to fill.
//
// The default implementation reads roles off the validated Claims that the
// auth interceptor already stashed on the context (Claims.Role + Claims.Roles
// — populated by your token validator and the enrichClaims hook in
// middleware.go). For most projects that is all you need: keep your role
// enrichment in enrichClaims and let this resolver project it.
//
// TODO(authz): adjust to your identity model. Common variants:
//   - resource-scoped roles: use the `procedure` argument to look up the
//     caller's role FOR the target resource (e.g. org-scoped or project-scoped
//     membership) rather than returning global roles.
//   - external lookup: call your user/permission service here (cache it — this
//     runs on every RPC).
//   - non-JWT identity: read an mTLS SPIFFE ID or a gateway-injected header off
//     the context instead of Claims.
type RoleResolver struct{}

// NewRoleResolver returns the project's RoleResolver. Wire it into the shared
// authz interceptor in cmd/server.go (or your bootstrap) like:
//
//	policy, err := authz.PolicyFromDescriptors(
//	    // optional: declare role implication, e.g. admin satisfies member-only RPCs
//	    authz.WithRoleImplication(map[authz.Role][]authz.Role{
//	        "admin": {"member"},
//	    }),
//	)
//	if err != nil { return err }
//	interceptors = append(interceptors,
//	    authz.RoleInterceptor(policy, middleware.NewRoleResolver()))
func NewRoleResolver() authz.RoleResolver { return RoleResolver{} }

// RolesFor implements authz.RoleResolver. It returns the roles the caller in
// ctx holds. An unauthenticated request returns CodeUnauthenticated, which the
// interceptor surfaces verbatim; an authenticated caller with no roles returns
// an empty slice (allowed only for authz_public methods, denied for every
// role-restricted method — fail-closed by construction).
func (RoleResolver) RolesFor(ctx context.Context, _ string) ([]authz.Role, error) {
	claims, ok := ClaimsFromContext(ctx)
	if !ok || claims == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("no authenticated user"))
	}
	roles := make([]authz.Role, 0, len(claims.Roles)+1)
	if claims.Role != "" {
		roles = append(roles, authz.Role(claims.Role))
	}
	for _, r := range claims.Roles {
		roles = append(roles, authz.Role(r))
	}
	return roles, nil
}
