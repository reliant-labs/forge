package contract

import (
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/goexec"
)

// loggerContract is the scaffolded package shape: Deps with a Logger, the
// implementing struct holding Deps, pointer receivers.
const loggerContract = `package store

import "context"

type Service interface {
	Get(ctx context.Context, id string) (string, error)
}
`

const loggerService = `package store

import (
	"context"
	"errors"
	"log/slog"
)

type Deps struct {
	Logger *slog.Logger
}

type service struct {
	deps Deps
}

func New(deps Deps) (Service, error) { return &service{deps: deps}, nil }

func (s *service) Get(ctx context.Context, id string) (string, error) {
	return "", errors.New("not found")
}
`

func writeLoggerPackage(t *testing.T, service string) string {
	t.Helper()
	dir := writeContract(t, loggerContract)
	if err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(service), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestObservedDecorator_BindsTheComponentLogger is the regression for the
// decorator whose chain logged to slog.Default whatever logger the component
// was built with: NewServiceWithForgeMiddleware reads the component's
// Deps.Logger back off the implementing value and binds it.
func TestObservedDecorator_BindsTheComponentLogger(t *testing.T) {
	got := genDecorator(t, writeLoggerPackage(t, loggerService), "Service", "store")
	for _, want := range []string{
		"chain := newObserveChain()",
		"if component, ok := inner.(*service); ok && component != nil {\n\t\tchain = chain.WithLogger(component.deps.Logger)\n\t}",
		"return &forgeMiddlewareService{inner: inner, chain: chain}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("decorator missing %q\n---\n%s", want, got)
		}
	}
}

// TestObservedDecorator_LoggerBindingShapes: an embedded Deps and value
// receivers are bound too; a package whose Deps has no slog Logger, or whose
// implementation holds no Deps, keeps the one-line constructor.
func TestObservedDecorator_LoggerBindingShapes(t *testing.T) {
	embedded := strings.NewReplacer(
		"type service struct {\n\tdeps Deps\n}", "type service struct {\n\tDeps\n}",
		"&service{deps: deps}", "service{Deps: deps}",
		"func (s *service)", "func (s service)",
	).Replace(loggerService)
	got := genDecorator(t, writeLoggerPackage(t, embedded), "Service", "store")
	for _, want := range []string{
		"switch component := inner.(type) {",
		"case *service:\n\t\tif component != nil {\n\t\t\tchain = chain.WithLogger(component.Deps.Logger)\n\t\t}",
		"case service:\n\t\tchain = chain.WithLogger(component.Deps.Logger)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("embedded/value decorator missing %q\n---\n%s", want, got)
		}
	}

	for name, src := range map[string]string{
		"no Logger":       strings.Replace(loggerService, "Logger *slog.Logger", "Log *slog.Logger", 1),
		"not slog":        strings.Replace(loggerService, "Logger *slog.Logger", "Logger *log.Logger", 1),
		"no Deps on impl": strings.Replace(strings.Replace(loggerService, "deps Deps", "name string", 1), "&service{deps: deps}", "&service{}", 1),
		"nil pointer Deps": strings.NewReplacer(
			"deps Deps", "deps *Deps",
			"&service{deps: deps}", "&service{}",
		).Replace(loggerService),
		"nil embedded pointer Deps": strings.NewReplacer(
			"type service struct {\n\tdeps Deps\n}", "type service struct {\n\t*Deps\n}",
			"&service{deps: deps}", "service{}",
			"func (s *service)", "func (s service)",
		).Replace(loggerService),
		"blank Deps": strings.NewReplacer(
			"deps Deps\n}", "_ Deps\n}",
			"&service{deps: deps}", "&service{}",
		).Replace(loggerService),
	} {
		got := genDecorator(t, writeLoggerPackage(t, src), "Service", "store")
		if strings.Contains(got, "WithLogger") {
			t.Errorf("%s: no Deps.Logger to bind, but the decorator binds one:\n%s", name, got)
		}
		if !strings.Contains(got, "return &forgeMiddlewareService{inner: inner, chain: newObserveChain()}") {
			t.Errorf("%s: the constructor should stay the one-liner:\n%s", name, got)
		}
	}
}

