package authz

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/reliant-labs/forge/pkg/forgepb"
)

// buildRolePolicyFiles compiles a synthetic proto with one service carrying a
// variety of authz annotations, plus a service-level default_roles, and
// returns it as a scoped *protoregistry.Files. Building real descriptors (vs.
// hand-stuffing the methods map) exercises the actual descriptor-walk +
// extension-read path PolicyFromDescriptors relies on.
func buildRolePolicyFiles(t *testing.T, pkgName string, serviceDefaults []string) *protoregistry.Files {
	t.Helper()

	method := func(name string, mo *forgepb.MethodOptions) *descriptorpb.MethodDescriptorProto {
		m := &descriptorpb.MethodDescriptorProto{
			Name:       proto.String(name),
			InputType:  proto.String("." + pkgName + ".Empty"),
			OutputType: proto.String("." + pkgName + ".Empty"),
		}
		if mo != nil {
			opts := &descriptorpb.MethodOptions{}
			proto.SetExtension(opts, forgepb.E_Method, mo)
			m.Options = opts
		}
		return m
	}

	var svcOpts *descriptorpb.ServiceOptions
	if len(serviceDefaults) > 0 {
		svcOpts = &descriptorpb.ServiceOptions{}
		proto.SetExtension(svcOpts, forgepb.E_Service, &forgepb.ServiceOptions{DefaultRoles: serviceDefaults})
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("authz_roles_testsvc.proto"),
		Package:     proto.String(pkgName),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name:    proto.String("TestService"),
				Options: svcOpts,
				Method: []*descriptorpb.MethodDescriptorProto{
					method("AdminOnly", &forgepb.MethodOptions{RequiredRoles: []string{"admin"}}),
					method("Public", &forgepb.MethodOptions{AuthzPublic: true}),
					method("Unannotated", nil),
					method("MultiRole", &forgepb.MethodOptions{RequiredRoles: []string{"editor", "admin"}}),
				},
			},
		},
	}

	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	files := &protoregistry.Files{}
	if err := files.RegisterFile(fd); err != nil {
		t.Fatalf("RegisterFile: %v", err)
	}
	return files
}

func TestPolicyFromDescriptors_RoleEnforcement(t *testing.T) {
	t.Cleanup(resetUnknownMethodWarnings)
	files := buildRolePolicyFiles(t, "authz.roles.enforce.v1", nil)
	p, err := PolicyFromDescriptors(WithPolicyFiles(files))
	if err != nil {
		t.Fatalf("PolicyFromDescriptors: %v", err)
	}

	const (
		adminOnly   = "/authz.roles.enforce.v1.TestService/AdminOnly"
		public      = "/authz.roles.enforce.v1.TestService/Public"
		unannotated = "/authz.roles.enforce.v1.TestService/Unannotated"
		multiRole   = "/authz.roles.enforce.v1.TestService/MultiRole"
	)

	// admin role allowed; non-admin denied with PermissionDenied.
	if err := p.Check(adminOnly, []Role{"admin"}); err != nil {
		t.Errorf("admin on AdminOnly: want allow, got %v", err)
	}
	if err := p.Check(adminOnly, []Role{"user"}); err == nil {
		t.Errorf("user on AdminOnly: want deny, got allow")
	} else if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("AdminOnly deny: want PermissionDenied, got %v", connect.CodeOf(err))
	}

	// public method: any caller (even no roles) passes.
	if err := p.Check(public, nil); err != nil {
		t.Errorf("no-role caller on Public: want allow, got %v", err)
	}

	// multi-role any-of: either role grants.
	if err := p.Check(multiRole, []Role{"editor"}); err != nil {
		t.Errorf("editor on MultiRole: want allow, got %v", err)
	}
	if err := p.Check(multiRole, []Role{"viewer"}); err == nil {
		t.Errorf("viewer on MultiRole: want deny, got allow")
	}

	// Unannotated, no service default → fail-closed deny, even for admin.
	if err := p.Check(unannotated, []Role{"admin"}); err == nil {
		t.Errorf("admin on Unannotated (fail-closed): want deny, got allow")
	}
	// Never-seen procedure → fail-closed deny.
	if err := p.Check("/authz.roles.enforce.v1.TestService/Ghost", []Role{"admin"}); err == nil {
		t.Errorf("ghost procedure (fail-closed): want deny, got allow")
	}
}

