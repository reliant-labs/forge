package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractMissingPkgSymbols pins the parse of `undefined: X` lines a
// probe build emits into the symbol list surfaced in the error message.
func TestExtractMissingPkgSymbols(t *testing.T) {
	out := `# example.com/app/.forge-pkgcompat-123
./probe.go:10:5: undefined: orm.UnknownFieldError
./probe.go:11:9: c.Dialect undefined (type orm.Context has no field or method Dialect)
./probe.go:12:5: undefined: orm.UnknownFieldError
`
	got := extractMissingPkgSymbols(out)
	if len(got) != 1 || got[0] != "orm.UnknownFieldError" {
		t.Fatalf("expected [orm.UnknownFieldError] (deduped), got %v", got)
	}
}

// TestCheckPkgCompat_NoForgePkgDependency is a no-op (nil) when the project
// doesn't depend on forge/pkg — nothing to probe, not our error to raise.
func TestCheckPkgCompat_NoForgePkgDependency(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	if err := checkPkgCompat(dir); err != nil {
		t.Fatalf("expected nil for a project without forge/pkg, got %v", err)
	}
}

// TestCheckPkgCompat_MissingSymbolsFailFast builds a self-contained module
// whose forge/pkg replace points at a STUB orm/crud that LACKS the symbols
// the generator emits (orm.Context.Dialect, orm.UnknownFieldError). The
// handshake must fail fast (before any codegen) and name the fix — the
// kalshi fr-ac69216583 scenario.
func TestCheckPkgCompat_MissingSymbolsFailFast(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module — skipped under -short")
	}
	dir := t.TempDir()
	writeStubForgePkg(t, dir, false /* withCompatSymbols */)

	err := checkPkgCompat(dir)
	if err == nil {
		t.Fatal("expected a compat error when forge/pkg lacks the emitted symbols")
	}
	msg := err.Error()
	if !strings.Contains(msg, "forge/pkg") || !strings.Contains(msg, "go get") {
		t.Errorf("error should name forge/pkg and the bump fix, got: %s", msg)
	}
	if !strings.Contains(msg, "No files were changed") {
		t.Errorf("error should reassure the tree is untouched, got: %s", msg)
	}
}

// TestCheckPkgCompat_PresentSymbolsPass is the mirror: a forge/pkg stub that
// DOES provide every emitted symbol passes the handshake.
func TestCheckPkgCompat_PresentSymbolsPass(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module — skipped under -short")
	}
	dir := t.TempDir()
	writeStubForgePkg(t, dir, true /* withCompatSymbols */)

	if err := checkPkgCompat(dir); err != nil {
		t.Fatalf("expected nil when forge/pkg provides every emitted symbol, got %v", err)
	}
}

// writeStubForgePkg lays down a minimal project module that requires
// forge/pkg via a local replace pointing at a hand-written stub providing
// (or omitting) the compat symbols. No network, no real forge/pkg.
func writeStubForgePkg(t *testing.T, dir string, withCompatSymbols bool) {
	t.Helper()

	for _, d := range []string{"forge-pkg-stub/orm", "forge-pkg-stub/crud"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Project module.
	mustWrite(t, filepath.Join(dir, "go.mod"), strings.Join([]string{
		"module example.com/app",
		"",
		"go 1.24",
		"",
		"require github.com/reliant-labs/forge/pkg v0.0.0",
		"",
		"replace github.com/reliant-labs/forge/pkg => ./forge-pkg-stub",
		"",
	}, "\n"))
	// A trivial root package so `go build ./<probe>` has a module to resolve in.
	mustWrite(t, filepath.Join(dir, "doc.go"), "package app\n")

	stub := filepath.Join(dir, "forge-pkg-stub")
	mustWrite(t, filepath.Join(stub, "go.mod"), "module github.com/reliant-labs/forge/pkg\n\ngo 1.24\n")

	// orm package: Context + Dialect type. The compat build references
	// orm.Context.Dialect() and orm.UnknownFieldError{}.
	ormSrc := "package orm\n\ntype Dialect interface{ Placeholder(i int) string }\n\ntype Context interface{ Bun() any }\n"
	if withCompatSymbols {
		ormSrc = "package orm\n\n" +
			"type Dialect interface{ Placeholder(i int) string }\n\n" +
			"type Context interface {\n\tBun() any\n\tDialect() Dialect\n}\n\n" +
			"type UnknownFieldError struct{ Field string }\n\n" +
			"func (e *UnknownFieldError) Error() string { return e.Field }\n"
	}
	mustWrite(t, filepath.Join(stub, "orm", "orm.go"), ormSrc)

	// crud package: Spec with (or without) the Timestamps/LegacyTextDeletedAt
	// fields the generator sets.
	crudSrc := "package crud\n\ntype Spec struct{}\n"
	if withCompatSymbols {
		crudSrc = "package crud\n\ntype Spec struct {\n\tTimestamps          bool\n\tLegacyTextDeletedAt bool\n}\n"
	}
	mustWrite(t, filepath.Join(stub, "crud", "crud.go"), crudSrc)
}
