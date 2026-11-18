package derivecontract

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/contractsrc"
	"github.com/reliant-labs/forge/internal/depsurface"
	"github.com/reliant-labs/forge/internal/generator/contract"
)

// writePkg writes files into root/internal/billing and returns root and the
// package directory.
func writePkg(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "billing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func migrate(t *testing.T, root, dir string) *Migration {
	t.Helper()
	m, err := Plan(root, dir)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := Apply(m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return m
}

const happyContract = `// Package billing charges customers.
//
//forge:outbound-io: calls the payment provider
package billing

import (
	"context"
	"time"
)

// Service charges customers.
type Service interface {
	// Charge bills a customer.
	Charge(ctx context.Context, id string, cents int64) error
	// Refund reverses a charge.
	//
	// forge:no-observe
	Refund(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (string, error) // Status reports a charge's state.
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}
`

const happyService = `package billing

import "context"

// Deps are the package's collaborators.
type Deps struct {
	Clock Clock
}

type service struct {
	deps Deps
}

// New builds the service.
func New(d Deps) (Service, error) { return &service{deps: d}, nil }

func (s *service) Charge(ctx context.Context, customer string, amount int64) error { return nil }

// Refund undoes a charge.
func (s *service) Refund(ctx context.Context, id string) error { return nil }

func (s service) Status(_ context.Context, id string) (string, error) { return "", nil }

func (s *service) helper() {}
`

// The single-implementation package with only dependency interfaces left in
// contract.go: the type is marked and takes the interface's docs, the
// interfaces move to a forge:deps deps.go, contract.go goes, and its package
// doc and directive survive. The contract derived from the result is the
// Service it replaced.
func TestPlan_MovesDepsAndRemovesContract(t *testing.T) {
	root, dir := writePkg(t, map[string]string{"contract.go": happyContract, "service.go": happyService})
	m := migrate(t, root, dir)

	if m.Type != "service" || m.TypeFile != "service.go" || m.KeptContract || m.DepsFile != "deps.go" {
		t.Errorf("migration = %+v", m)
	}
	if _, err := os.Stat(filepath.Join(dir, "contract.go")); !os.IsNotExist(err) {
		t.Errorf("contract.go should be removed, stat err = %v", err)
	}
	deps := read(t, filepath.Join(dir, "deps.go"))
	if !strings.HasPrefix(deps, depsurface.DepsFileMarker+"\n") || !depsurface.HasDepsMarker(dir) {
		t.Errorf("deps.go must start with the forge:deps marker:\n%s", deps)
	}
	for _, want := range []string{"// Clock tells the time.\ntype Clock interface", `import "time"`} {
		if !strings.Contains(deps, want) {
			t.Errorf("deps.go lacks %q:\n%s", want, deps)
		}
	}
	if strings.Contains(deps, `"context"`) {
		t.Errorf("deps.go kept the import only Service used:\n%s", deps)
	}

	svc := read(t, filepath.Join(dir, "service.go"))
	for _, want := range []string{
		"// Package billing charges customers.\n//\n//forge:outbound-io: calls the payment provider\npackage billing",
		"// service charges customers.\n//\n//forge:contract\ntype service struct",
		"// Charge bills a customer.\nfunc (s *service) Charge(",
		"// Refund undoes a charge.\n// forge:no-observe\nfunc (s *service) Refund(",
		"// Status reports a charge's state.\nfunc (s service) Status(",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("service.go lacks %q:\n%s", want, svc)
		}
	}
	if !contractsrc.HasMarkedType(dir) || len(contractsrc.HandWrittenContracts(dir)) != 0 {
		t.Errorf("the package must now be marked, with no hand-written contract")
	}

	gen, err := contract.RenderDerivedContract(dir, nil)
	if err != nil {
		t.Fatalf("RenderDerivedContract: %v", err)
	}
	for _, want := range []string{
		"\t// Charge bills a customer.\n\tCharge(ctx context.Context, customer string, amount int64) error",
		"\t// forge:no-observe\n\tRefund(ctx context.Context, id string) error",
		"\tStatus(_ context.Context, id string) (string, error)",
	} {
		if !strings.Contains(string(gen), want) {
			t.Errorf("derived contract lacks %q:\n%s", want, gen)
		}
	}

	// A second run refuses: the package already derives its contract.
	var refused *RefusedError
	if _, err := Plan(root, dir); !errors.As(err, &refused) || !strings.Contains(err.Error(), "already derives") {
		t.Errorf("second run: %v", err)
	}
}

