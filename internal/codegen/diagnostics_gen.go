package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

// diagnostics_gen.go — codegen for pkg/app/diagnostics_gen.go.
//
// The runtime library lives at pkg/diagnostics: a Registry that
// codegen-emitted init() targets, an Emitter the bootstrap calls at
// boot. This file builds the bridge between the codegen-time signals
// (nil-wired Deps fields, Tier-1 stubs returning ErrNotImplemented) and
// the runtime registration that bootstrap emits.
//
// We emit one pkg/app/diagnostics_gen.go per project. Its init() makes
// one diagnostics.Default.RegisterStub / RegisterNilDep call per
// detected issue. The file is regenerated on every `forge generate`;
// an empty file (no Register calls inside init()) is the clean-scaffold
// signal — checked into VCS so `forge audit` and reviewers can both
// observe that the project currently has zero unwired scaffolds.
//
// Detection sources:
//
//   - Nil-dep entries: passed in by the caller (typically the same
//     wire_gen pass that computed WireUnresolved). Mirrors the
//     UnresolvedFields header comment block — anything that wire_gen
//     would have called out to the developer is now also called out to
//     the operator at boot.
//
//   - Stub-impl entries: scanned from handlers/<svc>/{handlers.go,
//     handlers_gen.go} for the `// forge:gen unwired-stub` marker the
//     handler templates emit alongside each Unimplemented stub. The
//     marker carries the canonical <pkg>.<Name> symbol so the
//     registration is self-describing.

// DiagnosticEntry is one record the diagnostics_gen.go init() will
// register. The shape mirrors pkg/diagnostics.Diagnostic minus runtime
// fields (Message, Severity — derived from Kind by the runtime).
type DiagnosticEntry struct {
	// Kind is "stub-impl" or "nil-dep". String here (instead of an enum)
	// so callers don't have to import pkg/diagnostics — the codegen
	// package stays free of runtime-library dependencies.
	Kind string

	// Symbol is the canonical Go package.identifier for the diagnostic.
	// For stub-impl: "<svc-pkg>.<MethodName>". For nil-dep:
	// "<wireFunc>.<DepField>".
	Symbol string

	// File is the project-relative path of the codegen-emitted source
	// the diagnostic points at (forward slashes regardless of OS).
	File string

	// Line is the 1-indexed line number of the marker / unresolved
	// field in File. Best-effort; 0 is acceptable when the codegen
	// doesn't track lines.
	Line int

	// Component is the enclosing wire_gen function name for nil-dep
	// entries (e.g. "wireWorkerCalibratorRefitDeps"). Empty for
	// stub-impl.
	Component string

	// DepName is the Deps field name for nil-dep entries (e.g.
	// "PgUnsettled"). Empty for stub-impl.
	DepName string
}

// stubMarker is the literal codegen marker the handler templates emit
// above every Unimplemented stub body. The carve-out shape is
// `// forge:gen unwired-stub symbol=<pkg>.<Name>` — line-based,
// matches the existing `forge:<area> <subkind>` convention used
// elsewhere in the repo (e.g. forge:scaffold, forge:placeholder).
const stubMarker = "// forge:gen unwired-stub"

