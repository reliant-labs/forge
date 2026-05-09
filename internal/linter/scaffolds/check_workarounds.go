// Package scaffolds — `forge lint --check-workarounds` rule.
//
// Detects the canonical cross-lane workarounds that shipped to cpnext
// during the v0.2 rebuild (see FORGE_REVIEW_PROCESS.md §2 "Inline
// workarounds shipped in cpnext"). These are ad-hoc patterns that
// compile and ship clean but are real anti-patterns: they paper over a
// missing forge primitive rather than wait for it. Surfacing them as
// warnings turns the catch-bar from "will the next reviewer notice?"
// into "lint flags it before merge."
//
// All findings are SeverityWarning, not error — some workarounds are
// legitimate in some projects (a project might genuinely want a
// `cmd/maintenance.go` cli tool that's not modeled in forge.yaml). The
// warning includes a "should be:" pointer so the reader knows the
// canonical replacement once the corresponding forge primitive ships.
//
// Three rules today, each tracking a workaround actually shipped to
// cpnext:
//
//  1. `wireCastHelpers` — flags `castXxxRepo`-shaped functions in
//     `pkg/app/wire_gen.go`, the cross-lane `any`→typed bridge that
//     T2-D shipped (FORGE_REVIEW_PROCESS.md §2.1). R5-1's
//     `forge:placeholder` annotation eliminates the need.
//
//  2. `testingExtras` — flags `pkg/app/testing_extras.go` files, the
//     hand-rolled stub-repo workaround for the "scaffold-test factory
//     doesn't fill required Deps" gap (FORGE_REVIEW_PROCESS.md §2.2).
//     R5-2's auto-stubbing in `bootstrap_testing.go.tmpl` eliminates it.
//
//  3. `cmdNotInBinaries` — flags `cmd/<name>.go` files that aren't
//     declared in `forge.yaml`'s `binaries:` block. Today every
//     non-server second binary is hand-written (cpnext's
//     `cmd/workspace_proxy.go` is 270 LOC of cobra/k8s/signal-handling
//     boilerplate). R5-2's `forge add binary` command makes the
//     declaration explicit.
//
// Wiring: `runCheckWorkaroundsLint` invokes `LintWorkaroundsRoot` from
// the project root; `lint.go` registers the `--check-workarounds` flag
// and includes the rule in the default `forge lint` run.
package scaffolds

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// castHelperRE matches Go function declarations whose name follows the
// `cast<Name>` shape (T2-D's `castUserRepo` was the canonical instance).
// We deliberately do NOT match arbitrary lowercase-cast helpers — only
// the `cast<Pascal>` variant tied to the cross-lane bridge pattern.
//
// The receiver is unconstrained: `castUserRepo(...)` (free function) and
// `(s *App) castUserRepo(...)` (method) both fire. The `func ` literal
// keeps us off doc-comments and off string-literal references.
var castHelperRE = regexp.MustCompile(`(?m)^func\s+(?:\([^)]*\)\s+)?(cast[A-Z][A-Za-z0-9]*)\s*\(`)

