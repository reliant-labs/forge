package authz

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/reliant-labs/forge/pkg/forgepb"
)

// =============================================================================
// Descriptor-driven, proto-annotated authorization.
//
// This is the canonical forge authz pattern: per-method role policy is
// declared as proto annotations ((forge.v1.method).required_roles /
// .authz_public, with (forge.v1.service).default_roles as the fallback) and
// enforced by ONE shared Connect interceptor that reads a policy built once
// from the proto DESCRIPTOR. There is no generated policy table — the policy
// IS the descriptor, the same philosophy as the descriptor-driven config
// loader. The app supplies only identity→roles via a [RoleResolver]; the
// library owns the lookup, the decision (role-set satisfaction + implication),
// and the fail-closed default.
//
// Relationship to the older Decider/RolesDecider path in authz.go: that path
// is interface-driven with roles supplied out-of-band (an overlay map). This
// path moves roles into the proto and reads them off the descriptor. Both
// share the [Role] type and the [Interceptor]/[AccessChecker] seam, so a
// project can adopt either without re-plumbing its handlers.
// =============================================================================

// Role is a string-backed authorization role. Projects typically derive the
// concrete role values from their own roles proto enum (the scaffold ships a
// starter `Role` enum), but any string the [RoleResolver] returns and the
// proto annotations name will match — the library never inspects role
// semantics beyond set membership and the optional implication map.
type Role string

// RoleResolver maps an authenticated request to the roles its caller holds.
//
// This is the ONE seam the app implements: identity→roles. The library calls
// RolesFor at request time with the request context (carrying whatever the
// auth layer stashed — claims, headers, an mTLS identity) and the procedure
// being invoked, and decides allow/deny by comparing the returned roles
// against the method's proto-declared required roles.
//
// Returning an error denies the call (surfaced as PermissionDenied unless the
// error is already a *connect.Error). Returning an empty slice with no error
// means "an admitted caller with no roles" — allowed only for methods marked
// authz_public; every role-restricted method denies.
type RoleResolver interface {
	// RolesFor returns the roles the caller in ctx holds for the given
	// procedure (the full Connect method path,
	// e.g. "/shop.v1.OrderService/Create"). procedure is passed so a
	// resolver MAY scope roles per-procedure (e.g. resource-scoped roles),
	// though most resolvers ignore it and return the caller's global roles.
	RolesFor(ctx context.Context, procedure string) ([]Role, error)
}

// RoleResolverFunc adapts an ordinary function to [RoleResolver].
type RoleResolverFunc func(ctx context.Context, procedure string) ([]Role, error)

// RolesFor implements [RoleResolver].
func (f RoleResolverFunc) RolesFor(ctx context.Context, procedure string) ([]Role, error) {
	return f(ctx, procedure)
}

// methodPolicy is the per-procedure decision input built from proto
// annotations. A procedure is in the policy map iff it was explicitly
// authorized in the proto (required_roles set, authz_public set, or a
// service default_roles applied). A procedure absent from the map is
// unknown — it fails closed (see [RolePolicy.Check]).
type methodPolicy struct {
	// requiredRoles is the any-of allow-list. Empty + public==true means
	// any admitted caller; empty + public==false cannot occur (the builder
	// rejects it so an empty-but-not-public method is never silently open).
	requiredRoles []Role
	// public marks an authz_public method: any caller the auth layer
	// admits passes, with no role check.
	public bool
}

// RolePolicy is the immutable, descriptor-built authorization policy: a map
// from Connect procedure path to its required roles, plus the configured role
// implication. Build it ONCE at startup with [PolicyFromDescriptors] and hand
// it to [RoleInterceptor]; it is safe for concurrent use.
type RolePolicy struct {
	methods   map[string]methodPolicy
	implies   map[Role][]Role // transitively expanded: role → roles it grants
	failMode  FailMode
	onUnknown func(procedure string)

	warnOnce sync.Map // procedure → struct{}, dedups the unknown-method warning
}

