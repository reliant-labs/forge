package lint

// enforce-component-observe lint — the discoverability forcing-function for
// in-process component observability.
//
// Phase-2 of the universal component-middleware feature makes the
// `// forge:constructor` marker the opt-in signal for a component's generated
// observability decorator, and `// forge:no-observe` the opt-out. Neither is
// required to compile, so without a gate a component would silently default to
// "no decorator" — the exact silent-default this feature exists to eliminate.
//
// This lint fires when a package
//
//   - has a Service-contract interface, AND
//   - has a canonical constructor that RETURNS that interface
//     (New(Deps) Service — handler packages return a concrete *Service and are
//     edge-instrumented by otelconnect, so they are out of scope), AND
//   - is wired as a component (a contract-shaped internal package), BUT
//   - has made NO observability decision — NEITHER `// forge:constructor`
//     NOR `// forge:no-observe`.
//
// It aggregates ALL such packages into ONE error naming every one plus the
// three escapes (opt in / opt a package out / disable the check). The
// suggestion per package is I/O-aware: a component whose dep closure touches a
// DB / outbound / client / HTTP type is nudged toward `// forge:constructor`
// (observe it); a pure-compute component toward `// forge:no-observe`.
//
// Scope exclusions: handler packages (internal/handlers/*, concrete *Service),
// `//forge:external-component` (hand-wired), `//forge:exclude-contract`,
// testdata fixtures, and test-only packages (no Service interface in non-test
// source). Kill-switch: config.enforce_component_observe: off.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
)

// observeComponent is one wired component that has made no observability
// decision, plus the I/O-aware suggestion for which marker to add.
type observeComponent struct {
	// Name is the leaf package/dir name (e.g. "payments") — the concise label
	// used in the aggregated headline list.
	Name string
	// Rel is the module-relative package directory (e.g. "internal/payments").
	Rel string
	// NewFileRel is the module-relative file that declares `func New`, or the
	// package dir when it cannot be located — where the marker should be added.
	NewFileRel string
	// SuggestConstructor is true when the dep closure touches a DB / outbound /
	// client / HTTP type (→ suggest `// forge:constructor`); false for a
	// pure-compute component (→ suggest `// forge:no-observe`).
	SuggestConstructor bool
}

// scanUndecidedObserveComponents walks internal/ under projectDir and returns
// every wired component that has made no observability decision, in stable
// (leaf-name) order. A missing internal/ dir yields an empty slice.
func scanUndecidedObserveComponents(projectDir string) ([]observeComponent, error) {
	internalDir := filepath.Join(projectDir, "internal")
	if !dirExists(internalDir) {
		return nil, nil
	}

	handlersDir := filepath.Join(internalDir, "handlers")

	var out []observeComponent
	walkErr := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		// Handler packages are edge-instrumented (otelconnect owns the RPC
		// edge) and return a concrete *Service — out of scope. Skip the whole
		// subtree. testdata subtrees hold fixtures, not real packages.
		if path == handlersDir {
			return filepath.SkipDir
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}

		// Must be contract-shaped.
		if _, statErr := os.Stat(filepath.Join(path, "contract.go")); statErr != nil {
			return nil //nolint:nilerr // no contract.go → not a component dir
		}

		// Excluded from codegen / hand-wired — no forge-owned decorator applies.
		if codegen.HasExcludeContractDirective(path) || codegen.HasExternalComponentDirective(path) {
			return nil
		}

		// A Service-contract interface must exist (canonical `Service` or a
		// `//forge:service`/`//forge:contract`-marked interface). Empty → the
		// package is not component-shaped (or only declares it in test files).
		ifaceName := codegen.DetectServiceInterfaceName(path)
		if ifaceName == "" {
			return nil
		}

		// The constructor must RETURN that interface. A handler-shaped New
		// returning *Service (ctorType != ifaceName) is edge-instrumented and
		// out of scope; a package with no parseable New is not wired.
		ctorType, _ := codegen.DetectConstructorType(path)
		if ctorType != ifaceName {
			return nil
		}

		// Decision made? Either marker present → nothing to flag.
		if codegen.HasConstructorMarker(path) || codegen.HasPackageNoObserveDirective(path) {
			return nil
		}

		rel, relErr := filepath.Rel(projectDir, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		out = append(out, observeComponent{
			Name:               filepath.Base(rel),
			Rel:                rel,
			NewFileRel:         constructorFileRel(projectDir, path, rel),
			SuggestConstructor: componentTouchesIO(projectDir, path, map[string]bool{}),
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Rel < out[j].Rel
	})
	return out, nil
}

// constructorFileRel returns the module-relative path of the non-test .go file
// in dir that declares `func New`, or the package dir (dirRel) when it cannot
// be located — used to point the suggestion at the exact edit site.
func constructorFileRel(projectDir, dir, dirRel string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dirRel + "/"
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		if strings.Contains(string(data), "func New(") {
			if rel, relErr := filepath.Rel(projectDir, filepath.Join(dir, name)); relErr == nil {
				return filepath.ToSlash(rel)
			}
			return dirRel + "/" + name
		}
	}
	return dirRel + "/"
}

