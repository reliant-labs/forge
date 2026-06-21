package authz

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/reliant-labs/forge/pkg/auth"
	"github.com/reliant-labs/forge/pkg/forgepb"
)

// buildTestFiles compiles a single synthetic proto file with one service whose
// methods carry varying (forge.v1.method) auth_required annotations, and
// returns it as a scoped *protoregistry.Files. Building real descriptors (vs.
// hand-stuffing maps) exercises the actual descriptor-walk + extension-read
// path FromDescriptors relies on, and lets us assert procedure-path
// construction against connect's format.
//
// pkgName lets a test vary the proto package (e.g. nested "a.b.c.v1") so we can
// pin the "/pkg.Service/Method" shape for nested packages too.
func buildTestFiles(t *testing.T, pkgName string) *protoregistry.Files {
	t.Helper()

	tru := true
	fal := false

	// authRequired=true method.
	optTrue := &descriptorpb.MethodOptions{}
	proto.SetExtension(optTrue, forgepb.E_Method, &forgepb.MethodOptions{AuthRequired: &tru})
	// authRequired=false method.
	optFalse := &descriptorpb.MethodOptions{}
	proto.SetExtension(optFalse, forgepb.E_Method, &forgepb.MethodOptions{AuthRequired: &fal})
	// method with the extension present but auth_required unset.
	optUnset := &descriptorpb.MethodOptions{}
	proto.SetExtension(optUnset, forgepb.E_Method, &forgepb.MethodOptions{})

	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("authz_testsvc.proto"),
		Package: proto.String(pkgName),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Empty")},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: proto.String("TestService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       proto.String("Secured"),
						InputType:  proto.String("." + pkgName + ".Empty"),
						OutputType: proto.String("." + pkgName + ".Empty"),
						Options:    optTrue,
					},
					{
						Name:       proto.String("Public"),
						InputType:  proto.String("." + pkgName + ".Empty"),
						OutputType: proto.String("." + pkgName + ".Empty"),
						Options:    optFalse,
					},
					{
						Name:       proto.String("Unannotated"),
						InputType:  proto.String("." + pkgName + ".Empty"),
						OutputType: proto.String("." + pkgName + ".Empty"),
						// no Options at all
					},
					{
						Name:       proto.String("OptNoAuth"),
						InputType:  proto.String("." + pkgName + ".Empty"),
						OutputType: proto.String("." + pkgName + ".Empty"),
						Options:    optUnset, // extension present, auth_required unset
					},
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

// authedCtx / anonCtx model the request context the wired claims-lookup reads.
func authedCtx() context.Context {
	return putClaims(context.Background(), &auth.Claims{UserID: "u1", Role: "member"})
}
func anonCtx() context.Context { return context.Background() }

func TestFromDescriptors_ProcedurePathMatchesConnectSpec(t *testing.T) {
	// Pin the exact "/<pkg>.<Service>/<Method>" shape FromDescriptors emits,
	// including a nested package, against connect's procedure construction.
	files := buildTestFiles(t, "authz.nested.v1")

	var got map[string]bool
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		got = map[string]bool{}
		svc := fd.Services().Get(0)
		for i := 0; i < svc.Methods().Len(); i++ {
			m := svc.Methods().Get(i)
			got[procedurePath(svc, m)] = true
		}
		return true
	})

	// connect-go builds a handler/client procedure as
	// "/" + fullyQualifiedServiceName + "/" + methodName. Pin every method
	// against that shape, nested package included.
	wantProcs := []string{
		"/authz.nested.v1.TestService/Secured",
		"/authz.nested.v1.TestService/Public",
		"/authz.nested.v1.TestService/Unannotated",
		"/authz.nested.v1.TestService/OptNoAuth",
	}
	for _, want := range wantProcs {
		if !got[want] {
			t.Errorf("procedurePath: %q not produced; got %v", want, got)
		}
	}
}