func TestPolicyFromDescriptors_ServiceDefaultRoles(t *testing.T) {
	t.Cleanup(resetUnknownMethodWarnings)
	// Service default = ["user"]; Unannotated inherits it.
	files := buildRolePolicyFiles(t, "authz.roles.default.v1", []string{"user"})
	p, err := PolicyFromDescriptors(WithPolicyFiles(files))
	if err != nil {
		t.Fatalf("PolicyFromDescriptors: %v", err)
	}
	unannotated := "/authz.roles.default.v1.TestService/Unannotated"
	if err := p.Check(unannotated, []Role{"user"}); err != nil {
		t.Errorf("user on service-default method: want allow, got %v", err)
	}
	if err := p.Check(unannotated, []Role{"guest"}); err == nil {
		t.Errorf("guest on service-default method: want deny, got allow")
	}
	// Method-level required_roles still overrides the service default.
	adminOnly := "/authz.roles.default.v1.TestService/AdminOnly"
	if err := p.Check(adminOnly, []Role{"user"}); err == nil {
		t.Errorf("user on method that overrides with admin: want deny, got allow")
	}
}

func TestPolicyFromDescriptors_AllowUnknownMethods(t *testing.T) {
	t.Cleanup(resetUnknownMethodWarnings)
	files := buildRolePolicyFiles(t, "authz.roles.allowunknown.v1", nil)
	p, err := PolicyFromDescriptors(WithPolicyFiles(files), WithPolicyFailMode(AllowUnknownMethods))
	if err != nil {
		t.Fatalf("PolicyFromDescriptors: %v", err)
	}
	unannotated := "/authz.roles.allowunknown.v1.TestService/Unannotated"
	if err := p.Check(unannotated, nil); err != nil {
		t.Errorf("Unannotated under AllowUnknownMethods: want allow, got %v", err)
	}
}

func TestRolePolicy_Implication(t *testing.T) {
	t.Cleanup(resetUnknownMethodWarnings)
	files := buildRolePolicyFiles(t, "authz.roles.implies.v1", nil)
	// owner ⊃ admin ⊃ user (transitive).
	p, err := PolicyFromDescriptors(files0WithImplication(files, map[Role][]Role{
		"owner": {"admin"},
		"admin": {"user"},
	}))
	if err != nil {
		t.Fatalf("PolicyFromDescriptors: %v", err)
	}
	adminOnly := "/authz.roles.implies.v1.TestService/AdminOnly"
	// owner does not literally hold "admin" but implication grants it.
	if err := p.Check(adminOnly, []Role{"owner"}); err != nil {
		t.Errorf("owner (implies admin) on AdminOnly: want allow, got %v", err)
	}
	// user does not imply admin → deny.
	if err := p.Check(adminOnly, []Role{"user"}); err == nil {
		t.Errorf("user on AdminOnly: want deny, got allow")
	}
}

func TestExpandImplications_Transitive(t *testing.T) {
	got := expandImplications(map[Role][]Role{
		"owner": {"admin"},
		"admin": {"user"},
	})
	// owner should transitively grant admin AND user.
	want := map[Role]bool{"admin": true, "user": true}
	if len(got["owner"]) != 2 {
		t.Fatalf("owner grants = %v, want admin+user", got["owner"])
	}
	for _, g := range got["owner"] {
		if !want[g] {
			t.Errorf("owner unexpectedly grants %q", g)
		}
	}
}

func TestExpandImplications_CycleTerminates(t *testing.T) {
	// a→b→a must fixpoint, not hang.
	got := expandImplications(map[Role][]Role{
		"a": {"b"},
		"b": {"a"},
	})
	if _, ok := got["a"]; !ok {
		t.Fatal("cycle expansion dropped role a")
	}
}