func TestObservedDecorator_LoggerBindingBuildConstraints(t *testing.T) {
	otherOS := "windows"
	if build.Default.GOOS == otherOS {
		otherOS = "linux"
	}
	for _, name := range []string{"alternate_" + otherOS + ".go", "alternate.go"} {
		t.Run(name, func(t *testing.T) {
			// Only the excluded implementation has a Deps field. Combining it
			// with the active implementation would emit an invalid selector.
			active := strings.NewReplacer(
				"deps Deps\n}", "name string\n}",
				"&service{deps: deps}", "&service{}",
			).Replace(loggerService)
			dir := writeLoggerPackage(t, active)
			excluded := loggerService
			if name == "alternate.go" {
				excluded = "//go:build !" + build.Default.GOOS + "\n\n" + excluded
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(excluded), 0o644); err != nil {
				t.Fatal(err)
			}
			got := genDecorator(t, dir, "Service", "store")
			if strings.Contains(got, "WithLogger") {
				t.Fatalf("bound a field from an excluded implementation:\n%s", got)
			}
		})
	}
}

// Generation must not bake in the host's selector: the same generated file
// will also be compiled against the other platform's implementation.
func TestObservedDecorator_LoggerBindingCrossGOOS(t *testing.T) {
	for _, constraint := range []string{"filename", "build tag"} {
		t.Run(constraint, func(t *testing.T) {
			dir := writeContract(t, loggerContract)
			for _, goos := range []string{"linux", "windows"} {
				src := strings.ReplaceAll(loggerService, "deps", goos+"Deps")
				name := "service_" + goos + ".go"
				if constraint == "build tag" {
					name = goos + "_service.go"
					src = "//go:build " + goos + "\n\n" + src
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := genDecorator(t, dir, "Service", "store")
			if strings.Contains(got, "WithLogger") {
				t.Fatalf("bound a platform-specific selector into a shared decorator:\n%s", got)
			}
		})
	}
}

// Generate once for Linux, then compile those exact bytes for both targets.
func TestObservedDecorator_LoggerBindingCrossGOOSCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: cross-compiles a throwaway module; runs in task test")
	}
	t.Setenv("GOOS", "linux")
	t.Setenv("GOARCH", "amd64")
	forgeRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(forgeRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	for _, constraint := range []string{"filename", "build tag"} {
		t.Run(constraint, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"go.mod": "module example.com/proj\n\ngo 1.22\n\n" +
					"require github.com/reliant-labs/forge v0.0.0\n\n" +
					"replace github.com/reliant-labs/forge => " + filepath.ToSlash(forgeRoot) + "\n",
				"go.sum":                     string(sum),
				"internal/store/contract.go": loggerContract,
			}
			for _, goos := range []string{"linux", "windows"} {
				src := strings.ReplaceAll(loggerService, "deps", goos+"Deps")
				name := "service_" + goos + ".go"
				if constraint == "build tag" {
					name = goos + "_service.go"
					src = "//go:build " + goos + "\n\n" + src
				}
				files["internal/store/"+name] = src
			}
			writeTree(t, root, files)
			dir := filepath.Join(root, "internal", "store")
			cf, err := ParseContract(filepath.Join(dir, "contract.go"))
			if err != nil {
				t.Fatal(err)
			}
			def := &DefaultChain{Scope: "example.com/proj/internal/store", Name: "store", LogLevel: "slog.LevelDebug"}
			if _, err := WriteObservedDecorator(cf, dir, "Service", "", "", "store", def, Options{}); err != nil {
				t.Fatal(err)
			}
			for _, goos := range []string{"linux", "windows"} {
				cmd := exec.Command("go", "build", "./...")
				cmd.Dir = root
				cmd.Env = goexec.WithTrimpath(append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "CGO_ENABLED=0", "GOOS="+goos))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("go build for %s: %v\n%s", goos, err, out)
				}
			}
		})
	}
}

