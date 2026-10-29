package codegen

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/reliant-labs/forge/internal/generator/contract"
	"github.com/reliant-labs/forge/pkg/servicecatalog"
)

// One fixture crosses BOTH real generators: embedded Reader.Get is exposed by
// Service in package orders under a directory deliberately named order-port.
func catalogDecoratorIdentityFixture(t *testing.T) (string, []servicecatalog.CodeMethod) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "internal/order-port")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(path, src string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	forgeRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "go.mod"), "module example.com/shop\n\ngo 1.27\n\nrequire github.com/reliant-labs/forge v0.0.0\nreplace github.com/reliant-labs/forge => "+filepath.ToSlash(forgeRoot)+"\n")
	src := `package orders
 type Reader interface { Get() error }
 type Service interface { Reader }
 type impl struct{}
 func (*impl) Get() error { return nil }
 `
	path := filepath.Join(dir, "contract.go")
	write(path, src)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := new(types.Config).Check("example.com/shop/internal/order-port", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := &graphUniverse{app: &packages.Package{Types: pkg, Fset: fset}}
	methods := catalogMethods(root, u, pkg.Scope().Lookup("Service").Type())
	cf, err := contract.ParseContract(path)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := contract.WriteObservedDecorator(cf, dir, "Service", "NewServiceWithForgeMiddleware", "forgeMiddlewareService", cf.Package, &contract.DefaultChain{Scope: pkg.Path(), Name: cf.Package}, contract.Options{})
	if err != nil || !ok {
		t.Fatalf("decorator: %v %v", ok, err)
	}
	raw, err := json.Marshal(methods)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dir, "catalog.json"), string(raw))
	return root, methods
}

func TestServiceCatalogJoinsGeneratedEmbeddedMethodIdentity(t *testing.T) {
	root, methods := catalogDecoratorIdentityFixture(t)
	if len(methods) != 1 {
		t.Fatalf("methods=%+v", methods)
	}
	m := methods[0]
	if m.ID != servicecatalog.MethodID("example.com/shop/internal/order-port", "Service", "Get") || m.LegacyName != "orders.Get" {
		t.Fatalf("exposed interface/package identity: %+v", m)
	}
	if m.Source.Path != "internal/order-port/contract.go" || m.Source.Line != 2 {
		t.Fatalf("declaration location lost: %+v", m.Source)
	}
	// Extract the ACTUAL generated map rather than reconstructing it in a test.
	path := filepath.Join(root, "internal/order-port/middleware_gen.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithMethodIDs" {
			return true
		}
		lit, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("missing identity map")
		}
		for _, el := range lit.Elts {
			kv := el.(*ast.KeyValueExpr)
			legacy, _ := strconv.Unquote(kv.Key.(*ast.BasicLit).Value)
			id, _ := strconv.Unquote(kv.Value.(*ast.BasicLit).Value)
			if id == m.ID && legacy == m.LegacyName {
				joined = true
			}
		}
		return true
	})
	if !joined {
		t.Fatalf("generated decorator identity absent from catalog: %+v", methods)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `o.chain.Run(context.Background(), "orders.Get"`) {
		t.Fatalf("legacy name does not match actual generated operation:\n%s", raw)
	}
}

func TestServiceCatalogGeneratedSpanJoinsEmbeddedMethod(t *testing.T) {
	if testing.Short() {
		t.Skip("executes generated fixture with go test; runs in full package tests")
	}
	root, _ := catalogDecoratorIdentityFixture(t)
	path := filepath.Join(root, "internal/order-port/span_test.go")
	src := `package orders
 import (
  "context"
  "encoding/json"
  "os"
  "testing"
  "github.com/reliant-labs/forge/pkg/servicecatalog"
  "go.opentelemetry.io/otel"
  sdktrace "go.opentelemetry.io/otel/sdk/trace"
  "go.opentelemetry.io/otel/sdk/trace/tracetest"
 )
 func TestGeneratedSpanCatalogJoin(t *testing.T) {
  recorder:=tracetest.NewSpanRecorder()
  provider:=sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
  defer provider.Shutdown(context.Background())
  otel.SetTracerProvider(provider)
  if err:=NewServiceWithForgeMiddleware(&impl{}).Get();err!=nil{t.Fatal(err)}
  raw,err:=os.ReadFile("catalog.json");if err!=nil{t.Fatal(err)}
  var catalog []servicecatalog.CodeMethod
  if err:=json.Unmarshal(raw,&catalog);err!=nil{t.Fatal(err)}
  spans:=recorder.Ended();if len(spans)!=1{t.Fatalf("spans=%d",len(spans))}
  span:=spans[0];id:=""
  for _,attr:=range span.Attributes(){if string(attr.Key)=="forge.method.id"{id=attr.Value.AsString()}}
  for _,m:=range catalog {if id==m.ID && span.Name()==m.LegacyName && span.InstrumentationScope().Name==m.Scope{return}}
  t.Fatalf("actual span id=%s name=%s scope=%s absent from complete catalog %+v",id,span.Name(),span.InstrumentationScope().Name,catalog)
 }
 `
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-mod=mod", "./internal/order-port", "-run", "^TestGeneratedSpanCatalogJoin$")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated span fixture: %v\n%s", err, out)
	}
}
