package cli

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// TestPipelineBootstrapTesting_AuthzAware guards the PIPELINE's testing.go
// emission — the exact code path `stepBootstrapTesting` exercises
// (generateBootstrapTesting -> codegen.GenerateBootstrapTesting, including the
// CLI-side discoverPackages / discoverWorkers / discoverOperators helpers the
// codegen-package unit tests bypass).
//
// Regression context (fr-0ec014eb92): control-plane ran a FULL `forge generate`
// and pkg/app/testing.go came back with a legacy harness that hard-wired
// `deps.Authorizer` for EVERY service — a compile error for carve-out /
// //forge:external-component / descriptor-authz services whose Deps declare no
// Authorizer field, which forced the file into .forge/disowned.json. The
// codegen-package tests (TestGenerateBootstrapTesting_AuthzAware) proved the
// EXPORTED generator was authz-aware, but only this test pins that the PIPELINE
// step feeds that generator the same on-disk-derived signal end-to-end, so a
// future divergence between the pipeline call site and the exported function is
// caught here rather than at the next control-plane regenerate.
func TestPipelineBootstrapTesting_AuthzAware(t *testing.T) {
	projectDir := t.TempDir()

	// Service WITH an Authorizer dep — the normal case.
	authedSrc := `package authed

import "log/slog"

type Authorizer interface{ Can(string) bool }

type Deps struct {
	Logger     *slog.Logger
	Authorizer Authorizer
}

type Service struct{ deps Deps }

func New(deps Deps) (*Service, error) { return &Service{deps: deps}, nil }

func (s *Service) Register(mux interface{ Handle(string, interface{}) }, opts ...interface{}) {}
`
	writeFileAt(t, projectDir, filepath.Join("internal", "handlers", "authed", "service.go"), authedSrc)

	// Service WITHOUT an Authorizer dep — carve-out / external-component /
	// descriptor-authz shape.
	carveSrc := `package carve

import "log/slog"

type Deps struct {
	Logger *slog.Logger
}

type Service struct{ deps Deps }

func New(deps Deps) (*Service, error) { return &Service{deps: deps}, nil }

func (s *Service) Register(mux interface{ Handle(string, interface{}) }, opts ...interface{}) {}
`
	writeFileAt(t, projectDir, filepath.Join("internal", "handlers", "carve", "service.go"), carveSrc)

	services := []codegen.ServiceDef{
		{Name: "AuthedService", ModulePath: "example.com/proj"},
		{Name: "CarveService", ModulePath: "example.com/proj"},
	}

	// Drive the SAME CLI-side wrapper stepBootstrapTesting calls — NOT the
	// exported codegen function directly. This exercises discoverPackages /
	// discoverWorkers / discoverOperators on the way to GenerateBootstrapTesting.
	if err := generateBootstrapTesting(services, "example.com/proj", false, projectDir, nil); err != nil {
		t.Fatalf("generateBootstrapTesting (pipeline path): %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "testing.go"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Output must parse as valid Go (the legacy harness referenced a
	// non-existent deps.Authorizer field on carved services — that compiles
	// syntactically, so parse-validity is necessary but not sufficient; the
	// body checks below are the real regression pin).
	if _, perr := parser.ParseFile(token.NewFileSet(), "testing.go", data, parser.AllErrors); perr != nil {
		t.Fatalf("pipeline-emitted testing.go does not parse: %v\n%s", perr, content)
	}

	// The carve service's factory body must NOT wire deps.Authorizer (no such
	// field on its Deps — this is the legacy-harness compile bug).
	carveDeps := sliceBetweenCLI(content, "func newTestCarveDeps(", "return deps")
	if carveDeps == "" {
		t.Fatalf("could not locate newTestCarveDeps body:\n%s", content)
	}
	if strings.Contains(carveDeps, "deps.Authorizer") {
		t.Errorf("carve service (no Authorizer dep) must NOT wire deps.Authorizer:\n%s", carveDeps)
	}

	// The test seam is still preserved: the carve server mounts the authz
	// interceptor threading the cross-cutting cfg.authz, never deps.Authorizer.
	carveServer := sliceBetweenCLI(content, "func NewTestCarveServer(", "return srv, client")
	if !strings.Contains(carveServer, "middleware.AuthzInterceptor(cfg.authz)") {
		t.Errorf("carve server must mount AuthzInterceptor(cfg.authz):\n%s", carveServer)
	}
	if strings.Contains(carveServer, "deps.Authorizer") {
		t.Errorf("carve server must NOT reference deps.Authorizer:\n%s", carveServer)
	}

	// The authed service still threads its own deps.Authorizer.
	authedDeps := sliceBetweenCLI(content, "func newTestAuthedDeps(", "return deps")
	if !strings.Contains(authedDeps, "deps.Authorizer = cfg.authz") {
		t.Errorf("authed service must wire deps.Authorizer:\n%s", authedDeps)
	}
	authedServer := sliceBetweenCLI(content, "func NewTestAuthedServer(", "return srv, client")
	if !strings.Contains(authedServer, "middleware.AuthzInterceptor(deps.Authorizer)") {
		t.Errorf("authed server must mount AuthzInterceptor(deps.Authorizer):\n%s", authedServer)
	}
}

// sliceBetweenCLI returns the substring of s from the first occurrence of start
// up to (and including) the first occurrence of end after it. Empty if either
// marker is missing.
func sliceBetweenCLI(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return ""
	}
	return s[i : i+j+len(end)]
}