// Check reports whether a caller holding callerRoles may invoke procedure
// under this policy. It returns nil to allow, or a *connect.Error to deny.
//
// Decision order:
//
//   - procedure not in the policy (proto drift / hand-mounted endpoint /
//     probe): FailClosed (the default) denies with a once-per-procedure
//     warning; AllowUnknownMethods serves it.
//   - public method: allow unconditionally.
//   - role-restricted method: allow iff callerRoles (expanded by the
//     implication map) intersects the method's required roles; else deny.
//
// Check does NOT resolve identity — the interceptor does that via the
// [RoleResolver] and passes the resolved roles in. Exposed directly so it is
// unit-testable and reusable from a non-Connect call site.
func (p *RolePolicy) Check(procedure string, callerRoles []Role) error {
	mp, known := p.methods[procedure]
	if !known {
		p.warnUnknown(procedure)
		if p.failMode == AllowUnknownMethods {
			return nil
		}
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("authz: procedure %q has no authorization policy (denied by default)", procedure))
	}
	if mp.public {
		return nil
	}
	if p.satisfies(callerRoles, mp.requiredRoles) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("authz: caller lacks a required role for %q (need one of %v)", procedure, rolesToStrings(mp.requiredRoles)))
}

// satisfies reports whether the caller's roles (after implication expansion)
// include at least one of the required roles. An empty required set is only
// reached for public methods, which Check short-circuits, so here it means
// "no role grants access" → false unless the caller is expanded to match.
func (p *RolePolicy) satisfies(callerRoles, required []Role) bool {
	if len(required) == 0 {
		return false
	}
	have := make(map[Role]struct{}, len(callerRoles)*2)
	for _, r := range callerRoles {
		have[r] = struct{}{}
		for _, granted := range p.implies[r] {
			have[granted] = struct{}{}
		}
	}
	for _, want := range required {
		if _, ok := have[want]; ok {
			return true
		}
	}
	return false
}

func (p *RolePolicy) warnUnknown(procedure string) {
	if _, already := p.warnOnce.LoadOrStore(procedure, struct{}{}); already {
		return
	}
	if p.onUnknown != nil {
		p.onUnknown(procedure)
		return
	}
	// Reuse the package default warning shape used by RolesDecider so both
	// descriptor paths emit the same operator-visible signal.
	(RolesDecider{}).warnUnknownMethod(procedure)
}