// GenerateDiagnostics emits pkg/app/diagnostics_gen.go for the
// project, combining (1) the nil-dep entries the caller supplies
// (computed alongside wire_gen) and (2) the stub-impl entries scanned
// from handlers/<svc>/{handlers.go,handlers_gen.go} for the
// `// forge:gen unwired-stub` marker.
//
// Returns nil with no file written when the project has no services
// AND no workers AND no operators — there's no pkg/app/ in that case
// (matches GenerateWireGen's guard).
//
// The emitted file always declares package app + the init() function,
// even when there are zero entries — an empty body is the clean-
// scaffold signal that downstream consumers (forge audit, code
// review) rely on. The file is registered in the checksum manifest so
// `forge audit`'s orphan check leaves it alone.
func GenerateDiagnostics(services []ServiceDef, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, projectDir string, nilDeps []DiagnosticEntry, cs *checksums.FileChecksums) error {
	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}
	_ = modulePath // reserved for future template parameterization

	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}

	// Scan per-service handler files for the stub marker. We walk
	// handlers/<pkg>/ for each registered service and grep every .go
	// file for the marker — cheap, deterministic, and survives users
	// renaming the file the marker sits in. We skip _test.go files
	// (test scaffolds are not production traffic).
	var stubEntries []DiagnosticEntry
	for _, svc := range services {
		// Disk-first: locate the REAL handler dir (snake/compact/kebab —
		// whatever is on disk) instead of re-synthesizing it from the
		// proto name. A resolver error (broken package clauses) downgrades
		// to the same best-effort warning as a Deps parse failure below —
		// the bootstrap/wire generators already failed loudly on it.
		res, resErr := ResolveServiceComponent(projectDir, svc.Name)
		if resErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: resolving handler dir for %s: %v\n", svc.Name, resErr)
			continue
		}
		pkg := res.PackageName
		dir := res.Dir
		entries, err := scanStubMarkers(dir, pkg, projectDir)
		if err != nil {
			// Intentional soft warning (no --strict promotion): a parse
			// failure here means the handler dir has issues already
			// surfaced by the regular Go compile. Don't fail
			// diagnostics — write what we have and move on. Lives in
			// internal/codegen so no pipelineContext reach.
			fmt.Fprintf(os.Stderr, "Warning: scanning %s for stub markers: %v\n", dir, err)
			continue
		}
		stubEntries = append(stubEntries, entries...)
	}

	// Merge and sort for deterministic output. Sort by (Kind, Symbol)
	// so the rendered file is stable across regenerates regardless of
	// scan order or map iteration.
	allEntries := make([]DiagnosticEntry, 0, len(stubEntries)+len(nilDeps))
	allEntries = append(allEntries, stubEntries...)
	allEntries = append(allEntries, nilDeps...)
	sort.SliceStable(allEntries, func(i, j int) bool {
		if allEntries[i].Kind != allEntries[j].Kind {
			return allEntries[i].Kind < allEntries[j].Kind
		}
		return allEntries[i].Symbol < allEntries[j].Symbol
	})

	content := renderDiagnosticsFile(allEntries)

	relPath := filepath.Join("pkg", "app", "diagnostics_gen.go")
	if _, err := checksums.WriteGeneratedFile(projectDir, relPath, []byte(content), cs, true); err != nil {
		return fmt.Errorf("write pkg/app/diagnostics_gen.go: %w", err)
	}
	return nil
}