// contract.go that still declares the Deps struct (and New) stays, minus
// Service and the imports only Service used; its interfaces stay with it.
// The type in a `type (...)` group takes the marker inside the group.
func TestPlan_KeepsContractWithDeps(t *testing.T) {
	root, dir := writePkg(t, map[string]string{
		"contract.go": `package billing

import (
	"context"
	"time"
)

// Service schedules.
type Service interface {
	Next(after time.Time) time.Time
}

// Store persists.
type Store interface {
	Save(ctx context.Context, key string) error
}

// Deps are the collaborators.
type Deps struct {
	Store Store
}

// New builds it.
func New(d Deps) (Service, error) { return &scheduler{deps: d}, nil }
`,
		"scheduler.go": `package billing

import "time"

type (
	tick int

	// scheduler is the one implementation.
	scheduler struct {
		deps Deps
	}
)

func (s *scheduler) Next(after time.Time) time.Time { return after }
`,
	})
	m := migrate(t, root, dir)
	if !m.KeptContract || m.DepsFile != "" {
		t.Errorf("migration = %+v", m)
	}
	c := read(t, filepath.Join(dir, "contract.go"))
	if strings.Contains(c, "Service interface") || strings.Contains(c, `"time"`) {
		t.Errorf("contract.go kept Service or its only import:\n%s", c)
	}
	for _, want := range []string{`import "context"`, "type Store interface", "type Deps struct", "func New("} {
		if !strings.Contains(c, want) {
			t.Errorf("contract.go lacks %q:\n%s", want, c)
		}
	}
	s := read(t, filepath.Join(dir, "scheduler.go"))
	if !strings.Contains(s, "\t// scheduler is the one implementation.\n\t//\n\t//forge:contract\n\tscheduler struct") {
		t.Errorf("scheduler.go:\n%s", s)
	}
	if _, err := contract.RenderDerivedContract(dir, nil); err != nil {
		t.Errorf("RenderDerivedContract: %v", err)
	}
}

// A package whose deps.go is forge's gains contract.go's interfaces there,
// with their imports.
func TestPlan_AppendsToMarkedDeps(t *testing.T) {
	root, dir := writePkg(t, map[string]string{
		"contract.go": `package billing

import (
	"context"
	"io"
)

type Service interface {
	Run(ctx context.Context) error
}

// Sink receives output.
type Sink interface {
	io.Writer
}
`,
		"deps.go": depsurface.DepsFileMarker + `

package billing

import "context"

// Store persists.
type Store interface {
	Save(ctx context.Context) error
}
`,
		"service.go": `package billing

import "context"

type Deps struct {
	Store Store
	Sink  Sink
}

type service struct{ deps Deps }

func (s *service) Run(ctx context.Context) error { return nil }
`,
	})
	migrate(t, root, dir)
	deps := read(t, filepath.Join(dir, "deps.go"))
	if !strings.HasPrefix(deps, depsurface.DepsFileMarker) {
		t.Errorf("deps.go lost its marker:\n%s", deps)
	}
	for _, want := range []string{"type Store interface", "// Sink receives output.\ntype Sink interface", `"io"`, `"context"`} {
		if !strings.Contains(deps, want) {
			t.Errorf("deps.go lacks %q:\n%s", want, deps)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "contract.go")); !os.IsNotExist(err) {
		t.Errorf("contract.go should be removed")
	}
	s := read(t, filepath.Join(dir, "service.go"))
	if !strings.Contains(s, "//forge:contract\ntype service struct") {
		t.Errorf("service.go:\n%s", s)
	}
}

