// File: internal/cli/generate_validate_tests_test.go
//
// The validate step's second half: generated _test.go files must be
// typechecked. `go build ./...` does not compile test files, which is
// how `forge generate` printed "✅ Code generation complete — 36
// file(s) written" over a tree whose generated helpers_gen_test.go was
// not valid Go. The user found out later, from a contract-linter
// "packages failed to load" and a "[build failed]" test run, neither of
// which named the real cause.

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeValidateFixture lays down a minimal module. Every file in extra
// is written verbatim, so a test can add a broken _test.go.
func writeValidateFixture(t *testing.T, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/validateapp\n\ngo 1.22\n",
		"lib.go": "package validateapp\n\nfunc F() int { return 1 }\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// TestRunGoBuildValidate_RejectsUncompilableTestFile is the regression
// for the shipped-broken-tree bug. The non-test code compiles cleanly,
// so `go build ./...` is green; only the _test.go is broken. Validation
// must fail anyway.
func TestRunGoBuildValidate_RejectsUncompilableTestFile(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real typecheck over a temp module; full mode only")
	}
	dir := writeValidateFixture(t, map[string]string{
		// The generated-helper shape: a stub method returning a
		// composite literal for an interface. This is precisely what
		// zeroValueForResultType now prevents, kept here as the
		// end-to-end proof that the GATE catches it even if a future
		// generator regresses.
		"helpers_gen_test.go": `package validateapp

type Store interface{ WithTx() Store }

type stubStore struct{}

func (stubStore) WithTx() Store { return Store{} }
`,
	})

	_, restore := captureStderr(t)
	err := runGoBuildValidate(dir)
	restore()

	if err == nil {
		t.Fatal("validation must fail when a _test.go file does not compile — " +
			"`go build ./...` alone skips test files, which is how a broken tree shipped as '✅ complete'")
	}
	if !strings.Contains(err.Error(), "helpers_gen_test.go") &&
		!strings.Contains(errOutputOf(err), "helpers_gen_test.go") {
		t.Errorf("the failure must name the offending test file so the user isn't left guessing; got: %v\n%s",
			err, errOutputOf(err))
	}
}

// TestRunGoBuildValidate_IgnoresVetStyleFindings is the guard that
// rules out plain `go vet ./...` as the implementation. Vet's analyzer
// suite reports real-but-non-fatal lint findings in HAND-WRITTEN code
// (measured: a fmt.Printf arity bug exits 1 while go build exits 0).
// Failing `forge generate` on those would break generation for projects
// that are entirely valid, so the check must be a pure TYPECHECK.
func TestRunGoBuildValidate_IgnoresVetStyleFindings(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real typecheck over a temp module; full mode only")
	}
	dir := writeValidateFixture(t, map[string]string{
		"printf.go": `package validateapp

import "fmt"

// A genuine vet finding (format arity), but valid Go that compiles.
func G() { fmt.Printf("%d\n") }
`,
		"ok_test.go": "package validateapp\n\nfunc use() int { return F() }\n",
	})

	_, restore := captureStderr(t)
	err := runGoBuildValidate(dir)
	restore()

	if err != nil {
		t.Fatalf("a vet-style lint finding in hand-written code must NOT fail generation "+
			"(the tree compiles); got: %v\n%s", err, errOutputOf(err))
	}
}

// errOutputOf digs the captured compiler/typecheck text out of a
// validateBuildError so assertions can read it.
func errOutputOf(err error) string {
	var ve *validateBuildError
	if errors.As(err, &ve) {
		return ve.Output
	}
	return ""
}
