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

	// Rule 4 is dev-vendor specific — a project in "dev-vendor mode" has
	// `go.mod` with `replace github.com/reliant-labs/forge/pkg =>
	// ./.forge-pkg` plus a vendored `.forge-pkg/` directory. The generated
	// Dockerfile MUST `COPY .forge-pkg/` before `RUN go mod download`, else
	// the in-container `go mod download` fails with
	// `reading .forge-pkg/go.mod: no such file or directory`. The Dockerfile
	// is a Tier-2 scaffold-once file that `forge generate` does NOT
	// re-render, so projects scaffolded before the COPY-line template
	// feature carry a silently-broken Docker build. Surface it as a warning
	// so the user runs `forge upgrade` (or hand-adds the line).
	if finding, ok := lintDevVendorDockerfile(root); ok {
		result.Findings = append(result.Findings, finding)
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
			return nil //nolint:nilerr // unreadable file is treated as no findings; walk continues
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

// readDeclaredBinaries returns the set of binary-kind component names
// declared in forge.yaml's `components:` block (entries with
// `kind: binary`). Binaries used to live in a dedicated `binaries:`
// block; the component-model unification folded them into `components:`
// keyed on `kind:`.
//
// We parse forge.yaml manually to avoid pulling the full config package
// into the linter's dependency graph. The components block is shallow
// enough that a line-oriented reader works: we walk each `- name:`
// entry, remember its name, and emit it only once we see a sibling
// `kind: binary` line within the same entry.
func readDeclaredBinaries(path string) map[string]bool {
	out := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	inComponents := false
	curName := ""
	curKind := ""
	flush := func() {
		if curName != "" && curKind == "binary" {
			out[curName] = true
		}
		curName = ""
		curKind = ""
	}
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		// New top-level key resets the in-block flag (and flushes any
		// pending entry from the components block we're leaving).
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "-") && trimmed != "" {
			flush()
			inComponents = strings.HasPrefix(trimmed, "components:")
			continue
		}
		if !inComponents {
			continue
		}
		// A new list element starts a new entry — flush the previous one.
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			flush()
		}
		if name, ok := scalarYAMLValue(trimmed, "name"); ok {
			curName = name
			continue
		}
		if kind, ok := scalarYAMLValue(trimmed, "kind"); ok {
			curKind = kind
			continue
		}
	}
	flush()
	return out
}

// devVendorReplaceRE matches the go.mod replace directive that puts a
// project into dev-vendor mode: the forge/pkg module redirected at the
// local `./.forge-pkg` checkout. Whitespace around `=>` is flexible
// (gofmt aligns the column, but hand-edits vary); a leading `./` is
// optional so both `=> ./.forge-pkg` and `=> .forge-pkg` match.
var devVendorReplaceRE = regexp.MustCompile(
	`replace\s+github\.com/reliant-labs/forge/pkg\s+=>\s+\.?/?\.forge-pkg\b`)

// dockerfileCopyForgePkgRE matches the `COPY .forge-pkg/ ./.forge-pkg/`
// line the Dockerfile template emits for dev-vendor projects. We accept
// `COPY .forge-pkg/` and `COPY .forge-pkg ` (trailing space, no slash)
// so a hand-added variant still satisfies the rule.
var dockerfileCopyForgePkgRE = regexp.MustCompile(`(?m)^\s*COPY\s+\.forge-pkg[/ ]`)

// lintDevVendorDockerfile reports the stale-dev-vendor-Dockerfile
// workaround: the project is in dev-vendor mode (go.mod replace targeting
// ./.forge-pkg, or a `.forge-pkg/go.mod` on disk) but its Dockerfile
// lacks the `COPY .forge-pkg/` line that the in-container `go mod
// download` needs. Returns ok=false when the project is not in dev-vendor
// mode, has no Dockerfile, or the Dockerfile already carries the COPY line.
func lintDevVendorDockerfile(root string) (Finding, bool) {
	if !isDevVendorMode(root) {
		return Finding{}, false
	}
	dockerfilePath := filepath.Join(root, "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		// No Dockerfile (or unreadable) — nothing to warn about. A
		// dev-vendor project without a Dockerfile isn't building an image.
		return Finding{}, false
	}
	if dockerfileCopyForgePkgRE.Match(data) {
		return Finding{}, false
	}
	return Finding{
		Rule:     "workaround-dev-vendor-dockerfile",
		Severity: SeverityWarning,
		Path:     relPath(dockerfilePath, root),
		Message: "Dockerfile is missing a `COPY .forge-pkg/` line but go.mod vendors forge/pkg at ./.forge-pkg " +
			"(dev-vendor mode). The in-container `go mod download` will fail with " +
			"`reading .forge-pkg/go.mod: no such file or directory`. " +
			"Should be: add `COPY .forge-pkg/ ./.forge-pkg/` before `RUN go mod download`, or run `forge upgrade` to re-render the Dockerfile.",
	}, true
}

// isDevVendorMode reports whether the project rooted at root vendors
// forge/pkg locally. Two independent signals, either sufficient: a
// `.forge-pkg/go.mod` on disk (matches the generator's detection at
// internal/generator/upgrade.go), or a go.mod replace directive pointing
// at ./.forge-pkg.
func isDevVendorMode(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".forge-pkg", "go.mod")); err == nil {
		return true
	}
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return false
	}
	return devVendorReplaceRE.Match(data)
}

// scalarYAMLValue extracts the value of `key` from a line shaped like
// `key: value` or `- key: value` (the first key of a list element),
// stripping quotes. Returns ok=false when the line isn't that key.
func scalarYAMLValue(trimmed, key string) (string, bool) {
	body := strings.TrimPrefix(trimmed, "- ")
	prefix := key + ":"
	if !strings.HasPrefix(body, prefix) {
		return "", false
	}
	v := strings.TrimSpace(strings.TrimPrefix(body, prefix))
	return strings.Trim(v, `"'`), true
}
