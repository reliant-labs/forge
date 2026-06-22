package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/cliutil"
)

// forge/pkg compatibility handshake (kalshi fr-ac69216583).
//
// The generator's ORM/CRUD emitters reference a fixed set of symbols from
// the project's resolved forge/pkg module: orm.Context.Dialect(),
// orm.UnknownFieldError, and crud.Spec{Timestamps, LegacyTextDeletedAt}.
// A project that pins (or dev-vendors) a forge/pkg OLDER than this binary
// can lack those symbols. Without a handshake, `forge generate` rewrites
// the whole tree and only THEN fails its own `go build` validate with
// `undefined: orm.UnknownFieldError`, leaving the repo mid-regen (kalshi
// needed two manual ".forge-pkg re-sync" commits just to recover).
//
// pkgCompatProbe compiles a tiny throwaway program against the project's
// resolved forge/pkg BEFORE any codegen mutates the tree. If a required
// symbol is missing, generate aborts loudly with the version mismatch and
// the one-line fix, and the tree is untouched.
//
// The probe is the single source of truth for the binary↔library contract:
// when the generator starts emitting a new forge/pkg symbol, add it to
// requiredPkgSymbols and projects on a too-old pin fail fast here instead
// of deep in validate.

// pkgSymbolProbe is one symbol the generated code depends on, plus the
// snippet that exercises it in the probe program. exprFmt is a Go
// statement that references the symbol; it must compile iff the symbol
// exists in the resolved forge/pkg.
type pkgSymbolProbe struct {
	// imports the probe needs (import path → local alias; alias "" means
	// the default package name).
	imports map[string]string
	// stmt is a statement placed in the probe's func body. It must
	// reference the symbol such that a missing symbol yields `undefined:`.
	stmt string
}

// requiredPkgSymbols is the set of forge/pkg symbols THIS binary's
// generator emits into a project. Each must exist in the project's
// resolved forge/pkg or the generated code won't compile. Keep this in
// lockstep with the ORM/CRUD emitters (internal/generator/plan_orm_gen.go,
// pkg/crud).
func requiredPkgSymbols() []pkgSymbolProbe {
	orm := map[string]string{"github.com/reliant-labs/forge/pkg/orm": ""}
	crud := map[string]string{"github.com/reliant-labs/forge/pkg/crud": ""}
	return []pkgSymbolProbe{
		// orm.Context.Dialect() — the raw-SQL escape hatch in user-owned
		// *_repo_ext.go handlers (kalshi fr-3c3f470f2c).
		{imports: orm, stmt: "var c orm.Context; _ = func() orm.Dialect { return c.Dialect() }"},
		// orm.UnknownFieldError — emitted in the Update<Entity>Masked
		// doc-comment + returned by crud.UpdateMasked (kalshi fr-ac69216583).
		{imports: orm, stmt: "_ = orm.UnknownFieldError{}"},
		// crud.Spec{Timestamps, LegacyTextDeletedAt} — the per-entity repo
		// spec the generator constructs.
		{imports: crud, stmt: "_ = crud.Spec{Timestamps: true, LegacyTextDeletedAt: true}"},
	}
}

