package depsurface

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture writes a module whose tools handler takes a 12-method foreign
// interface and calls two of its methods, one through a local.
func writeFixture(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("loads a fixture module through go/packages (a `go list` child process, several seconds); runs in task test")
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/fx\n\ngo 1.22\n",
		"internal/store/store.go": `package store

import "context"

type Daemon struct{ ID string }

// Repository is the wide repository every handler used to take.
type Repository interface {
	Get(ctx context.Context, id string) (*Daemon, error)
	Put(ctx context.Context, d *Daemon) error
	A()
	B()
	C()
	D()
	E()
	F()
	G()
	H()
	I()
	J()
}

// Impl is the one implementation.
type Impl struct{}

func (*Impl) Get(context.Context, string) (*Daemon, error) { return nil, nil }
func (*Impl) Put(context.Context, *Daemon) error            { return nil }
func (*Impl) A()                                             {}
func (*Impl) B()                                             {}
func (*Impl) C()                                             {}
func (*Impl) D()                                             {}
func (*Impl) E()                                             {}
func (*Impl) F()                                             {}
func (*Impl) G()                                             {}
func (*Impl) H()                                             {}
func (*Impl) I()                                             {}
func (*Impl) J()                                             {}
`,
		"pkg/config/config.go": "package config\n\ntype Config struct{ Port int }\n",
		"internal/handlers/tools/service.go": `package tools

import (
	"context"
	"log/slog"

	"example.com/fx/internal/store"
	"example.com/fx/pkg/config"
)

type Deps struct {
	Repo   store.Repository
	Other  store.Repository
	Logger *slog.Logger
	Config *config.Config
	Bag    any
}

type Service struct{ deps Deps }

func New(d Deps) (*Service, error) {
	if d.Repo == nil {
		return nil, nil
	}
	return &Service{deps: d}, nil
}

func (s *Service) Lookup(ctx context.Context, id string) (*store.Daemon, error) {
	return s.deps.Repo.Get(ctx, id)
}

func (s *Service) Save(ctx context.Context, d *store.Daemon) error {
	r := s.deps.Repo
	s.deps.Logger.Info("save")
	_ = s.deps.Config.Port
	return r.Put(ctx, d)
}

func (s *Service) Raw() *store.Impl {
	impl, _ := s.deps.Other.(*store.Impl)
	return impl
}
`,
	}
	for name, src := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func fieldNamed(t *testing.T, p *Package, name string) Field {
	t.Helper()
	for _, f := range p.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no Deps field %s", name)
	return Field{}
}

func TestAnalyze_UsedMethodsEscapesContainersAndDowncasts(t *testing.T) {
	dir := writeFixture(t)
	dirs, err := FindComponentDirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(dirs, ",") != "internal/handlers/tools" {
		t.Fatalf("component dirs = %v", dirs)
	}
	pkgs, err := Analyze(dir, dirs)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("%d packages", len(pkgs))
	}
	p := pkgs[0]
	repo := fieldNamed(t, p, "Repo")
	if !repo.Interface || !repo.Foreign || len(repo.Methods) != 12 || repo.Escapes {
		t.Fatalf("Repo = %+v", repo)
	}
	if !repo.Used["Get"] || !repo.Used["Put"] || len(repo.Used) != 2 {
		t.Fatalf("Repo used %v, want Get (through the field) and Put (through a local)", repo.Used)
	}
	if got := len(Unused(repo)); got != 10 {
		t.Fatalf("Repo unused = %d", got)
	}
	if c := fieldNamed(t, p, "Config").Container; c != "" {
		t.Errorf("the root config is not flagged until the scaffold gives components config blocks (K3): %q", c)
	}
	if c := fieldNamed(t, p, "Bag").Container; !strings.Contains(c, "any") {
		t.Errorf("Bag container = %q", c)
	}
	if c := fieldNamed(t, p, "Logger").Container; c != "" {
		t.Errorf("Logger is no container: %q", c)
	}
	if len(p.Downcasts) != 1 || p.Downcasts[0].Field != "Other" || p.Downcasts[0].Target != "*store.Impl" {
		t.Fatalf("downcasts = %+v", p.Downcasts)
	}
}

func TestNarrow_WritesTheConsumerInterfaceAndRetypesTheField(t *testing.T) {
	dir := writeFixture(t)
	res, err := Narrow(dir, "internal/handlers/tools")
	if err != nil {
		t.Fatal(err)
	}
	if res.File != "internal/handlers/tools/deps.go" || len(res.Narrowed) != 1 || res.Narrowed[0].To != "Repository" {
		t.Fatalf("result = %+v", res)
	}
	// Other is only downcast: left alone, with the reason.
	if len(res.Skipped) != 1 || res.Skipped[0].Field != "Other" || !strings.Contains(res.Skipped[0].Reason, "type-asserts") {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
	deps, err := os.ReadFile(filepath.Join(dir, res.File))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{DepsFileMarker, "type Repository interface {", "Get(ctx context.Context, id string) (*store.Daemon, error)", "Put(ctx context.Context, d *store.Daemon) error"} {
		if !strings.Contains(string(deps), want) {
			t.Errorf("deps.go lacks %q:\n%s", want, deps)
		}
	}
	svc, _ := os.ReadFile(filepath.Join(dir, "internal/handlers/tools/service.go"))
	if !strings.Contains(string(svc), "Repo   Repository") && !strings.Contains(string(svc), "Repo Repository") {
		t.Errorf("Deps.Repo was not retyped:\n%s", svc)
	}
	// The lint goes quiet: nothing unused through Repo any more.
	pkgs, err := Analyze(dir, []string{"internal/handlers/tools"})
	if err != nil || len(pkgs) != 1 {
		t.Fatalf("re-analyze: %v (%d packages) — the narrowed package must type-check", err, len(pkgs))
	}
	if repo := fieldNamed(t, pkgs[0], "Repo"); repo.Foreign || len(Unused(repo)) != 0 {
		t.Fatalf("after narrowing Repo = %+v", repo)
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the narrowed module does not build: %v\n%s", err, out)
	}
}

func TestNarrow_DepsDeclaredInContractGo(t *testing.T) {
	dir := writeFixture(t)
	svcPath := filepath.Join(dir, "internal/handlers/tools/service.go")
	svc, err := os.ReadFile(svcPath)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(svc), "type Deps struct {")
	end := start + strings.Index(string(svc)[start:], "}\n") + 2
	contract := "package tools\n\nimport (\n\t\"log/slog\"\n\n\t\"example.com/fx/internal/store\"\n\t\"example.com/fx/pkg/config\"\n)\n\n" + string(svc)[start:end]
	rest := strings.Replace(string(svc)[:start]+string(svc)[end:], "\t\"log/slog\"\n", "", 1)
	rest = strings.Replace(rest, "\t\"example.com/fx/pkg/config\"\n", "", 1)
	if err := os.WriteFile(filepath.Join(dir, "internal/handlers/tools/contract.go"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svcPath, []byte(rest), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Narrow(dir, "internal/handlers/tools")
	if err != nil {
		t.Fatal(err)
	}
	if res.File != "internal/handlers/tools/contract.go" || len(res.Narrowed) != 1 {
		t.Fatalf("result = %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(dir, res.File))
	if !strings.Contains(string(got), "type Repository interface {") || !strings.Contains(string(got), "Repo   Repository") {
		t.Fatalf("contract.go was not narrowed in place:\n%s", got)
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the narrowed module does not build: %v\n%s\ncontract.go:\n%s", err, out, got)
	}
}
