package cigate

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testModule = "example.com/shop"

// findingsRoot is a project tree holding the files the fixtures name, so a
// generic `path:line:` line is placed only when the file exists.
func findingsRoot(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func assertFindings(t *testing.T, got, want []Finding) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		var g, w strings.Builder
		for _, f := range got {
			fmt.Fprintf(&g, "  %+v\n", f)
		}
		for _, f := range want {
			fmt.Fprintf(&w, "  %+v\n", f)
		}
		t.Errorf("findings:\n%swant:\n%s", g.String(), w.String())
	}
}

// go test's plain output: a test log line names only a file's base name, and
// the package line that follows the block places it. Subtests, multi-line
// messages, a panic with no log line, a passing package, and a package that
// failed with no test (TestMain exit) are each placed.
func TestExtractFindings_GoTest(t *testing.T) {
	out := `task: [test] go test -race -count=1 ./...
--- FAIL: TestCheckout (0.00s)
    checkout_test.go:42: total = 10, want 12
--- FAIL: TestOrders (0.00s)
    --- FAIL: TestOrders/empty (0.00s)
        orders_test.go:17: got error: boom
            more detail
            and more
    --- FAIL: TestOrders/nil (0.00s)
        orders_test.go:30: unexpected nil
FAIL
FAIL	example.com/shop/internal/orders	0.031s
ok  	example.com/shop/internal/billing	0.010s
--- FAIL: TestCrash (0.00s)
panic: runtime error: index out of range [3] with length 1 [recovered]

goroutine 7 [running]:
	/home/ci/shop/internal/cart/cart_test.go:12 +0x1d
FAIL	example.com/shop/internal/cart	0.020s
FAIL	example.com/shop	0.004s
FAIL
`
	got, omitted := ExtractFindings(out, FindingsInput{Module: testModule})
	assertFindings(t, got, []Finding{
		{Path: ".", Level: LevelError, Message: "example.com/shop failed"},
		{Path: "internal/cart", Level: LevelError, Message: "TestCrash: panic: runtime error: index out of range [3] with length 1 [recovered]"},
		{Path: "internal/orders/checkout_test.go", Line: 42, Level: LevelError, Message: "TestCheckout: total = 10, want 12"},
		{Path: "internal/orders/orders_test.go", Line: 17, Level: LevelError, Message: "TestOrders/empty: got error: boom\nmore detail\nand more"},
		{Path: "internal/orders/orders_test.go", Line: 30, Level: LevelError, Message: "TestOrders/nil: unexpected nil"},
	})
	if omitted != 0 {
		t.Errorf("omitted %d", omitted)
	}
}

// go test -json carries the same text in its output events, in -v order: a
// test's log lines come BEFORE its --- FAIL line, and a passing test's log is
// printed too, which is not a finding. Each event's Test attributes its line.
func TestExtractFindings_GoTestJSON(t *testing.T) {
	root := findingsRoot(t, "internal/orders/orders_test.go")
	out := `{"Action":"start","Package":"example.com/shop/internal/orders"}
{"Action":"run","Package":"example.com/shop/internal/orders","Test":"TestOK"}
{"Action":"output","Package":"example.com/shop/internal/orders","Test":"TestOK","Output":"=== RUN   TestOK\n"}
{"Action":"output","Package":"example.com/shop/internal/orders","Test":"TestOK","Output":"    orders_test.go:3: just logging\n"}
{"Action":"output","Package":"example.com/shop/internal/orders","Test":"TestOK","Output":"--- PASS: TestOK (0.00s)\n"}
{"Action":"run","Package":"example.com/shop/internal/orders","Test":"TestOrders"}
{"Action":"output","Package":"example.com/shop/internal/orders","Test":"TestOrders","Output":"=== RUN   TestOrders\n"}
{"Action":"output","Package":"example.com/shop/internal/orders","Test":"TestOrders","Output":"    orders_test.go:9: want 2 rows\n"}
{"Action":"output","Package":"example.com/shop/internal/orders","Test":"TestOrders","Output":"--- FAIL: TestOrders (0.00s)\n"}
{"Action":"fail","Package":"example.com/shop/internal/orders","Test":"TestOrders"}
{"Action":"output","Package":"example.com/shop/internal/orders","Output":"FAIL\n"}
{"Action":"output","Package":"example.com/shop/internal/orders","Output":"FAIL\texample.com/shop/internal/orders\t0.01s\n"}
{"Action":"fail","Package":"example.com/shop/internal/orders"}
`
	got, _ := ExtractFindings(out, FindingsInput{Root: root, Module: testModule})
	assertFindings(t, got, []Finding{
		{Path: "internal/orders/orders_test.go", Line: 9, Level: LevelError, Message: "TestOrders: want 2 rows"},
	})
}