// componentTouchesIO reports whether the component at dir has a dependency
// closure that touches a DB / outbound-boundary / client / HTTP type. It
// inspects the package's own Deps and follows one level into any internal
// package a Deps field references, bounded by visited to stop at cycles. A
// package explicitly marked `//forge:outbound-io` does I/O by definition.
// The conservative default — no I/O signal found — is pure-compute
// (suggest opt-out).
func componentTouchesIO(projectDir, dir string, visited map[string]bool) bool {
	if visited[dir] {
		return false
	}
	visited[dir] = true

	// A declared outbound boundary does I/O by definition.
	if codegen.HasOutboundIODirective(dir) {
		return true
	}
	// A DB dependency (orm.Context on Deps).
	if db, _ := codegen.DetectDepsDBField(dir); db {
		return true
	}

	deps, _ := codegen.ParseServiceDeps(dir)
	for _, d := range deps {
		if depTypeIsIO(d.Type) {
			return true
		}
		// One level of closure: a dep typed `<pkg>.<Name>` referencing another
		// internal package — follow it (its own Deps / outbound-io marker decide).
		if pkg := selectorPkgName(d.Type); pkg != "" {
			if refDir := resolveInternalPkgDir(projectDir, pkg); refDir != "" {
				if componentTouchesIO(projectDir, refDir, visited) {
					return true
				}
			}
		}
	}
	return false
}

// ioTypeNeedles are the DB / HTTP / client type fragments that mark a Deps
// field as an I/O dependency. Matched as substrings of the pretty-printed Go
// type (pointer / slice prefixes are irrelevant to the match).
var ioTypeNeedles = []string{
	"orm.Context", "sql.DB", "sql.Tx", "sqlx.", "pgx.", "pgxpool.",
	"http.Client", "http.RoundTripper", "grpc.ClientConn",
	"redis.", "mongo.", "kafka.", "amqp.", "nats.", "s3.",
}

// depTypeIsIO reports whether a Deps field type names a DB / HTTP / client
// primitive.
func depTypeIsIO(t string) bool {
	for _, needle := range ioTypeNeedles {
		if strings.Contains(t, needle) {
			return true
		}
	}
	return false
}

// selectorPkgName returns the package identifier of a `<pkg>.<Name>` selector
// type (after stripping leading `*` / `[]`), or "" when the type is not a
// package-qualified named type. e.g. "*billing.Gateway" → "billing".
func selectorPkgName(t string) string {
	t = strings.TrimLeft(t, "*[]")
	dot := strings.IndexByte(t, '.')
	if dot <= 0 {
		return ""
	}
	pkg := t[:dot]
	rest := t[dot+1:]
	// pkg must be a lowercase-leading identifier; the member must be an
	// exported (uppercase-leading) type name.
	if !isLowerIdent(pkg) || rest == "" || rest[0] < 'A' || rest[0] > 'Z' {
		return ""
	}
	return pkg
}

