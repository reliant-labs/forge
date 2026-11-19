package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/connectv2test"
)

// A service may declare an RPC named like its mount helper — Register in
// connect-go v1, RegisterV2 in v2. One type cannot carry both, so the helper
// is renamed (Mount / MountV2), and every generated caller has to follow.
func TestMountMethodFor(t *testing.T) {
	plain := ServiceDef{Methods: []Method{{Name: "Ping"}}}
	register := ServiceDef{Methods: []Method{{Name: "Register"}, {Name: "Ping"}}}
	registerV2 := ServiceDef{Methods: []Method{{Name: "RegisterV2"}, {Name: "Ping"}}}
	for _, tc := range []struct {
		name string
		svc  ServiceDef
		v2   bool
		want string
	}{
		{"v1 plain", plain, false, "Register"},
		{"v1 Register rpc", register, false, "Mount"},
		{"v1 RegisterV2 rpc does not collide", registerV2, false, "Register"},
		{"v2 plain", plain, true, "RegisterV2"},
		{"v2 Register rpc does not collide", register, true, "RegisterV2"},
		{"v2 RegisterV2 rpc", registerV2, true, "MountV2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mountMethodFor(tc.svc, tc.v2); got != tc.want {
				t.Errorf("mountMethodFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// registerV2Service is the v2 ItemsService fixture declaring an RPC named
// RegisterV2 — the collision with the v2 mount helper — beside a Ping.
func registerV2Service() ServiceDef {
	svc := v2ItemsService()
	svc.Methods = []Method{
		{Name: "RegisterV2", InputType: "RegisterV2Request", OutputType: "RegisterV2Response", AuthRequired: true},
		{Name: "Ping", InputType: "PingRequest", OutputType: "PingResponse", AuthRequired: true},
	}
	return svc
}

const registerV2Proto = `syntax = "proto3";
package items.v1;
option go_package = "example.com/v2app/gen/services/items/v1;itemsv1";
message RegisterV2Request {}
message RegisterV2Response {}
message PingRequest {}
message PingResponse {}
service ItemsService {
  rpc RegisterV2(RegisterV2Request) returns (RegisterV2Response);
  rpc Ping(PingRequest) returns (PingResponse);
}
`

const (
	v2HelperRegister = "func (s *Service) RegisterV2(server *connect.Server)"
	v2HelperMount    = "func (s *Service) MountV2(server *connect.Server)"
)

// A fresh v2 service with a RegisterV2 RPC: service.go's helper is MountV2,
// the RPC gets its stub, and the mock picks the same helper name.
func TestServiceStubConnectV2RegisterV2RPC(t *testing.T) {
	project := t.TempDir()
	writeConnectV2GoMod(t, project)
	handlerDir := filepath.Join(project, "internal", "handlers", "items")
	if err := GenerateServiceStub(registerV2Service(), handlerDir); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}
	service := readFile(t, filepath.Join(handlerDir, "service.go"))
	if !strings.Contains(service, v2HelperMount) || strings.Contains(service, v2HelperRegister) {
		t.Errorf("service.go must declare the MountV2 helper, not RegisterV2:\n%s", service)
	}
	stub := readFile(t, filepath.Join(handlerDir, RPCHandlerFileName("RegisterV2")))
	if !strings.Contains(stub, "func (s *Service) RegisterV2(") {
		t.Errorf("%s does not stub the RegisterV2 rpc:\n%s", RPCHandlerFileName("RegisterV2"), stub)
	}

	mocks := filepath.Join(project, "internal", "handlers", "mocks")
	if _, err := GenerateMock(registerV2Service(), project, mocks, nil); err != nil {
		t.Fatalf("GenerateMock: %v", err)
	}
	entries, err := os.ReadDir(mocks)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one mock file, got %v (%v)", entries, err)
	}
	mock := readFile(t, filepath.Join(mocks, entries[0].Name()))
	if !strings.Contains(mock, ") MountV2(server *connect.Server)") {
		t.Errorf("the mock's helper must be MountV2, as the service's is:\n%s", mock)
	}

	// The component test helper mounts through the same name.
	data := goldenHelperData(true)
	data.MountMethod = mountMethodFor(registerV2Service(), true)
	helpers := string(renderProjectTemplate(t, "component_test_helpers.go.tmpl", data))
	if !strings.Contains(helpers, "testkit.NewConnectClientV2(t, svc.MountV2,") {
		t.Errorf("the test helper must mount through MountV2:\n%s", helpers)
	}
}

// The usual order: the service is scaffolded with no RPCs (helper
// RegisterV2), and a RegisterV2 RPC is added to the proto later. The stub
// pass must rename the owned helper to MountV2 — not count it as the RPC's
// implementation — and stub the RPC; removing the RPC renames it back.
func TestGenerateMissingHandlerStubs_ConnectV2RegisterV2RPCAddedLater(t *testing.T) {
	project := t.TempDir()
	writeConnectV2GoMod(t, project)
	handlerDir := filepath.Join(project, "internal", "handlers", "items")
	pingOnly := registerV2Service()
	pingOnly.Methods = pingOnly.Methods[1:]
	if err := GenerateServiceStub(pingOnly, handlerDir); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}
	servicePath := filepath.Join(handlerDir, "service.go")
	if !strings.Contains(readFile(t, servicePath), v2HelperRegister) {
		t.Fatalf("precondition: service.go is born with the RegisterV2 helper")
	}
	// An author's own edit elsewhere in the file must survive the rename.
	if err := os.WriteFile(servicePath, []byte(readFile(t, servicePath)+"\n// kept: the author's note\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := GenerateMissingHandlerStubs(registerV2Service(), project, handlerDir, nil, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs: %v", err)
	}
	if strings.Join(res.NewMethods, ",") != "RegisterV2" {
		t.Errorf("NewMethods = %v, want [RegisterV2]: the helper was counted as the RPC", res.NewMethods)
	}
	service := readFile(t, servicePath)
	if !strings.Contains(service, "// MountV2 registers the service's Connect handlers") ||
		!strings.Contains(service, v2HelperMount) || strings.Contains(service, v2HelperRegister) {
		t.Errorf("service.go's helper (and its doc comment) must be renamed to MountV2:\n%s", service)
	}
	if !strings.Contains(service, "// kept: the author's note") {
		t.Errorf("the rename rewrote more than the helper's name:\n%s", service)
	}
	if !strings.Contains(readFile(t, filepath.Join(handlerDir, RPCHandlerFileName("RegisterV2"))), "func (s *Service) RegisterV2(") {
		t.Errorf("the RegisterV2 rpc was not stubbed")
	}

	// Idempotent: a second pass changes nothing.
	if _, err := GenerateMissingHandlerStubs(registerV2Service(), project, handlerDir, nil, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs (again): %v", err)
	}
	if again := readFile(t, servicePath); again != service {
		t.Errorf("a second pass rewrote service.go:\n%s", again)
	}

	// The RPC is removed (proto and handler file): the helper is RegisterV2 again.
	if err := os.Remove(filepath.Join(handlerDir, RPCHandlerFileName("RegisterV2"))); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateMissingHandlerStubs(pingOnly, project, handlerDir, nil, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs (rpc removed): %v", err)
	}
	if back := readFile(t, servicePath); !strings.Contains(back, v2HelperRegister) || strings.Contains(back, v2HelperMount) {
		t.Errorf("with the RPC gone the helper must be RegisterV2 again:\n%s", back)
	}
}

// connect-go v1 is untouched: the stub pass never renames an owned
// service.go's Register helper.
func TestGenerateMissingHandlerStubs_ConnectV1HelperNotRenamed(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module "+v2ItemsModule+"\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handlerDir := filepath.Join(project, "internal", "handlers", "items")
	svc := registerV2Service()
	svc.Methods = svc.Methods[1:]
	if err := GenerateServiceStub(svc, handlerDir); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}
	before := readFile(t, filepath.Join(handlerDir, "service.go"))
	svc.Methods = append(svc.Methods, Method{Name: "Register", InputType: "RegisterRequest", OutputType: "RegisterResponse"})
	if _, err := GenerateMissingHandlerStubs(svc, project, handlerDir, nil, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs: %v", err)
	}
	if after := readFile(t, filepath.Join(handlerDir, "service.go")); after != before {
		t.Errorf("a v1 service.go was rewritten:\n%s", after)
	}
}