// The shape forge's old scaffold wrote: contract.go with Service and the
// dependency-interface guide but no interface, beside a service.go with the
// same ownership banner. contract.go goes; its banner is not duplicated, its
// scaffold description and guide go with it, and a package directive left
// in its body survives in service.go.
func TestPlan_OldScaffoldShape(t *testing.T) {
	root, dir := writePkg(t, map[string]string{
		"contract.go": `// yours: scaffolded once, never touched again — forge will not overwrite this file
// Package contract; canonical pattern lives in ` + "`forge skill load contracts`" + `.
package billing

import "context"

// Service defines the billing package boundary.
// All exported methods on the implementation type must be declared here.
type Service interface {
	Run(ctx context.Context) error
}

// ─── Dependency interfaces ──────────────────────────────────────────
//
// Declare here the *behaviour* this package needs from each thing it
// calls.

//forge:outbound-io: calls the payment provider
`,
		"service.go": `// yours: scaffolded once, never touched again — forge will not overwrite this file
// Service implementation; canonical pattern in ` + "`forge skill load service-layer`" + `.
package billing

import "context"

type Deps struct{}

type service struct {
	deps Deps
}

func (s *service) Run(ctx context.Context) error { return nil }
`,
	})
	m := migrate(t, root, dir)
	if _, err := os.Stat(filepath.Join(dir, "contract.go")); !os.IsNotExist(err) {
		t.Errorf("contract.go should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "deps.go")); !os.IsNotExist(err) {
		t.Errorf("no dependency interface moved, so no deps.go")
	}
	s := read(t, filepath.Join(dir, "service.go"))
	want := "// yours: scaffolded once, never touched again — forge will not overwrite this file\n// Service implementation; canonical pattern in `forge skill load service-layer`.\npackage billing\n"
	if !strings.HasPrefix(s, want) {
		t.Errorf("service.go header changed:\n%s", s)
	}
	for _, want := range []string{
		"// service defines the billing package boundary.\n// All exported methods on the implementation type must be declared here.\n//\n//forge:contract\ntype service struct",
		"\n//forge:outbound-io: calls the payment provider\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("service.go lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Dependency interfaces") || strings.Contains(s, "Package contract") {
		t.Errorf("contract.go's own prose moved into service.go:\n%s", s)
	}
	if !strings.Contains(strings.Join(m.Rewrites, "\n"), "3 free-standing comment line(s)") {
		t.Errorf("the dropped guide must be reported: %q", m.Rewrites)
	}
}

// Every shape the migration would change the contract of is refused, with
// the reason, and nothing is written.
func TestPlan_Refusals(t *testing.T) {
	impl := `package billing

import "context"

type service struct{}

func (s *service) Run(ctx context.Context) error { return nil }
`
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "two implementations",
			files: map[string]string{
				"contract.go": "package billing\n\nimport \"context\"\n\ntype Service interface {\n\tRun(ctx context.Context) error\n}\n",
				"service.go":  impl,
				"other.go":    "package billing\n\nimport \"context\"\n\ntype fake struct{}\n\nfunc (fake) Run(context.Context) error { return nil }\n",
			},
			want: "2 types implement Service — fake (other.go:5), service (service.go:5)",
		},
		{
			name: "embedded interface",
			files: map[string]string{
				"contract.go": "package billing\n\nimport (\n\t\"context\"\n\t\"io\"\n)\n\ntype Service interface {\n\tio.Closer\n\tRun(ctx context.Context) error\n}\n",
				"service.go":  impl,
			},
			want: "Service embeds io.Closer",
		},
		{
			name: "extra exported method",
			files: map[string]string{
				"contract.go": "package billing\n\nimport \"context\"\n\ntype Service interface {\n\tRun(ctx context.Context) error\n}\n",
				"service.go":  impl + "\nfunc (s *service) Close() error { return nil }\n",
			},
			want: "service exports methods Service does not have — Close (service.go:9)",
		},
		{
			name: "already marked",
			files: map[string]string{
				"contract.go": "package billing\n\ntype Deps struct{}\n",
				"service.go":  strings.Replace(impl, "type service struct{}", "//forge:contract\ntype service struct{}", 1),
			},
			want: "already derives its contract from the //forge:contract type service (service.go:6)",
		},
		{
			name: "no Service",
			files: map[string]string{
				"contract.go": "package billing\n\ntype Deps struct{}\n",
				"service.go":  impl,
			},
			want: "contract.go declares no `type Service interface`",
		},
		{
			name: "no contract.go",
			files: map[string]string{
				"service.go": impl,
			},
			want: "it has no contract.go",
		},
		{
			name: "differently named contract interface",
			files: map[string]string{
				"contract.go": "package billing\n\nimport \"context\"\n\n//forge:service\ntype Runner interface {\n\tRun(ctx context.Context) error\n}\n",
				"service.go":  impl,
			},
			want: "its contract interface is Runner (contract.go:6), and a derived contract is always named Service",
		},
		{
			name: "excluded package",
			files: map[string]string{
				"contract.go": "//forge:exclude-contract: hand-rolled\npackage billing\n\nimport \"context\"\n\ntype Service interface {\n\tRun(ctx context.Context) error\n}\n",
				"service.go":  impl,
			},
			want: "contract.go:1 opts it out of contract generation",
		},
		{
			name: "generic implementation",
			files: map[string]string{
				"contract.go": "package billing\n\nimport \"context\"\n\ntype Service interface {\n\tRun(ctx context.Context) error\n}\n",
				"service.go":  "package billing\n\nimport \"context\"\n\ntype service[T any] struct{}\n\nfunc (s *service[T]) Run(ctx context.Context) error { return nil }\n",
			},
			want: "is generic",
		},
		{
			name: "signature differs",
			files: map[string]string{
				"contract.go": "package billing\n\nimport \"context\"\n\ntype Service interface {\n\tRun(ctx context.Context) error\n}\n",
				"service.go":  "package billing\n\nimport \"context\"\n\ntype service struct{}\n\nfunc (s *service) Run(ctx context.Context) (int, error) { return 0, nil }\n",
			},
			want: "service.Run is Run(ctx context.Context) (int, error), Service has Run(ctx context.Context) error",
		},
		{
			name: "only a promoted implementation",
			files: map[string]string{
				"contract.go": "package billing\n\nimport \"context\"\n\ntype Service interface {\n\tRun(ctx context.Context) error\n}\n",
				"service.go":  impl + "\ntype wrapper struct{ *service }\n",
			},
			// service declares Run: wrapper only promotes it, so it is not a
			// second implementation; this case migrates.
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, dir := writePkg(t, tc.files)
			m, err := Plan(root, dir)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Plan: %v", err)
				}
				return
			}
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Plan = %+v, %v; want a refusal", m, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %v\nwant it to contain %q", err, tc.want)
			}
			for name, src := range tc.files {
				if got := read(t, filepath.Join(dir, name)); got != src {
					t.Errorf("%s was rewritten by a refused plan", name)
				}
			}
		})
	}
}