func TestPolicyFromDescriptors_ContradictionFails(t *testing.T) {
	// A method that sets both authz_public and required_roles must fail the build.
	pkgName := "authz.roles.contradiction.v1"
	mo := &forgepb.MethodOptions{AuthzPublic: true, RequiredRoles: []string{"admin"}}
	opts := &descriptorpb.MethodOptions{}
	proto.SetExtension(opts, forgepb.E_Method, mo)
	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("authz_contradiction.proto"),
		Package:     proto.String(pkgName),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("S"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name: proto.String("Bad"), InputType: proto.String("." + pkgName + ".Empty"),
				OutputType: proto.String("." + pkgName + ".Empty"), Options: opts,
			}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	files := &protoregistry.Files{}
	if err := files.RegisterFile(fd); err != nil {
		t.Fatalf("RegisterFile: %v", err)
	}
	if _, err := PolicyFromDescriptors(WithPolicyFiles(files)); err == nil {
		t.Fatal("policy build with public+roles contradiction must fail")
	}
}

func TestRoleInterceptor_NilArgsPanic(t *testing.T) {
	t.Run("nil policy", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("RoleInterceptor(nil, resolver) must panic")
			}
		}()
		RoleInterceptor(nil, RoleResolverFunc(func(context.Context, string) ([]Role, error) { return nil, nil }))
	})
	t.Run("nil resolver", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("RoleInterceptor(policy, nil) must panic")
			}
		}()
		RoleInterceptor(&RolePolicy{}, nil)
	})
}

func TestRoleInterceptor_UnaryGate(t *testing.T) {
	t.Cleanup(resetUnknownMethodWarnings)
	files := buildRolePolicyFiles(t, "authz.roles.intercept.v1", nil)
	p, err := PolicyFromDescriptors(WithPolicyFiles(files))
	if err != nil {
		t.Fatalf("PolicyFromDescriptors: %v", err)
	}
	adminOnly := "/authz.roles.intercept.v1.TestService/AdminOnly"

	// Drive the interceptor's resolve→decide seam directly (the WrapUnary
	// wrapper only adds req.Spec().Procedure plumbing, which connect's test
	// request doesn't carry).
	allowResolver := RoleResolverFunc(func(_ context.Context, _ string) ([]Role, error) {
		return []Role{"admin"}, nil
	})
	denyResolver := RoleResolverFunc(func(_ context.Context, _ string) ([]Role, error) {
		return []Role{"user"}, nil
	})
	errResolver := RoleResolverFunc(func(_ context.Context, _ string) ([]Role, error) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no token"))
	})

	if err := RoleInterceptor(p, allowResolver).(*roleInterceptor).check(context.Background(), adminOnly); err != nil {
		t.Errorf("admin resolver on AdminOnly: want allow, got %v", err)
	}
	if err := RoleInterceptor(p, denyResolver).(*roleInterceptor).check(context.Background(), adminOnly); err == nil {
		t.Errorf("user resolver on AdminOnly: want deny, got allow")
	}
	// Resolver error with a typed connect code passes through verbatim.
	err = RoleInterceptor(p, errResolver).(*roleInterceptor).check(context.Background(), adminOnly)
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("resolver Unauthenticated: want passthrough, got %v", connect.CodeOf(err))
	}
}

// files0WithImplication composes WithPolicyFiles + WithRoleImplication so the
// implication test reads cleanly.
func files0WithImplication(files *protoregistry.Files, implies map[Role][]Role) PolicyOption {
	return func(c *roleConfig) {
		WithPolicyFiles(files)(c)
		WithRoleImplication(implies)(c)
	}
}

// Pin procedurePath shape for the role path too (shared helper, but assert it
// here against a nested package so the role policy keys match connect's spec).
func TestPolicyFromDescriptors_ProcedurePathNested(t *testing.T) {
	t.Cleanup(resetUnknownMethodWarnings)
	files := buildRolePolicyFiles(t, "authz.roles.nested.deep.v1", nil)
	p, err := PolicyFromDescriptors(WithPolicyFiles(files))
	if err != nil {
		t.Fatalf("PolicyFromDescriptors: %v", err)
	}
	want := "/authz.roles.nested.deep.v1.TestService/AdminOnly"
	found := false
	for _, m := range p.Methods() {
		if m == want {
			found = true
		}
	}
	if !found {
		t.Errorf("procedure %q not in policy; got %v", want, p.Methods())
	}
}