// Methods returns the procedures the policy covers, sorted. Useful for boot
// logging ("authz: enforcing N procedures") and for the completeness check
// in tests. The returned slice is a copy.
func (p *RolePolicy) Methods() []string {
	out := make([]string, 0, len(p.methods))
	for k := range p.methods {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RequiredRoles returns the declared required roles for a procedure and
// whether the procedure is known to the policy. A public method returns
// (nil, true). The returned slice is a copy.
func (p *RolePolicy) RequiredRoles(procedure string) ([]Role, bool) {
	mp, ok := p.methods[procedure]
	if !ok {
		return nil, false
	}
	return append([]Role(nil), mp.requiredRoles...), true
}

// =============================================================================
// Policy builder — walk the proto descriptor once.
// =============================================================================

// roleConfig accumulates [PolicyOption] settings.
type roleConfig struct {
	files     fileSource
	implies   map[Role][]Role
	failMode  FailMode
	onUnknown func(procedure string)
}

// PolicyOption configures [PolicyFromDescriptors].
type PolicyOption func(*roleConfig)

// WithPolicyFiles scopes the descriptor scan to an explicit file source
// instead of [protoregistry.GlobalFiles]. Production callers omit this (every
// imported .pb.go self-registers globally); tests pass a bounded
// *protoregistry.Files so one test's descriptors do not bleed into another's.
func WithPolicyFiles(files fileSource) PolicyOption {
	return func(c *roleConfig) { c.files = files }
}

// WithRoleImplication declares that holding the key role implies holding each
// listed role (e.g. {"admin": {"user"}} — an admin satisfies any method that
// requires "user"). Implication is applied transitively at build time, so
// {"owner": {"admin"}, "admin": {"user"}} grants an owner the user role too.
// Cycles are tolerated (the expansion fixpoints). When unset, roles match
// only themselves.
func WithRoleImplication(implies map[Role][]Role) PolicyOption {
	return func(c *roleConfig) { c.implies = implies }
}

// WithPolicyFailMode sets the [FailMode] applied to procedures absent from the
// built policy (proto drift, hand-mounted endpoints, probes). Default is
// [FailClosed] — defense in depth behind the generate-time completeness lint.
func WithPolicyFailMode(mode FailMode) PolicyOption {
	return func(c *roleConfig) { c.failMode = mode }
}

// WithPolicyOnUnknown wires the once-per-procedure unknown-method callback.
// nil (the default) emits the standard slog warning.
func WithPolicyOnUnknown(fn func(procedure string)) PolicyOption {
	return func(c *roleConfig) { c.onUnknown = fn }
}

// PolicyFromDescriptors builds a [*RolePolicy] by walking registered proto
// FileDescriptors and reading the per-method/per-service authz annotations.
// It runs ONCE at startup; the resulting policy is immutable and the shared
// [RoleInterceptor] consults it on every RPC.
//
// For each method it resolves the effective policy:
//
//   - (forge.v1.method).authz_public = true → public (any admitted caller).
//   - (forge.v1.method).required_roles = [...] → those roles (any-of).
//   - neither, but (forge.v1.service).default_roles = [...] → the service
//     default applies to the method.
//   - none of the above → the method is left OUT of the policy, so it routes
//     through the unknown-method FailMode path (FailClosed denies). This is
//     the runtime backstop for the generate-time completeness lint: a
//     forgotten annotation is a loud, fail-closed event, never a silent open.
//
// A method that sets BOTH required_roles and authz_public is a contradiction;
// the builder fails so the ambiguity can never ship (the lint also catches it
// at generate time, but the builder is the last line of defense).
func PolicyFromDescriptors(opts ...PolicyOption) (*RolePolicy, error) {
	cfg := roleConfig{files: protoregistry.GlobalFiles, failMode: FailClosed}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.files == nil {
		return nil, fmt.Errorf("authz.PolicyFromDescriptors: descriptor source is nil")
	}

	methods := make(map[string]methodPolicy)
	var scanErr error
	cfg.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			defaults := serviceDefaultRoles(svc)
			ms := svc.Methods()
			for j := 0; j < ms.Len(); j++ {
				m := ms.Get(j)
				procedure := procedurePath(svc, m)
				mp, ok, err := methodPolicyFor(m, defaults)
				if err != nil {
					scanErr = fmt.Errorf("%s: %w", procedure, err)
					return false
				}
				if !ok {
					// Unannotated and no service default: leave out so it
					// routes through the unknown-method FailMode path.
					continue
				}
				if _, dup := methods[procedure]; dup {
					scanErr = fmt.Errorf(
						"authz.PolicyFromDescriptors: duplicate procedure %q across registered descriptors", procedure)
					return false
				}
				methods[procedure] = mp
			}
		}
		return true
	})
	if scanErr != nil {
		return nil, scanErr
	}

	return &RolePolicy{
		methods:   methods,
		implies:   expandImplications(cfg.implies),
		failMode:  cfg.failMode,
		onUnknown: cfg.onUnknown,
	}, nil
}

// methodPolicyFor resolves the effective policy for a single method.
// ok==false means the method is unannotated AND inherits no service default
// (caller leaves it out → unknown-method path). An error is a contradictory
// annotation (both public and roles).
func methodPolicyFor(m protoreflect.MethodDescriptor, serviceDefaults []Role) (mp methodPolicy, ok bool, err error) {
	public, roles, annotated := methodAuthzAnnotation(m)
	if annotated {
		if public && len(roles) > 0 {
			return methodPolicy{}, false, fmt.Errorf(
				"method sets both authz_public and required_roles — pick one")
		}
		if public {
			return methodPolicy{public: true}, true, nil
		}
		return methodPolicy{requiredRoles: roles}, true, nil
	}
	if len(serviceDefaults) > 0 {
		return methodPolicy{requiredRoles: serviceDefaults}, true, nil
	}
	return methodPolicy{}, false, nil
}