func isLowerIdent(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// resolveInternalPkgDir returns the directory of an internal package whose leaf
// name is pkg (and which contains a contract.go), or "" when none is found.
// Bounded shallow walk of internal/.
func resolveInternalPkgDir(projectDir, pkg string) string {
	internalDir := filepath.Join(projectDir, "internal")
	found := ""
	_ = filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // best-effort resolution
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if found != "" {
			return filepath.SkipDir
		}
		if filepath.Base(path) == pkg {
			if _, statErr := os.Stat(filepath.Join(path, "contract.go")); statErr == nil {
				found = path
				return filepath.SkipDir
			}
		}
		return nil
	})
	return found
}

// componentObserveError builds the ONE aggregated, build-gating error naming
// every undecided component, an I/O-aware per-component suggestion, and the
// three escapes. The precondition is len(comps) > 0.
func componentObserveError(comps []observeComponent) error {
	var b strings.Builder
	writeComponentObserveReport(&b, comps)
	return errors.New(b.String())
}

// writeComponentObserveReport writes the aggregated report (headline + per-
// component suggestion + the three-escape footer) to w. Shared by the error
// text and the human `forge lint` output so they stay identical.
func writeComponentObserveReport(w io.Writer, comps []observeComponent) {
	names := make([]string, 0, len(comps))
	for _, c := range comps {
		names = append(names, c.Name)
	}
	fmt.Fprintf(w, "%d component(s) have no observability decision: %s.\n", len(comps), strings.Join(names, ", "))
	fmt.Fprintln(w, "Each wired component (a Service interface + a New(Deps) Service constructor) must decide whether its in-process calls are observed:")
	for _, c := range comps {
		if c.SuggestConstructor {
			fmt.Fprintf(w, "  - %s: deps touch I/O (DB/outbound-io/client/HTTP) — add `// forge:constructor` to func New in %s\n", c.Name, c.NewFileRel)
		} else {
			fmt.Fprintf(w, "  - %s: pure compute (only Logger/Config/pure deps) — add `// forge:no-observe` to func New in %s\n", c.Name, c.NewFileRel)
		}
	}
	fmt.Fprint(w, "Opt in with `// forge:constructor`, opt a package out with `// forge:no-observe`, "+
		"or disable this check with `config.enforce_component_observe: off`.")
}

// runEnforceComponentObserveLint scans the project for wired components with no
// observability decision and, when any exist, prints the aggregated report and
// returns the ONE gating error. A clean project prints a success line and
// returns nil.
func runEnforceComponentObserveLint(projectDir string) error {
	fmt.Println("Running enforce-component-observe lint...")
	comps, err := scanUndecidedObserveComponents(projectDir)
	if err != nil {
		return fmt.Errorf("scan components: %w", err)
	}
	if len(comps) == 0 {
		fmt.Println("  every wired component has an observability decision")
		return nil
	}
	var b strings.Builder
	writeComponentObserveReport(&b, comps)
	fmt.Println()
	fmt.Println(b.String())
	return componentObserveError(comps)
}

// collectEnforceComponentObserveJSON is the JSON-mode collector: one error
// finding per undecided component (so tools can locate each), gated true when
// any exist. The three escapes ride along in each finding's fix hint.
func collectEnforceComponentObserveJSON(projectDir string) ([]lintJSONFinding, bool, error) {
	comps, err := scanUndecidedObserveComponents(projectDir)
	if err != nil {
		return nil, false, err
	}
	const escapes = "opt in with `// forge:constructor`, opt out with `// forge:no-observe`, " +
		"or disable via config.enforce_component_observe: off"
	var findings []lintJSONFinding
	for _, c := range comps {
		suggest := "// forge:no-observe (pure compute — only Logger/Config/pure deps)"
		if c.SuggestConstructor {
			suggest = "// forge:constructor (deps touch I/O — DB/outbound-io/client/HTTP)"
		}
		findings = append(findings, lintJSONFinding{
			File:     c.NewFileRel,
			Severity: lintSevError,
			Rule:     "forge-enforce-component-observe",
			Message: fmt.Sprintf("component %q has no observability decision (neither // forge:constructor nor // forge:no-observe); suggest %s",
				c.Name, suggest),
			FixHint: escapes,
		})
	}
	return findings, len(findings) > 0, nil
}