func TestFromDescriptors_AuthRequiredEnforcement(t *testing.T) {
	wireLookup(t, lookupClaims)
	t.Cleanup(resetUnknownMethodWarnings)

	files := buildTestFiles(t, "authz.enforce.v1")
	a, err := FromDescriptors(WithFiles(files))
	if err != nil {
		t.Fatalf("FromDescriptors: %v", err)
	}

	const (
		secured     = "/authz.enforce.v1.TestService/Secured"
		public      = "/authz.enforce.v1.TestService/Public"
		unannotated = "/authz.enforce.v1.TestService/Unannotated"
		optNoAuth   = "/authz.enforce.v1.TestService/OptNoAuth"
	)

	// auth_required=true: denies anonymous, allows authenticated.
	if err := a.CanAccess(anonCtx(), secured); err == nil {
		t.Errorf("%s anonymous: want deny, got allow", secured)
	} else if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("%s anonymous: want Unauthenticated, got %v", secured, connect.CodeOf(err))
	}
	if err := a.CanAccess(authedCtx(), secured); err != nil {
		t.Errorf("%s authenticated: want allow, got %v", secured, err)
	}

	// auth_required=false: allows anonymous.
	if err := a.CanAccess(anonCtx(), public); err != nil {
		t.Errorf("%s anonymous: want allow, got %v", public, err)
	}

	// No explicit auth_required (no options, and options-without-auth_required):
	// not in the table → fail-closed deny by default, for both anon and authed.
	for _, proc := range []string{unannotated, optNoAuth} {
		if err := a.CanAccess(anonCtx(), proc); err == nil {
			t.Errorf("%s anonymous (fail-closed): want deny, got allow", proc)
		}
		if err := a.CanAccess(authedCtx(), proc); err == nil {
			t.Errorf("%s authenticated (fail-closed): want deny, got allow", proc)
		}
	}
}

func TestFromDescriptors_AllowUnknownMethods(t *testing.T) {
	wireLookup(t, lookupClaims)
	t.Cleanup(resetUnknownMethodWarnings)

	files := buildTestFiles(t, "authz.unknown.v1")
	a, err := FromDescriptors(WithFiles(files), WithFailMode(AllowUnknownMethods))
	if err != nil {
		t.Fatalf("FromDescriptors: %v", err)
	}

	unannotated := "/authz.unknown.v1.TestService/Unannotated"
	// Under AllowUnknownMethods the unannotated method is served.
	if err := a.CanAccess(authedCtx(), unannotated); err != nil {
		t.Errorf("%s under AllowUnknownMethods: want allow, got %v", unannotated, err)
	}
	if err := a.CanAccess(anonCtx(), unannotated); err != nil {
		t.Errorf("%s under AllowUnknownMethods (anon): want allow, got %v", unannotated, err)
	}

	// A procedure that was never scanned at all is also unknown → allowed.
	if err := a.CanAccess(authedCtx(), "/authz.unknown.v1.TestService/NotEvenRegistered"); err != nil {
		t.Errorf("never-registered proc under AllowUnknownMethods: want allow, got %v", err)
	}
}

func TestFromDescriptors_RoleOverlay(t *testing.T) {
	wireLookup(t, lookupClaims)
	t.Cleanup(resetUnknownMethodWarnings)

	files := buildTestFiles(t, "authz.roles.v1")
	secured := "/authz.roles.v1.TestService/Secured"

	// Overlay requires the "admin" role on the secured method.
	a, err := FromDescriptors(files0WithOverlay(files, map[string][]string{
		secured: {"admin"},
	}))
	if err != nil {
		t.Fatalf("FromDescriptors: %v", err)
	}

	// member is authenticated but lacks admin → deny.
	memberCtx := putClaims(context.Background(), &auth.Claims{UserID: "u1", Role: "member"})
	if err := a.CanAccess(memberCtx, secured); err == nil {
		t.Errorf("%s member: want deny (needs admin), got allow", secured)
	}
	// admin → allow.
	adminCtx := putClaims(context.Background(), &auth.Claims{UserID: "u2", Role: "admin"})
	if err := a.CanAccess(adminCtx, secured); err != nil {
		t.Errorf("%s admin: want allow, got %v", secured, err)
	}
}

func TestFromDescriptors_EmptyOverlayAnyAuthenticated(t *testing.T) {
	wireLookup(t, lookupClaims)
	t.Cleanup(resetUnknownMethodWarnings)

	files := buildTestFiles(t, "authz.anyauth.v1")
	a, err := FromDescriptors(WithFiles(files)) // no overlay
	if err != nil {
		t.Fatalf("FromDescriptors: %v", err)
	}
	secured := "/authz.anyauth.v1.TestService/Secured"
	// Any authenticated user (no role overlay) is allowed.
	if err := a.CanAccess(authedCtx(), secured); err != nil {
		t.Errorf("%s any-authenticated: want allow, got %v", secured, err)
	}
}

// files0WithOverlay is a tiny helper so the role-overlay test reads cleanly.
func files0WithOverlay(files *protoregistry.Files, overlay map[string][]string) Option {
	// Returns a composite via a closure that applies both options.
	return func(c *config) {
		WithFiles(files)(c)
		WithRoleOverlay(overlay)(c)
	}
}