// methodAuthzAnnotation reads (forge.v1.method) and reports the explicit authz
// intent. annotated is true iff the method explicitly declared authz_public or
// a non-empty required_roles. An empty required_roles with authz_public unset
// is treated as NOT annotated (the lint requires an explicit choice), so it is
// never silently open. Both public and roles are returned even when both are
// set so [methodPolicyFor] can reject the contradiction.
func methodAuthzAnnotation(m protoreflect.MethodDescriptor) (public bool, roles []Role, annotated bool) {
	opts := m.Options()
	if opts == nil {
		return false, nil, false
	}
	ext := proto.GetExtension(opts, forgepb.E_Method)
	mo, okType := ext.(*forgepb.MethodOptions)
	if !okType || mo == nil {
		return false, nil, false
	}
	public = mo.GetAuthzPublic()
	roles = stringsToRoles(mo.GetRequiredRoles())
	annotated = public || len(roles) > 0
	return public, roles, annotated
}

// serviceDefaultRoles reads (forge.v1.service).default_roles.
func serviceDefaultRoles(svc protoreflect.ServiceDescriptor) []Role {
	opts := svc.Options()
	if opts == nil {
		return nil
	}
	ext := proto.GetExtension(opts, forgepb.E_Service)
	so, ok := ext.(*forgepb.ServiceOptions)
	if !ok || so == nil {
		return nil
	}
	return stringsToRoles(so.GetDefaultRoles())
}

// expandImplications computes the transitive closure of the implication map so
// Check does a single-hop lookup at request time. Cycles fixpoint safely.
func expandImplications(raw map[Role][]Role) map[Role][]Role {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[Role][]Role, len(raw))
	for r := range raw {
		seen := map[Role]struct{}{r: {}}
		var stack []Role
		stack = append(stack, raw[r]...)
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if _, ok := seen[cur]; ok {
				continue
			}
			seen[cur] = struct{}{}
			stack = append(stack, raw[cur]...)
		}
		delete(seen, r) // a role doesn't "imply" itself in the granted set
		granted := make([]Role, 0, len(seen))
		for g := range seen {
			granted = append(granted, g)
		}
		sort.Slice(granted, func(i, j int) bool { return granted[i] < granted[j] })
		out[r] = granted
	}
	return out
}

func stringsToRoles(ss []string) []Role {
	out := make([]Role, len(ss))
	for i, s := range ss {
		out[i] = Role(s)
	}
	return out
}

func rolesToStrings(rs []Role) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

// =============================================================================
// Interceptor — the ONE shared enforcement point.
// =============================================================================

// RoleInterceptor returns a Connect interceptor that enforces the
// descriptor-built [RolePolicy] on every unary and streaming-handler RPC. On
// each call it resolves the caller's roles via resolver, then asks the policy
// whether those roles satisfy the procedure's declared requirement, denying
// with PermissionDenied (loudly) on failure.
//
// policy and resolver must be non-nil; passing nil is a construction bug that
// would nil-panic per request, so the constructor panics at boot instead.
func RoleInterceptor(policy *RolePolicy, resolver RoleResolver) connect.Interceptor {
	if policy == nil {
		panic("authz.RoleInterceptor: policy must not be nil; build one with authz.PolicyFromDescriptors")
	}
	if resolver == nil {
		panic("authz.RoleInterceptor: resolver must not be nil; implement authz.RoleResolver (the scaffolded stub fails closed)")
	}
	return &roleInterceptor{policy: policy, resolver: resolver}
}

type roleInterceptor struct {
	policy   *RolePolicy
	resolver RoleResolver
}

// check runs the resolve→decide pipeline for one procedure. A resolver error
// denies: a *connect.Error passes through verbatim, anything else becomes
// PermissionDenied so a misbehaving resolver fails closed.
func (i *roleInterceptor) check(ctx context.Context, procedure string) error {
	roles, err := i.resolver.RolesFor(ctx, procedure)
	if err != nil {
		if connect.CodeOf(err) != connect.CodeUnknown {
			return err // already a typed connect error (e.g. Unauthenticated)
		}
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	return i.policy.Check(procedure, roles)
}

func (i *roleInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := i.check(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *roleInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // client-side: no server authz to enforce
}

func (i *roleInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := i.check(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
