package codegen

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// A service may declare an RPC named like its mount helper, Register. One
// type cannot carry both, so the helper is renamed Mount, and every
// generated caller has to follow.
func TestMountMethodFor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		methods []string
		want    string
	}{
		{"no rpcs", nil, "Register"},
		{"plain", []string{"Ping"}, "Register"},
		{"Register rpc", []string{"Register", "Ping"}, "Mount"},
		{"snake-case register rpc", []string{"register"}, "Mount"},
		{"RegisterUser does not collide", []string{"RegisterUser"}, "Register"},
		{"Mount rpc does not collide", []string{"Mount"}, "Register"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var svc ServiceDef
			for _, m := range tc.methods {
				svc.Methods = append(svc.Methods, Method{Name: m})
			}
			if got := mountMethodFor(svc); got != tc.want {
				t.Errorf("mountMethodFor(%v) = %q, want %q", tc.methods, got, tc.want)
			}
		})
	}
}

const registerItemsModule = "example.com/registerapp"

// registerItemsService is an ItemsService declaring an RPC named Register —
// the collision with the mount helper — beside a Ping.
func registerItemsService() ServiceDef {
	return ServiceDef{
		Name:       "ItemsService",
		Package:    "items.v1",
		GoPackage:  registerItemsModule + "/gen/services/items/v1",
		PkgName:    "itemsv1",
		ProtoFile:  "proto/services/items/v1/items.proto",
		ModulePath: registerItemsModule,
		Methods: []Method{
			{Name: "Register", InputType: "RegisterRequest", OutputType: "RegisterResponse", AuthRequired: true},
			{Name: "Ping", InputType: "PingRequest", OutputType: "PingResponse", AuthRequired: true},
		},
	}
}

// pingOnlyItemsService is registerItemsService before the Register RPC.
func pingOnlyItemsService() ServiceDef {
	svc := registerItemsService()
	svc.Methods = svc.Methods[1:]
	return svc
}

const registerItemsProto = `syntax = "proto3";
package items.v1;
option go_package = "example.com/registerapp/gen/services/items/v1;itemsv1";
message RegisterRequest {}
message RegisterResponse {}
message PingRequest {}
message PingResponse {}
service ItemsService {
  rpc Register(RegisterRequest) returns (RegisterResponse);
  rpc Ping(PingRequest) returns (PingResponse);
}
`

const (
	helperRegister    = "func (s *Service) Register(mux *http.ServeMux, opts ...connect.HandlerOption)"
	helperMount       = "func (s *Service) Mount(mux *http.ServeMux, opts ...connect.HandlerOption)"
	helperRegisterDoc = "// Register registers the service handlers with the HTTP mux.\n"
	helperMountDoc    = "// Mount registers the service handlers with the HTTP mux.\n"
)

// newRegisterItemsProject is an empty project directory with a go.mod, and
// the handler directory a service named ItemsService scaffolds into.
func newRegisterItemsProject(t *testing.T, leaf string) (project, handlerDir string) {
	t.Helper()
	project = t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module "+registerItemsModule+"\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return project, filepath.Join(project, "internal", "handlers", leaf)
}

// A service with no Register RPC renders the helper exactly as it always
// has; one with a Register RPC differs from it in the helper's name and its
// doc comment's first word, and nowhere else — so the fresh scaffold and the
// in-place rename (reconcileMountHelper) converge on the same file.
func TestServiceTemplateMountHelperName(t *testing.T) {
	_, plainDir := newRegisterItemsProject(t, "items")
	if err := GenerateServiceStub(pingOnlyItemsService(), plainDir); err != nil {
		t.Fatalf("GenerateServiceStub (plain): %v", err)
	}
	plain := readFile(t, filepath.Join(plainDir, "service.go"))
	if !strings.Contains(plain, "}\n\n"+helperRegisterDoc+helperRegister+" {\n\tpath, handler := ") {
		t.Errorf("a service without a Register rpc must keep the Register helper byte for byte:\n%s", plain)
	}

	_, registerDir := newRegisterItemsProject(t, "items")
	if err := GenerateServiceStub(registerItemsService(), registerDir); err != nil {
		t.Fatalf("GenerateServiceStub (Register rpc): %v", err)
	}
	withRPC := readFile(t, filepath.Join(registerDir, "service.go"))
	want := strings.Replace(strings.Replace(plain, helperRegisterDoc, helperMountDoc, 1), helperRegister, helperMount, 1)
	if withRPC != want {
		t.Errorf("service.go with a Register rpc must differ only in the helper's name:\n%s", withRPC)
	}
}