// A package that does not compile: the compiler's located lines are the
// findings, and the package is not reported a second time.
func TestExtractFindings_GoTestBuildFailure(t *testing.T) {
	root := findingsRoot(t, "internal/orders/orders.go")
	out := `# example.com/shop/internal/orders
internal/orders/orders.go:12:2: undefined: total
FAIL	example.com/shop/internal/orders [build failed]
`
	got, _ := ExtractFindings(out, FindingsInput{Root: root, Module: testModule})
	assertFindings(t, got, []Finding{
		{Path: "internal/orders/orders.go", Line: 12, Column: 2, Level: LevelError, Message: "undefined: total"},
	})
}

// golangci-lint's text output: the located line, then the source line and a
// caret, which are not findings. A path that names no file in the project
// (a URL, a generated path elsewhere) is dropped.
func TestExtractFindings_Golangci(t *testing.T) {
	root := findingsRoot(t, "internal/orders/orders.go", "cmd/server/main.go")
	out := `internal/orders/orders.go:23:9: Error return value of ` + "`rows.Close`" + ` is not checked (errcheck)
	defer rows.Close()
	      ^
cmd/server/main.go:88:1: Function 'main' is too long (171 > 150) (funlen)
internal/orders/orders.go:40:2: warning: S1009: should omit nil check (gosimple)
vendor/x/y.go:1:1: not ours (revive)
see https://golangci-lint.run:443: for help
3 issues:
* errcheck: 1
`
	got, _ := ExtractFindings(out, FindingsInput{Root: root, Module: testModule})
	assertFindings(t, got, []Finding{
		{Path: "cmd/server/main.go", Line: 88, Column: 1, Level: LevelError, Message: "Function 'main' is too long (171 > 150) (funlen)"},
		{Path: "internal/orders/orders.go", Line: 23, Column: 9, Level: LevelError, Message: "Error return value of `rows.Close` is not checked (errcheck)"},
		{Path: "internal/orders/orders.go", Line: 40, Column: 2, Level: LevelWarning, Message: "S1009: should omit nil check (gosimple)"},
	})
}

// buf lint (no space after the last colon), tsc relative to the check's
// directory, and an absolute path under the root.
func TestExtractFindings_BufTSCAndAbsolute(t *testing.T) {
	root := findingsRoot(t, "proto/shop/v1/shop.proto", "frontends/web/src/app.tsx", "internal/a/a.go")
	out := "proto/shop/v1/shop.proto:12:3:Field name \"Total\" should be lower_snake_case.\n" +
		"src/app.tsx(7,14): error TS2322: Type 'string' is not assignable to type 'number'.\n" +
		filepath.Join(root, "internal", "a", "a.go") + ":5:1: vet: unreachable code\n"
	// A relative path is relative to the check's directory: from the
	// frontend, tsc's path is found and buf's is not. The absolute path is
	// placed either way.
	got, _ := ExtractFindings(out, FindingsInput{Root: root, Dir: "frontends/web", Module: testModule})
	assertFindings(t, got, []Finding{
		{Path: "frontends/web/src/app.tsx", Line: 7, Column: 14, Level: LevelError, Message: "TS2322: Type 'string' is not assignable to type 'number'."},
		{Path: "internal/a/a.go", Line: 5, Column: 1, Level: LevelError, Message: "vet: unreachable code"},
	})
	got, _ = ExtractFindings(out, FindingsInput{Root: root, Module: testModule})
	assertFindings(t, got, []Finding{
		{Path: "internal/a/a.go", Line: 5, Column: 1, Level: LevelError, Message: "vet: unreachable code"},
		{Path: "proto/shop/v1/shop.proto", Line: 12, Column: 3, Level: LevelError, Message: `Field name "Total" should be lower_snake_case.`},
	})
}

// The same output always gives the same list: sorted, de-duplicated, capped.
func TestExtractFindings_DeterministicAndCapped(t *testing.T) {
	root := findingsRoot(t, "a.go")
	var b strings.Builder
	for i := MaxFindings + 25; i >= 1; i-- {
		fmt.Fprintf(&b, "a.go:%d:1: issue %d\n", i, i)
		fmt.Fprintf(&b, "a.go:%d:1: issue %d\n", i, i) // repeated: one finding
	}
	got, omitted := ExtractFindings(b.String(), FindingsInput{Root: root})
	if len(got) != MaxFindings || omitted != 25 {
		t.Fatalf("got %d findings, %d omitted; want %d and 25", len(got), omitted, MaxFindings)
	}
	for i, f := range got {
		if f.Line != i+1 {
			t.Fatalf("finding %d is line %d: not sorted", i, f.Line)
		}
	}
	again, _ := ExtractFindings(b.String(), FindingsInput{Root: root})
	if !reflect.DeepEqual(got, again) {
		t.Error("the same output gave a different list")
	}
}