// Parameter names and import aliases do not make a signature differ: the
// implementation's file may name a package differently from contract.go.
func TestPlan_AliasesAndNamesAreTheSameSignature(t *testing.T) {
	root, dir := writePkg(t, map[string]string{
		"contract.go": "package billing\n\nimport (\n\tstdctx \"context\"\n\t\"time\"\n)\n\ntype Service interface {\n\tAt(c stdctx.Context, d time.Duration, more ...string) (t time.Time, err error)\n}\n",
		"service.go":  "package billing\n\nimport (\n\t\"context\"\n\ttm \"time\"\n)\n\ntype service struct{}\n\nfunc (service) At(context.Context, tm.Duration, ...string) (tm.Time, error) { return tm.Time{}, nil }\n",
	})
	migrate(t, root, dir)
	if _, err := os.Stat(filepath.Join(dir, "contract.go")); !os.IsNotExist(err) {
		t.Errorf("contract.go should be removed")
	}
}

// The rewritten package, with the contract_gen.go forge derives from it,
// builds and vets: the migration is a compile-preserving change.
func TestPlan_ResultBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("execs go build and go vet; runs in task test")
	}
	root, dir := writePkg(t, map[string]string{"contract.go": happyContract, "service.go": happyService})
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/proj\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	migrate(t, root, dir)
	if _, err := contract.SyncDerivedContract(dir, contract.Options{}); err != nil {
		t.Fatalf("SyncDerivedContract: %v", err)
	}
	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet: %v\n%s", err, out)
	}
}