// A fresh service with a Register RPC: service.go's helper is Mount, the RPC
// gets its stub, and the mock picks the same helper name.
func TestServiceStubRegisterRPC(t *testing.T) {
	project, handlerDir := newRegisterItemsProject(t, "items")
	if err := GenerateServiceStub(registerItemsService(), handlerDir); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}
	service := readFile(t, filepath.Join(handlerDir, "service.go"))
	if !strings.Contains(service, helperMount) || strings.Contains(service, helperRegister) {
		t.Errorf("service.go must declare the Mount helper, not Register:\n%s", service)
	}
	stub := readFile(t, filepath.Join(handlerDir, RPCHandlerFileName("Register")))
	if !strings.Contains(stub, "func (s *Service) Register(") {
		t.Errorf("%s does not stub the Register rpc:\n%s", RPCHandlerFileName("Register"), stub)
	}

	mocks := filepath.Join(project, "internal", "handlers", "mocks")
	if _, err := GenerateMock(registerItemsService(), project, mocks, nil); err != nil {
		t.Fatalf("GenerateMock: %v", err)
	}
	mock := readFile(t, filepath.Join(project, MockRelPath(registerItemsService())))
	if !strings.Contains(mock, ") Mount(mux *http.ServeMux, opts ...connect.HandlerOption)") {
		t.Errorf("the mock's helper must be Mount, as the service's is:\n%s", mock)
	}
}

// The usual order: the service is scaffolded with no Register RPC (helper
// Register), and the RPC is added to the proto later. The stub pass must
// rename the owned helper to Mount — not count it as the RPC's
// implementation — and stub the RPC; removing the RPC renames it back.
func TestGenerateMissingHandlerStubs_RegisterRPCAddedLater(t *testing.T) {
	project, handlerDir := newRegisterItemsProject(t, "items")
	if err := GenerateServiceStub(pingOnlyItemsService(), handlerDir); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}
	servicePath := filepath.Join(handlerDir, "service.go")
	born := readFile(t, servicePath)
	if !strings.Contains(born, helperRegister) {
		t.Fatalf("precondition: service.go is born with the Register helper:\n%s", born)
	}
	// An author's own edit elsewhere in the file must survive the rename.
	authored := born + "\n// kept: the author's note\n"
	if err := os.WriteFile(servicePath, []byte(authored), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := GenerateMissingHandlerStubs(registerItemsService(), project, handlerDir, nil, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs: %v", err)
	}
	if strings.Join(res.NewMethods, ",") != "Register" {
		t.Errorf("NewMethods = %v, want [Register]: the helper was counted as the RPC", res.NewMethods)
	}
	service := readFile(t, servicePath)
	want := strings.Replace(strings.Replace(authored, helperRegisterDoc, helperMountDoc, 1), helperRegister, helperMount, 1)
	if service != want {
		t.Errorf("only the helper's name and its doc comment's first word may change:\n%s", service)
	}
	if !strings.Contains(readFile(t, filepath.Join(handlerDir, RPCHandlerFileName("Register"))), "func (s *Service) Register(") {
		t.Errorf("the Register rpc was not stubbed")
	}

	// Idempotent: a second pass changes nothing.
	if _, err := GenerateMissingHandlerStubs(registerItemsService(), project, handlerDir, nil, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs (again): %v", err)
	}
	if again := readFile(t, servicePath); again != service {
		t.Errorf("a second pass rewrote service.go:\n%s", again)
	}

	// While the RPC's implementation is still on disk the helper stays
	// Mount: renaming it back would declare Register twice.
	if _, err := GenerateMissingHandlerStubs(pingOnlyItemsService(), project, handlerDir, nil, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs (rpc dropped from proto): %v", err)
	}
	if kept := readFile(t, servicePath); kept != service {
		t.Errorf("the helper was renamed back beside a Register method:\n%s", kept)
	}

	// The RPC is removed (proto and handler file): the file is as it was.
	if err := os.Remove(filepath.Join(handlerDir, RPCHandlerFileName("Register"))); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateMissingHandlerStubs(pingOnlyItemsService(), project, handlerDir, nil, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs (rpc removed): %v", err)
	}
	if back := readFile(t, servicePath); back != authored {
		t.Errorf("with the RPC gone the helper must be Register again:\n%s", back)
	}
}

