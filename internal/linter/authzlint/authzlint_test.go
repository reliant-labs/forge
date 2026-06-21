package authzlint

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	forgev1 "github.com/reliant-labs/forge/pkg/forgepb"
)

// methodSpec is a tiny declarative description of an rpc's authz annotation for
// the test descriptor builder.
type methodSpec struct {
	name     string
	roles    []string // required_roles
	public   bool     // authz_public
	annotate bool     // attach a (forge.v1.method) extension at all
}

// buildFiles compiles a one-service proto from the given method specs (and an
// optional service default_roles) into a scoped *protoregistry.Files. Building
// real descriptors exercises the same extension-read path the lint uses in
// production.
func buildFiles(t *testing.T, pkg string, serviceDefaults []string, methods ...methodSpec) *protoregistry.Files {
	t.Helper()

	var svcOpts *descriptorpb.ServiceOptions
	if len(serviceDefaults) > 0 {
		svcOpts = &descriptorpb.ServiceOptions{}
		proto.SetExtension(svcOpts, forgev1.E_Service, &forgev1.ServiceOptions{DefaultRoles: serviceDefaults})
	}

	var mds []*descriptorpb.MethodDescriptorProto
	for _, ms := range methods {
		md := &descriptorpb.MethodDescriptorProto{
			Name:       proto.String(ms.name),
			InputType:  proto.String("." + pkg + ".Empty"),
			OutputType: proto.String("." + pkg + ".Empty"),
		}
		if ms.annotate {
			opts := &descriptorpb.MethodOptions{}
			proto.SetExtension(opts, forgev1.E_Method, &forgev1.MethodOptions{
				RequiredRoles: ms.roles,
				AuthzPublic:   ms.public,
			})
			md.Options = opts
		}
		mds = append(mds, md)
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String(pkg + ".proto"),
		Package:     proto.String(pkg),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name:    proto.String("TestService"),
			Options: svcOpts,
			Method:  mds,
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
	return files
}

func TestLint_AllAnnotated_Passes(t *testing.T) {
	files := buildFiles(t, "authzlint.pass.v1", nil,
		methodSpec{name: "AdminOnly", roles: []string{"admin"}, annotate: true},
		methodSpec{name: "Health", public: true, annotate: true},
	)
	res := Lint(files)
	if res.HasErrors() {
		t.Fatalf("fully-annotated service must pass; got findings: %+v", res.Findings)
	}
}

func TestLint_OneUnannotated_FailsNamingMethod(t *testing.T) {
	files := buildFiles(t, "authzlint.fail.v1", nil,
		methodSpec{name: "AdminOnly", roles: []string{"admin"}, annotate: true},
		methodSpec{name: "Forgotten", annotate: false}, // the foot-gun
	)
	res := Lint(files)
	if !res.HasErrors() {
		t.Fatal("a service with an unannotated method must fail the lint")
	}
	var found bool
	for _, f := range res.Findings {
		if f.Rule == RuleMissingPolicy && strings.Contains(f.Message, "/authzlint.fail.v1.TestService/Forgotten") {
			found = true
		}
		if strings.Contains(f.Message, "AdminOnly") {
			t.Errorf("annotated method AdminOnly must NOT be flagged: %s", f.Message)
		}
	}
	if !found {
		t.Errorf("finding must name the unannotated method Forgotten; got %+v", res.Findings)
	}
}

func TestLint_EmptyRequiredRoles_IsUnannotated(t *testing.T) {
	// An explicit but EMPTY required_roles is not an authorization decision —
	// it must be treated as unannotated and fail, never as "any authenticated".
	files := buildFiles(t, "authzlint.empty.v1", nil,
		methodSpec{name: "Empty", roles: nil, public: false, annotate: true},
	)
	res := Lint(files)
	if !res.HasErrors() {
		t.Fatal("a method with empty required_roles and no public flag must fail")
	}
}

func TestLint_ServiceDefaultCovers(t *testing.T) {
	// Service-level default_roles authorizes an otherwise-unannotated method.
	files := buildFiles(t, "authzlint.svcdefault.v1", []string{"user"},
		methodSpec{name: "InheritsDefault", annotate: false},
		methodSpec{name: "AdminOnly", roles: []string{"admin"}, annotate: true},
	)
	res := Lint(files)
	if res.HasErrors() {
		t.Fatalf("service default_roles must cover unannotated methods; got %+v", res.Findings)
	}
}

func TestLint_Contradiction_Fails(t *testing.T) {
	files := buildFiles(t, "authzlint.contradiction.v1", nil,
		methodSpec{name: "Bad", roles: []string{"admin"}, public: true, annotate: true},
	)
	res := Lint(files)
	if !res.HasErrors() {
		t.Fatal("public+roles contradiction must fail the lint")
	}
	var found bool
	for _, f := range res.Findings {
		if f.Rule == RuleContradiction {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a %s finding; got %+v", RuleContradiction, res.Findings)
	}
}
