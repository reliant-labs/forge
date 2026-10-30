package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/goexec"
)

// A contract filename is not evidence that Deps, New or Service exists.
// Pure libraries must not acquire a compile-broken component test factory.
func TestComponentHelpersRequireConstructibleShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"pure-profile-library", `type Profile struct{ Name string }
func Profiles() []Profile { return nil }`, false},
		{"interface-only", `type Service interface{ Run() }`, false},
		{"deps-without-constructor", `type Deps struct{}
type Service struct{}`, false},
		{"constructor-without-deps", `type Service struct{}
func New() *Service { return nil }`, false},
		{"unrelated-constructor", `type Deps struct{}
type Profile struct{}
func New(name string) Profile { return Profile{} }`, false},
		{"constructor-without-result", `type Deps struct{}
func New(Deps) {}`, false},
		{"constructor-error-only", `type Deps struct{}
func New(Deps) error { return nil }`, false},
		{"constructor-extra-results", `type Deps struct{}
type Service struct{}
func New(Deps) (Service, bool, error) { return Service{}, true, nil }`, false},
		{"generic-constructor", `type Deps struct{}
type Service struct{}
func New[T any](Deps) Service { return Service{} }`, false},
		{"canonical-service", `type Deps struct{}
type Service interface{ Run() }
func New(Deps) Service { return nil }`, true},
		{"concrete-service", `type Deps struct{}
type Service struct{}
func New(Deps) (*Service, error) { return nil, nil }`, true},
		{"named-contract-and-constructor", `type Deps struct{}
type Provider interface{ Run() }
// forge:constructor
func Open(Deps) (Provider, error) { return nil, nil }`, true},
		{"marked-constructor-wins", `type Deps struct{}
type Provider interface{ Run() }
func New() string { return "not the component constructor" }
// forge:constructor
func Open(Deps) (Provider, error) { return nil, nil }`, true},
		{"test-only-constructor", `type Profile struct{}`, false},
		{"generated-only-constructor", `type Profile struct{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeHarnessFixture(t, map[string]string{
				"internal/widget/contract.go": "package widget\n" + tc.src + "\n",
			})
			dir := filepath.Join(root, "internal", "widget")
			// Neither tests nor generated output may provide the evidence
			// that causes another generated file to refer to production symbols.
			if tc.name == "test-only-constructor" || tc.name == "generated-only-constructor" {
				name := "shape_test.go"
				if tc.name == "generated-only-constructor" {
					name = "shape_gen.go"
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte("package widget\ntype Deps struct{}\ntype Service struct{}\nfunc New(Deps) Service { return Service{} }\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := IsWiredComponentDir(dir); got != tc.want {
				t.Errorf("IsWiredComponentDir = %v, want %v", got, tc.want)
			}
			wantName := "Widget"
			if tc.want {
				wantName = "SvcWidget"
			}
			if got := ComputeTestHelperName("widget", root); got != wantName {
				t.Errorf("helper name = %q, want %q", got, wantName)
			}
			pkgs, err := PackageDataFromNames([]string{"widget"}, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := GenerateBootstrapTesting(BootstrapTestingGenInput{
				GenContext: GenContext{ProjectDir: root, ModulePath: "example.com/proj"},
				Packages:   pkgs,
			}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, componentTestHelperFile))
			if !tc.want {
				if !os.IsNotExist(err) {
					t.Fatalf("non-component got helper file: err=%v\n%s", err, data)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "func NewTestWidget(") {
				t.Fatalf("component lost its test factory:\n%s", data)
			}
			if tc.name == "named-contract-and-constructor" || tc.name == "marked-constructor-wins" {
				for _, want := range []string{"opts ...TestOption) Provider", "svc, err := Open(Deps{})", "t.Fatalf(\"creating widget package: %v\", err)"} {
					if !strings.Contains(string(data), want) {
						t.Errorf("named component helper missing %q:\n%s", want, data)
					}
				}
			}
		})
	}
}

// Type-check the real emitted test files beside both a pure library and a
// service whose constructor and return type have non-canonical names.
func TestComponentHelpersPureAndServiceCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("executes go vet over a fixture module; runs in task test")
	}
	forgeRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(forgeRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	root := writeHarnessFixture(t, map[string]string{
		"go.mod": "module example.com/proj\n\ngo " + pkgGoVersion(t, forgeRoot) + "\n\n" +
			"require github.com/reliant-labs/forge v0.0.0\n\n" +
			"replace github.com/reliant-labs/forge => " + filepath.ToSlash(forgeRoot) + "\n",
		"go.sum": string(sum),
		"internal/profiles/contract.go": `package profiles
type Profile struct{ Name string }
func Profiles() []Profile { return nil }
`,
		"internal/widget/contract.go": `package widget
type Deps struct{}
type Provider interface{ Run() }
type provider struct{}
func (provider) Run() {}
func New() string { return "not the component constructor" }
// forge:constructor
func Open(Deps) (Provider, error) { return provider{}, nil }
`,
	})
	pkgs, err := PackageDataFromNames([]string{"profiles", "widget"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := GenerateBootstrapTesting(BootstrapTestingGenInput{
		GenContext: GenContext{ProjectDir: root, ModulePath: "example.com/proj"},
		Packages:   pkgs,
	}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "vet", "./internal/...")
	cmd.Dir = root
	cmd.Env = goexec.WithTrimpath(append(os.Environ(), "GOFLAGS="+os.Getenv("GOFLAGS")+" -mod=mod", "GOWORK=off"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pure library and component helpers do not compile: %v\n%s", err, out)
	}
}