// A disowned service.go is the author's alone: the helper is not renamed.
func TestGenerateMissingHandlerStubs_DisownedServiceKeepsHelper(t *testing.T) {
	project, handlerDir := newRegisterItemsProject(t, "items")
	if err := GenerateServiceStub(pingOnlyItemsService(), handlerDir); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}
	servicePath := filepath.Join(handlerDir, "service.go")
	before := readFile(t, servicePath)
	cs := &checksums.FileChecksums{Disowned: map[string]checksums.DisownedEntry{"internal/handlers/items/service.go": {}}}
	if _, err := GenerateMissingHandlerStubs(registerItemsService(), project, handlerDir, nil, cs); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs: %v", err)
	}
	if after := readFile(t, servicePath); after != before {
		t.Errorf("a disowned service.go was rewritten:\n%s", after)
	}
}

// The proof: against real protoc-gen-connect-go stubs, a service declaring
// a Register RPC builds — fresh, and when the RPC arrives after the service
// was scaffolded — with the service and its mock both satisfying the
// generated Handler and both mounting through Mount, as the generated
// mounts and route sets call it.
func TestServiceRegisterRPCCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("builds codegen plugins, runs buf and compiles a module against real connect-go stubs; runs in task test")
	}
	mod := newConnectModule(t, registerItemsModule, map[string]string{"services/items/v1/items.proto": registerItemsProto})

	// Fresh: the RPC exists when the service is scaffolded.
	if err := GenerateServiceStub(registerItemsService(), filepath.Join(mod, "internal", "handlers", "items")); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}
	// Later: scaffolded with Ping only, then the stub pass sees Register.
	laterDir := filepath.Join(mod, "internal", "handlers", "later")
	if err := GenerateServiceStub(pingOnlyItemsService(), laterDir); err != nil {
		t.Fatalf("GenerateServiceStub (later): %v", err)
	}
	if _, err := GenerateMissingHandlerStubs(registerItemsService(), mod, laterDir, nil, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs: %v", err)
	}
	if _, err := GenerateMock(registerItemsService(), mod, filepath.Join(mod, "internal", "handlers", "mocks"), nil); err != nil {
		t.Fatalf("GenerateMock: %v", err)
	}
	writeModuleFile(t, mod, "internal/check/check.go", `// Package check pins the rendered types to what the generated mounts call.
package check

import (
	"net/http"

	"connectrpc.com/connect"

	"example.com/registerapp/gen/services/items/v1/itemsv1connect"
	"example.com/registerapp/internal/handlers/items"
	later "example.com/registerapp/internal/handlers/later"
	"example.com/registerapp/internal/handlers/mocks"
)

var (
	_ itemsv1connect.ItemsServiceHandler = (*items.Service)(nil)
	_ itemsv1connect.ItemsServiceHandler = (*later.Service)(nil)
	_ itemsv1connect.ItemsServiceHandler = (*mocks.ItemsServiceMock)(nil)
)

// Mount is what mounts_services_gen.go and routes_gen.go emit for a service
// whose helper is Mount.
func Mount(mux *http.ServeMux, opts []connect.HandlerOption, a *items.Service, b *later.Service, c *mocks.ItemsServiceMock) {
	a.Mount(mux, opts...)
	b.Mount(mux, opts...)
	c.Mount(mux, opts...)
}
`)
	runIn(t, mod, "go", "build", "./internal/...")
	runIn(t, mod, "go", "vet", "./internal/check")
}