// An unrelated pointer-receiver helper does not prevent service values from
// satisfying Service or having their logger bound by the decorator.
func TestObservedDecorator_ValueLoggerWithPointerHelper(t *testing.T) {
	src := strings.NewReplacer(
		"&service{deps: deps}", "service{deps: deps}",
		"func (s *service) Get", "func (s service) Get",
	).Replace(loggerService) + "\nfunc (s *service) reset() {}\n"
	got := genDecorator(t, writeLoggerPackage(t, src), "Service", "store")
	for _, want := range []string{
		"switch component := inner.(type) {",
		"case *service:",
		"case service:\n\t\tchain = chain.WithLogger(component.deps.Logger)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("value implementation with pointer helper missing %q\n---\n%s", want, got)
		}
	}
}

// TestGenerate_DecoratorLogsToTheComponentLogger compiles the decorator
// against this checkout's pkg/observe and runs it: a component built with its
// own logger writes its failed call there and nothing to slog.Default, through
// both the default chain and a seam scaffolded with a nil logger.
func TestGenerate_DecoratorLogsToTheComponentLogger(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: builds and runs a throwaway module with `go test`; runs in task test")
	}
	forgeRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(forgeRoot, "go.sum"))
	if err != nil {
		t.Fatalf("read forge go.sum: %v", err)
	}
	const probe = `package store

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestDecoratorLogsToTheComponentLogger(t *testing.T) {
	var global, own bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&global, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc, err := NewInstrumented(Deps{Logger: slog.New(slog.NewTextHandler(&own, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(context.Background(), "x"); err == nil {
		t.Fatal("want the inner error")
	}
	if global.Len() != 0 {
		t.Errorf("the decorator logged to slog.Default:\n%s", global.String())
	}
	if !strings.Contains(own.String(), "store.Get") {
		t.Errorf("the failed call is not on the component's logger:\n%s", own.String())
	}
}
`
	const seam = `package store

import (
	"log/slog"

	"github.com/reliant-labs/forge/pkg/observe"
)

func newObserveChain() *observe.ComponentChain {
	var logger *slog.Logger
	return observe.NewComponentChain(observe.RecoverMiddleware(logger), observe.LogMiddleware(logger, slog.LevelDebug))
}
`
	for name, withSeam := range map[string]bool{"default chain": false, "nil-logger seam": true} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"go.mod": "module example.com/proj\n\ngo 1.22\n\n" +
					"require github.com/reliant-labs/forge v0.0.0\n\n" +
					"replace github.com/reliant-labs/forge => " + filepath.ToSlash(forgeRoot) + "\n",
				"go.sum":                       string(sum),
				"internal/store/contract.go":   loggerContract,
				"internal/store/service.go":    loggerService,
				"internal/store/probe_test.go": probe,
			}
			if withSeam {
				files["internal/store/observe_chain.go"] = seam
			}
			writeTree(t, root, files)
			dir := filepath.Join(root, "internal", "store")
			cf, err := ParseContract(filepath.Join(dir, "contract.go"))
			if err != nil {
				t.Fatalf("ParseContract: %v", err)
			}
			var def *DefaultChain
			if !withSeam {
				def = &DefaultChain{Scope: "example.com/proj/internal/store", Name: "store", LogLevel: "slog.LevelDebug"}
			}
			if _, err := WriteObservedDecorator(cf, dir, "Service", "", "", "store", def, Options{}); err != nil {
				t.Fatalf("WriteObservedDecorator: %v", err)
			}
			cmd := exec.Command("go", "test", "./...")
			cmd.Dir = root
			cmd.Env = goexec.WithTrimpath(append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off"))
			if out, err := cmd.CombinedOutput(); err != nil {
				deco, _ := os.ReadFile(filepath.Join(dir, "middleware_gen.go"))
				t.Fatalf("go test: %v\n%s\n---- middleware_gen.go ----\n%s", err, out, deco)
			}
		})
	}
}