// renderDiagnosticsFile builds the diagnostics_gen.go body. Hand-rolled
// (no template) so the codegen stays understandable and the
// dependency on internal/templates is one fewer for this file. The
// output is gofmt-compatible — `forge generate` runs gofmt on every
// generated file already.
func renderDiagnosticsFile(entries []DiagnosticEntry) string {
	var b strings.Builder
	b.WriteString("// Code generated by forge. DO NOT EDIT.\n")
	b.WriteString("// Source: handlers/<svc>/*.go (forge:gen unwired-stub markers)\n")
	b.WriteString("//         + pkg/app/wire_gen.go (nil-dep wire entries).\n")
	b.WriteString("//\n")
	b.WriteString("// This file registers every unwired scaffold the codegen pipeline\n")
	b.WriteString("// detected at generate time. Bootstrap reads the registry at boot\n")
	b.WriteString("// and emits one structured slog line per entry when\n")
	b.WriteString("// `features.diagnostics: true` (or `features.strict_wiring: true`).\n")
	b.WriteString("//\n")
	b.WriteString("// An empty init() body is the clean-scaffold signal — committed\n")
	b.WriteString("// to VCS so reviewers can see at a glance that the project\n")
	b.WriteString("// currently has zero unwired scaffolds.\n")
	b.WriteString("package app\n\n")

	if len(entries) == 0 {
		// Even with no entries we still emit the import + init() so the
		// file is uniformly shaped — diff between "clean" and "dirty"
		// projects is just the body of init(). Using `_` as the import
		// alias keeps "imported and not used" out of the way when there
		// are zero register calls.
		b.WriteString("import _ \"github.com/reliant-labs/forge/pkg/diagnostics\"\n\n")
		b.WriteString("func init() {\n")
		b.WriteString("\t// No unwired scaffolds detected at last `forge generate`.\n")
		b.WriteString("}\n")
		return b.String()
	}

	b.WriteString("import \"github.com/reliant-labs/forge/pkg/diagnostics\"\n\n")
	b.WriteString("func init() {\n")
	for _, e := range entries {
		switch e.Kind {
		case "stub-impl":
			fmt.Fprintf(&b, "\tdiagnostics.Default.RegisterStub(%q, %q, %d)\n",
				e.Symbol, e.File, e.Line)
		case "nil-dep":
			fmt.Fprintf(&b, "\tdiagnostics.Default.RegisterNilDep(%q, %q, %q, %d)\n",
				e.Component, e.DepName, e.File, e.Line)
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// scanStubMarkers walks handlerDir for non-test .go files and collects
// one DiagnosticEntry per `// forge:gen unwired-stub` marker. The
// marker line carries the symbol (e.g.
// `// forge:gen unwired-stub symbol=admin.Login`); the line number is
// recorded for the audit / lint surface.
//
// Returns an empty slice (not nil) so the caller can append
// unconditionally. Missing directory is not an error — a freshly
// generated project may not have handlers/ yet.
func scanStubMarkers(handlerDir string, fallbackPkg string, projectDir string) ([]DiagnosticEntry, error) {
	info, err := os.Stat(handlerDir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var out []DiagnosticEntry
	entries, err := os.ReadDir(handlerDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		full := filepath.Join(handlerDir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		rel, relErr := filepath.Rel(projectDir, full)
		if relErr != nil {
			rel = full
		}
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, stubMarker) {
				continue
			}
			symbol := extractStubSymbol(trimmed, fallbackPkg)
			if symbol == "" {
				continue
			}
			out = append(out, DiagnosticEntry{
				Kind:   "stub-impl",
				Symbol: symbol,
				File:   rel,
				Line:   i + 1,
			})
		}
	}
	// Stable order per directory so multiple files in the same package
	// render deterministically.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

// NilDepEntriesFromWireData converts the UnresolvedFields the wire_gen
// pass already computes into DiagnosticEntry records the diagnostics
// codegen consumes. Called by the generator pipeline after
// GenerateWireGen and before GenerateDiagnostics so the same
// resolution map drives both the developer-facing UNRESOLVED header
// and the operator-facing boot warning.
//
// wireFuncPrefix is "wire" for services, "wireWorker" for workers,
// "wireOperator" for operators — matches the template's function-name
// conventions so the registered Component matches the runtime symbol
// the wire_gen function actually emits.
//
// The File field is hardcoded to "pkg/app/wire_gen.go" because that's
// where the unresolved field gets emitted as a typed-zero assignment.
// Line is 0 — we don't track line numbers across the template render
// boundary, and the runtime emitter handles 0 gracefully.
func NilDepEntriesFromWireData(wireData []WireGenServiceData, wireFuncPrefix string) []DiagnosticEntry {
	if len(wireData) == 0 {
		return nil
	}
	var out []DiagnosticEntry
	for _, d := range wireData {
		if len(d.UnresolvedFields) == 0 {
			continue
		}
		component := wireFuncPrefix + d.FieldName + "Deps"
		for _, uf := range d.UnresolvedFields {
			out = append(out, DiagnosticEntry{
				Kind:      "nil-dep",
				Symbol:    component + "." + uf.Name,
				File:      "pkg/app/wire_gen.go",
				Line:      0,
				Component: component,
				DepName:   uf.Name,
			})
		}
	}
	return out
}

// extractStubSymbol parses the symbol= attribute out of a stub-marker
// line. Accepts either `symbol=<value>` or no attribute (returns
// fallbackPkg + "." + "Unknown" when the symbol is missing — gives the
// audit something to point at instead of dropping the entry). Trailing
// whitespace and comma-separated extras are tolerated for future-
// proofing.
func extractStubSymbol(line, fallbackPkg string) string {
	// Strip the marker prefix; remainder is "symbol=foo.Bar [extras...]".
	rest := strings.TrimSpace(strings.TrimPrefix(line, stubMarker))
	if rest == "" {
		return ""
	}
	for _, tok := range strings.Fields(rest) {
		if !strings.HasPrefix(tok, "symbol=") {
			continue
		}
		val := strings.TrimPrefix(tok, "symbol=")
		val = strings.Trim(val, `"`)
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}
		// If the symbol is bare (no dot), qualify with the directory
		// package name so the diagnostic is still locatable.
		if !strings.Contains(val, ".") && fallbackPkg != "" {
			val = fallbackPkg + "." + val
		}
		return val
	}
	return ""
}