var (
	connectPluginsOnce sync.Once
	connectPluginsDir  string
	connectPluginsErr  string
)

// connectPlugins builds protoc-gen-go and protoc-gen-connect-go from forge's
// own module graph, once per test process, so the stubs match the runtime
// versions forge's go.mod pins.
func connectPlugins(t *testing.T, forgeRoot string) string {
	t.Helper()
	connectPluginsOnce.Do(func() {
		dir, err := os.MkdirTemp("", "forge-connect-plugins-")
		if err != nil {
			connectPluginsErr = err.Error()
			return
		}
		for name, pkg := range map[string]string{
			"protoc-gen-go":         "google.golang.org/protobuf/cmd/protoc-gen-go",
			"protoc-gen-connect-go": "connectrpc.com/connect/cmd/protoc-gen-connect-go",
		} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name), pkg)
			cmd.Dir = forgeRoot
			if out, err := cmd.CombinedOutput(); err != nil {
				connectPluginsErr = "build " + pkg + ": " + err.Error() + "\n" + string(out)
				return
			}
		}
		connectPluginsDir = dir
	})
	if connectPluginsErr != "" {
		t.Fatalf("%s", connectPluginsErr)
	}
	return connectPluginsDir
}

// newConnectModule creates a module at t.TempDir() that requires what forge's
// go.mod requires (so forge's go.sum covers it) and replaces forge with this
// checkout, then generates Go and connect-go stubs for protoFiles (path
// under proto/ → source) into gen/ with paths=source_relative. It skips when
// buf is not installed.
func newConnectModule(t *testing.T, modulePath string, protoFiles map[string]string) string {
	t.Helper()
	buf, err := exec.LookPath("buf")
	if err != nil {
		t.Skip("buf is not installed; it generates the connect-go stubs this check compiles against")
	}
	forgeRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := connectPlugins(t, forgeRoot)
	dir := t.TempDir()

	goMod, err := os.ReadFile(filepath.Join(forgeRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goSum, err := os.ReadFile(filepath.Join(forgeRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("module " + modulePath + "\n\n")
	inRequire := false
	for _, line := range strings.Split(string(goMod), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "go "), strings.HasPrefix(trimmed, "toolchain "):
			b.WriteString(line + "\n")
		case strings.HasPrefix(trimmed, "require ("):
			inRequire = true
			b.WriteString(line + "\n")
		case inRequire:
			b.WriteString(line + "\n")
			inRequire = trimmed != ")"
		case strings.HasPrefix(trimmed, "require "):
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\nrequire github.com/reliant-labs/forge v0.0.0\n\nreplace github.com/reliant-labs/forge => " + filepath.ToSlash(forgeRoot) + "\n")
	writeModuleFile(t, dir, "go.mod", b.String())
	writeModuleFile(t, dir, "go.sum", string(goSum))

	for rel, src := range protoFiles {
		writeModuleFile(t, dir, filepath.Join("proto", rel), src)
	}
	writeModuleFile(t, dir, "buf.yaml", "version: v2\nmodules:\n  - path: proto\n")
	writeModuleFile(t, dir, "buf.gen.yaml", "version: v2\nplugins:\n"+
		"  - local: "+filepath.Join(bin, "protoc-gen-go")+"\n    out: gen\n    opt: paths=source_relative\n"+
		"  - local: "+filepath.Join(bin, "protoc-gen-connect-go")+"\n    out: gen\n    opt: paths=source_relative\n")
	runIn(t, dir, buf, "generate")
	return dir
}

func writeModuleFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runIn runs name in dir and fails the test on a non-zero exit, printing its
// output. -mod=mod lets the build list settle on what the module's packages
// import; GOPROXY=off keeps it to the local cache.
func runIn(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out.String())
	}
}