// checkPkgCompat probes the project's resolved forge/pkg for the symbols
// the generator emits. Returns a user-facing error (tree untouched) when a
// symbol is missing, nil when the handshake passes or can't be performed
// (no forge/pkg dependency, or the toolchain is unavailable — generate's
// existing validate step is the backstop in that case).
func checkPkgCompat(projectDir string) error {
	// Only meaningful when the project depends on forge/pkg at all. A
	// project with no go.mod, or one that doesn't require forge/pkg, emits
	// no code that references these symbols.
	gomod := filepath.Join(projectDir, "go.mod")
	data, err := os.ReadFile(gomod)
	if err != nil {
		return nil // no module → nothing to probe; not our error to raise
	}
	if !strings.Contains(string(data), "github.com/reliant-labs/forge/pkg") {
		return nil
	}

	probes := requiredPkgSymbols()

	// One probe file referencing every required symbol — a single missing
	// symbol fails the build and we surface the version mismatch. Building
	// them together keeps the probe to one `go build` invocation.
	allImports := map[string]string{}
	var stmts []string
	for _, p := range probes {
		for path, alias := range p.imports {
			allImports[path] = alias
		}
		stmts = append(stmts, "\t"+p.stmt)
	}

	var b strings.Builder
	b.WriteString("//go:build forgepkgcompat\n\n")
	b.WriteString("package forgepkgcompat\n\n")
	b.WriteString("import (\n")
	for path, alias := range allImports {
		if alias == "" {
			fmt.Fprintf(&b, "\t%q\n", path)
		} else {
			fmt.Fprintf(&b, "\t%s %q\n", alias, path)
		}
	}
	b.WriteString(")\n\n")
	b.WriteString("func forgePkgCompatProbe() {\n")
	for _, s := range stmts {
		b.WriteString(s + "\n")
	}
	b.WriteString("}\n")

	// Land the probe in a temp package dir INSIDE the project so it
	// resolves forge/pkg through the project's module graph (its require +
	// any replace / .forge-pkg dev-vendor). The build tag keeps it out of
	// the normal package's compilation and `go build ./...`.
	probeDir, err := os.MkdirTemp(projectDir, ".forge-pkgcompat-")
	if err != nil {
		return nil // can't probe → defer to the validate backstop
	}
	defer os.RemoveAll(probeDir)
	probeFile := filepath.Join(probeDir, "probe.go")
	if err := os.WriteFile(probeFile, []byte(b.String()), 0o644); err != nil {
		return nil
	}

	cmd := exec.Command("go", "build", "-tags", "forgepkgcompat", "./"+filepath.Base(probeDir))
	cmd.Dir = projectDir
	out, buildErr := cmd.CombinedOutput()
	if buildErr == nil {
		return nil // handshake passed
	}

	// Only an `undefined:`/missing-symbol failure is a compat problem. A
	// build error from a broken module graph, missing go.sum entries, etc.
	// is not the binary↔library contract — let the normal pipeline (and
	// its richer diagnostics) handle those rather than mis-attributing.
	outStr := string(out)
	if !strings.Contains(outStr, "undefined:") &&
		!strings.Contains(outStr, "not a type") &&
		!strings.Contains(outStr, "unknown field") &&
		!strings.Contains(outStr, "too many arguments") &&
		!strings.Contains(outStr, "has no field or method") {
		return nil
	}

	missing := extractMissingPkgSymbols(outStr)
	detail := "the resolved forge/pkg is missing symbol(s) this forge binary's generated code requires"
	if len(missing) > 0 {
		detail = fmt.Sprintf("the resolved forge/pkg is missing: %s", strings.Join(missing, ", "))
	}
	return cliutil.UserErr("forge generate (forge/pkg compatibility handshake)",
		detail+" — generating would rewrite the tree and then fail its own validate. No files were changed.",
		"",
		"bump the forge/pkg pin to match this binary: `go get github.com/reliant-labs/forge/pkg@latest && go mod tidy` "+
			"(and re-tidy gen/ if present). If you dev-vendor forge/pkg via .forge-pkg, re-sync it from a matching forge checkout. "+
			"Then re-run 'forge generate'.")
}

// extractMissingPkgSymbols pulls the forge/pkg symbols a probe build
// reported as undefined, for an actionable error message. Best-effort —
// returns nil if nothing recognizable was found.
func extractMissingPkgSymbols(buildOutput string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(buildOutput, "\n") {
		idx := strings.Index(line, "undefined: ")
		if idx < 0 {
			continue
		}
		sym := strings.TrimSpace(line[idx+len("undefined: "):])
		// Trim a trailing parenthesized note the compiler sometimes adds.
		if sp := strings.IndexAny(sym, " \t("); sp >= 0 {
			sym = sym[:sp]
		}
		if sym != "" && !seen[sym] {
			seen[sym] = true
			out = append(out, sym)
		}
	}
	return out
}