// LintWorkaroundsRoot walks the project tree rooted at root and applies
// the three workaround-detection rules. Returns a Result whose Findings
// are all SeverityWarning.
//
// The walker reuses the same skipDir set as the scaffolds linter so we
// don't churn through `gen/`, `node_modules/`, or `vendor/`.
func LintWorkaroundsRoot(root string) (Result, error) {
	var result Result

	// Rule 2 is path-based — `pkg/app/testing_extras.go` is the canonical
	// site. Check explicitly rather than waiting for the walker to
	// stumble onto it; this also fires when the walker would otherwise
	// skip the directory (defensive).
	testingExtrasPath := filepath.Join(root, "pkg", "app", "testing_extras.go")
	if _, err := os.Stat(testingExtrasPath); err == nil {
		rel := relPath(testingExtrasPath, root)
		result.Findings = append(result.Findings, Finding{
			Rule:     "workaround-testing-extras",
			Severity: SeverityWarning,
			Path:     rel,
			Message: "pkg/app/testing_extras.go is a hand-rolled stub-repo file shipped to bridge `app.NewTest<Svc>` not filling required Deps " +
				"(FORGE_REVIEW_PROCESS.md §2.2). " +
				"Should be: remove once `bootstrap_testing.go.tmpl` auto-stubs interface-typed Deps; the testing factory will then construct realistic per-test deps without a hand-rolled bridge.",
		})
	}

	// Rule 3 needs the forge.yaml binaries: block AND the cmd/ dir.
	// Build the declared-binaries set first; if forge.yaml has no
	// binaries: block, the rule effectively reports every cmd/<name>.go
	// other than `server.go` (the canonical first binary).
	declaredBinaries := readDeclaredBinaries(filepath.Join(root, "forge.yaml"))

	cmdDir := filepath.Join(root, "cmd")
	if entries, err := os.ReadDir(cmdDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			if strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			base := strings.TrimSuffix(e.Name(), ".go")
			// `server.go` is the canonical first-binary forge ships.
			// `root.go`, `version.go`, and `_shared.go` style helpers
			// are also exempt — they're conventional cobra harness
			// scaffolding rather than independent binaries.
			if isExemptCmdFile(base) {
				continue
			}
			// Match either kebab (forge.yaml convention) or snake
			// (Go file convention). `workspace-proxy` in forge.yaml
			// pairs with `workspace_proxy.go` on disk; both should
			// satisfy the binaries: declaration.
			if declaredBinaries[base] || declaredBinaries[strings.ReplaceAll(base, "_", "-")] {
				continue
			}
			rel := relPath(filepath.Join(cmdDir, e.Name()), root)
			result.Findings = append(result.Findings, Finding{
				Rule:     "workaround-cmd-not-in-binaries",
				Severity: SeverityWarning,
				Path:     rel,
				Message: fmt.Sprintf(
					"cmd/%s is a hand-written second binary not declared in forge.yaml's `binaries:` block "+
						"(FORGE_REVIEW_PROCESS.md §2.4 workspace_proxy pattern). "+
						"Should be: declare in forge.yaml binaries: block (post-`forge add binary`), or remove if unused.",
					e.Name(),
				),
			})
		}
	}

	// Rule 1 is content-based — walk pkg/app/wire_gen.go and look for
	// `cast<Name>` function declarations. We could broaden this to all
	// wire-gen-adjacent files but the canonical site is the only one
	// that's shipped to cpnext, so we scope tightly to keep false
	// positives down.
	wireGenPath := filepath.Join(root, "pkg", "app", "wire_gen.go")
	if data, err := os.ReadFile(wireGenPath); err == nil {
		matches := castHelperRE.FindAllStringSubmatch(string(data), -1)
		seen := make(map[string]bool)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			name := m[1]
			if seen[name] {
				continue
			}
			seen[name] = true
			rel := relPath(wireGenPath, root)
			result.Findings = append(result.Findings, Finding{
				Rule:     "workaround-wire-cast-helper",
				Severity: SeverityWarning,
				Path:     rel,
				Message: fmt.Sprintf(
					"%s in pkg/app/wire_gen.go is a cross-lane `any`→typed bridge "+
						"(FORGE_REVIEW_PROCESS.md §2.1 castUserRepo pattern). "+
						"Should be: remove once `forge:placeholder` annotation lands; AppExtras fields can carry their final types directly without a cast helper.",
					name,
				),
			})
		}
	}

	// Belt-and-suspenders: also scan the entire tree for unexpected
	// matches of the castHelperRE pattern, in case a project has moved
	// the helper to a sibling file. We intentionally only walk
	// pkg/app/ — broader scopes turn up unrelated `cast` helpers in
	// internal packages.
	pkgAppDir := filepath.Join(root, "pkg", "app")
	_ = filepath.WalkDir(pkgAppDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // soft-skip per-file errors
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if path == wireGenPath {
			return nil // already handled above
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil //nolint:nilerr
		}
		matches := castHelperRE.FindAllStringSubmatch(string(data), -1)
		seen := make(map[string]bool)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			name := m[1]
			if seen[name] {
				continue
			}
			seen[name] = true
			result.Findings = append(result.Findings, Finding{
				Rule:     "workaround-wire-cast-helper",
				Severity: SeverityWarning,
				Path:     relPath(path, root),
				Message: fmt.Sprintf(
					"%s helper in pkg/app/ is a cross-lane `any`→typed bridge "+
						"(FORGE_REVIEW_PROCESS.md §2.1 castUserRepo pattern). "+
						"Should be: remove once `forge:placeholder` annotation lands; AppExtras fields can carry their final types directly without a cast helper.",
					name,
				),
			})
		}
		return nil
	})

	return result, nil
}

// isExemptCmdFile returns true for cmd/<name>.go files that conventionally
// belong to the cobra harness rather than to an independent binary.
// Keep this list synced with the forge templates that emit cmd/* —
// `forge new --kind service` ships server.go, db.go, and otel.go, all
// of which are generated cobra subcommands rather than independent
// second-binary scaffolds.
//
//   - server.go: forge's canonical first-binary scaffold
//   - db.go: cobra `<binary> db migrate ...` subcommand (Tier-1 from
//     internal/templates/project/cmd-db.go.tmpl)
//   - otel.go: OpenTelemetry init shim shared by all binaries
//   - root.go, version.go: cobra root + version subcommand
//   - main.go: cobra harness entry
//   - <name>_shared.go suffix: shared helper file
func isExemptCmdFile(base string) bool {
	switch base {
	case "server", "root", "version", "main", "db", "otel":
		return true
	}
	return strings.HasSuffix(base, "_shared")
}

// readDeclaredBinaries returns the set of binary names declared in
// forge.yaml's `binaries:` block. Until R5-2 ships `forge add binary`,
// the block is empty in every project — i.e. the rule fires on every
// non-`server.go` cmd file. That's fine: the warning surfaces the gap.
//
// We parse forge.yaml manually to avoid pulling the full config package
// into the linter's dependency graph; the binaries block is shallow
// enough that a line-oriented reader works.
func readDeclaredBinaries(path string) map[string]bool {
	out := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inBinaries := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		// New top-level key resets the in-block flag.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "-") && trimmed != "" {
			inBinaries = strings.HasPrefix(trimmed, "binaries:")
			continue
		}
		if !inBinaries {
			continue
		}
		// Look for `- name: <name>` in the binaries: block.
		if strings.HasPrefix(trimmed, "- name:") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				name := strings.TrimSpace(parts[1])
				name = strings.Trim(name, `"'`)
				if name != "" {
					out[name] = true
				}
			}
			continue
		}
	}
	return out
}