// The proof: against real connect-go v2 stubs, a service declaring a
// RegisterV2 RPC builds — fresh, and when the RPC arrives after the service
// was scaffolded — with the service and its mock both satisfying the
// generated Handler and both mounting through MountV2 on a mountkit.MuxV2.
func TestServiceConnectV2RegisterV2RPCCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("runs buf and compiles a module against real connect-go v2 stubs; runs in task test")
	}
	mod := connectv2test.NewModule(t, v2ItemsModule, map[string]string{"services/items/v1/items.proto": registerV2Proto})

	// Fresh: the RPC exists when the service is scaffolded.
	if err := GenerateServiceStub(registerV2Service(), filepath.Join(mod.Dir, "internal", "handlers", "items")); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}
	// Later: scaffolded with Ping only, then the stub pass sees RegisterV2.
	laterDir := filepath.Join(mod.Dir, "internal", "handlers", "later")
	pingOnly := registerV2Service()
	pingOnly.Methods = pingOnly.Methods[1:]
	if err := GenerateServiceStub(pingOnly, laterDir); err != nil {
		t.Fatalf("GenerateServiceStub (later): %v", err)
	}
	if _, err := GenerateMissingHandlerStubs(registerV2Service(), mod.Dir, laterDir, nil, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs: %v", err)
	}
	if _, err := GenerateMock(registerV2Service(), mod.Dir, filepath.Join(mod.Dir, "internal", "handlers", "mocks"), nil); err != nil {
		t.Fatalf("GenerateMock: %v", err)
	}
	mod.Write(t, "internal/check/check.go", `// Package check pins the rendered types to what the generated mounts call.
package check

import (
	"github.com/reliant-labs/forge/pkg/mountkit"

	"example.com/v2app/gen/services/items/v1/itemsv1connect"
	"example.com/v2app/internal/handlers/items"
	later "example.com/v2app/internal/handlers/later"
	"example.com/v2app/internal/handlers/mocks"
)

var (
	_ itemsv1connect.ItemsServiceHandler = (*items.Service)(nil)
	_ itemsv1connect.ItemsServiceHandler = (*later.Service)(nil)
	_ itemsv1connect.ItemsServiceHandler = (*mocks.ItemsServiceMock)(nil)
)

// Mount is what mounts_services_gen.go and routes_gen.go emit for a service
// whose helper is MountV2.
func Mount(m *mountkit.MuxV2, a *items.Service, b *later.Service, c *mocks.ItemsServiceMock) {
	m.Register(a.MountV2)
	m.Register(b.MountV2)
	m.Register(c.MountV2)
}
`)
	mod.Go(t, "build", "./internal/...")
	mod.Go(t, "vet", "./internal/check")
}
