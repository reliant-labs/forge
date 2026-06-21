package authz

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/reliant-labs/forge/pkg/forgepb"
)

// FromDescriptors builds an [*Authorizer] at runtime by walking registered
// proto FileDescriptors and reading the per-method (forge.v1.method) option,
// instead of consuming a per-service generated authorizer table.
//
// For every method of every service it finds, it constructs the Connect
// procedure path ("/<pkg>.<Service>/<Method>") and records whether the method
// requires authentication. The rule mirrors the proto-level contract:
//
//   - method annotated with auth_required = true  → MethodAuthRequired = true
//   - method annotated with auth_required = false → MethodAuthRequired = false
//     (the method is reachable without authentication)
//   - method with NO explicit auth_required (no (forge.v1.method) annotation,
//     or one that sets only other fields) → left OUT of the table, so it hits
//     the unknown-method path: [FailClosed] denies it (the default),
//     [AllowUnknownMethods] serves it. This keeps a forgotten annotation a
//     loud, fail-closed event rather than a silent require-auth default.
//
// The proto carries only auth_required; ROLES are user-owned policy and never
// live in the proto. Supply them out-of-band with [WithRoleOverlay]. The
// default empty overlay means "any authenticated user", matching today's
// daemon behaviour where MethodRoles is empty.
//
// The returned [*Authorizer] wraps a [RolesDecider]; compose it with the
// existing [Interceptor] to enforce it on a Connect handler. Construction
// does not read claims — wire [SetClaimsLookup] (the generated shim does this)
// so CanAccess can resolve claims at request time.
//
// By default it scans [protoregistry.GlobalFiles]; pass [WithFiles] to scope
// the scan to an explicit descriptor set (used by tests and by callers that
// want to bound which services participate).
func FromDescriptors(opts ...Option) (*Authorizer, error) {
	cfg := config{
		files:    protoregistry.GlobalFiles,
		overlay:  nil,
		failMode: FailClosed,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.files == nil {
		return nil, fmt.Errorf("authz.FromDescriptors: descriptor source is nil")
	}

	methodAuthRequired := make(map[string]bool)
	var scanErr error
	cfg.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			methods := svc.Methods()
			for j := 0; j < methods.Len(); j++ {
				m := methods.Get(j)
				authReq, annotated := methodAuthRequiredFor(m)
				if !annotated {
					// No explicit auth_required: leave it out of the table so
					// it routes through the unknown-method FailMode path
					// (fail-closed denies; AllowUnknownMethods serves).
					continue
				}
				procedure := procedurePath(svc, m)
				if _, dup := methodAuthRequired[procedure]; dup {
					// Two registered descriptors yielded the same Connect
					// procedure path. That is a genuine ambiguity (duplicate
					// service registration / colliding package names) and we
					// refuse to silently pick one mapping over the other.
					scanErr = fmt.Errorf(
						"authz.FromDescriptors: duplicate procedure %q across registered descriptors",
						procedure)
					return false
				}
				methodAuthRequired[procedure] = authReq
			}
		}
		return true
	})
	if scanErr != nil {
		return nil, scanErr
	}

	overlay := cfg.overlay
	if overlay == nil {
		// A nil MethodRoles map with a non-nil empty Default means every
		// known method allows any authenticated user. We keep MethodRoles
		// nil (cheaper) and rely on Default; see decider construction below.
		overlay = map[string][]string{}
	}

	dec := RolesDecider{
		MethodRoles:        overlay,
		MethodAuthRequired: methodAuthRequired,
		// Default empty (non-nil) slice: a known method with no explicit
		// role overlay entry is allowed for any authenticated user. This
		// matches the daemon's current empty-roles behaviour. Unknown
		// procedures never reach this branch for a known method — they are
		// governed by FailMode via MethodAuthRequired's miss path.
		Default:         []string{},
		FailMode:        cfg.failMode,
		OnUnknownMethod: cfg.onUnknownMethod,
	}
	return New(dec), nil
}

// procedurePath returns the Connect procedure string for a method, exactly
// as connect-go reports it in req.Spec().Procedure: a leading slash, the
// fully-qualified service name (proto package + service, dot-separated), a
// slash, and the bare method name. Nested proto packages are handled by
// svc.FullName(), which is already the dotted path (e.g.
// "proto.services.users.v1.UsersService").
func procedurePath(svc protoreflect.ServiceDescriptor, m protoreflect.MethodDescriptor) string {
	return "/" + string(svc.FullName()) + "/" + string(m.Name())
}

// methodAuthRequiredFor reads the (forge.v1.method) option off a method.
//
// It returns (authRequired, annotated). annotated is false when the method
// carries no (forge.v1.method) extension, or carries one that does not set the
// optional auth_required field — in which case the caller leaves the method
// out of the table so it routes through the unknown-method FailMode path. Only
// an explicit auth_required value (true or false) yields annotated == true,
// honouring the proto field's distinguishable-optional contract.
func methodAuthRequiredFor(m protoreflect.MethodDescriptor) (authRequired, annotated bool) {
	opts := m.Options()
	if opts == nil {
		return false, false
	}
	ext := proto.GetExtension(opts, forgepb.E_Method)
	mo, ok := ext.(*forgepb.MethodOptions)
	if !ok || mo == nil || mo.AuthRequired == nil {
		return false, false
	}
	return *mo.AuthRequired, true
}

// fileSource is the subset of *protoregistry.Files that FromDescriptors needs.
// Narrowing to this interface lets [WithFiles] accept either the global
// registry or a test-scoped *protoregistry.Files without a concrete-type
// dependency leaking into the option signature.
type fileSource interface {
	RangeFiles(func(protoreflect.FileDescriptor) bool)
}

type config struct {
	files           fileSource
	overlay         map[string][]string
	failMode        FailMode
	onUnknownMethod func(method string)
}

// Option configures [FromDescriptors].
type Option func(*config)

// WithFiles scopes the descriptor scan to an explicit file source instead of
// [protoregistry.GlobalFiles]. The common production caller omits this and
// scans the global registry (every imported .pb.go self-registers there).
// Tests pass a bounded *protoregistry.Files so one test's descriptors do not
// bleed into another's.
func WithFiles(files fileSource) Option {
	return func(c *config) { c.files = files }
}

// WithRoleOverlay supplies the user-owned MethodRoles map. Keys are Connect
// procedure paths ("/<pkg>.<Service>/<Method>"); values are the allowed roles
// for that method (empty slice == any authenticated user). Procedures absent
// from the overlay fall through to "any authenticated user" for known methods.
// Roles are deliberately NOT carried in the proto — they are policy the caller
// owns and may source from config, a DB, or a policy engine.
func WithRoleOverlay(overlay map[string][]string) Option {
	return func(c *config) { c.overlay = overlay }
}

// WithFailMode sets the [FailMode] applied to procedures the scan never saw
// (proto drift, hand-mounted endpoints, probes). Default is [FailClosed].
func WithFailMode(mode FailMode) Option {
	return func(c *config) { c.failMode = mode }
}

// WithOnUnknownMethod wires the once-per-method unknown-procedure callback
// passed through to the underlying [RolesDecider]. nil (the default) emits the
// standard slog warning.
func WithOnUnknownMethod(fn func(method string)) Option {
	return func(c *config) { c.onUnknownMethod = fn }
}
