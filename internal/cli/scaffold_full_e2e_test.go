//go:build e2e

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestE2EScaffoldFullSpecProject is the authoritative regression test for
// `forge new`'s scaffold output. It exercises the exact invocation promised
// in the README:
//
//	forge new demo --mod github.com/example/demo --service api --frontend web --license MIT
//
// and then verifies — in order of blast radius —
//  1. the generated tree compiles, vets, and tests cleanly
//  2. `go mod tidy` is a no-op (guards against drift in go.mod/go.sum)
//  3. optional linters (golangci-lint, buf) if installed
//  4. every file listed in the spec actually exists
//  5. specific byte-level content guards that protect against known
//     past regressions (see inline comments on each guard).
//
// Each guard traces back to a bug we've already shipped to users once.
// Don't soften them without replacing with an equivalent check.
func TestE2EScaffoldFullSpecProject(t *testing.T) {
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// Exact invocation from the spec. Any deviation here reduces the value
	// of this test as a user-facing regression guard.
	runCmd(t, dir, forgeBin,
		"new", "demo",
		"--mod", "github.com/example/demo",
		"--service", "api",
		"--frontend", "web",
		"--license", "MIT",
	)

	projectDir := filepath.Join(dir, "demo")

	// Let `forge generate` fill in the derived sources (gen/, bootstrap,
	// handler stubs). Without this step `go build` fails because the proto
	// stubs don't exist yet.
	runCmd(t, projectDir, forgeBin, "generate")

	// `go mod tidy` across both module boundaries (project + gen). The
	// post-generate state is what users see after cloning a scaffold.
	runCmd(t, projectDir, "go", "mod", "tidy")
	runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

	// Capture go.mod/go.sum and ensure a second tidy is a no-op. Drift here
	// means `forge new` emits a go.mod that's not self-consistent, which
	// surfaces as "your go.mod is out of sync" on fresh clones.
	modBefore := readFileE2E(t, filepath.Join(projectDir, "go.mod"))
	runCmd(t, projectDir, "go", "mod", "tidy")
	modAfter := readFileE2E(t, filepath.Join(projectDir, "go.mod"))
	if modBefore != modAfter {
		t.Fatalf("go mod tidy is not idempotent on the scaffold output\nbefore:\n%s\nafter:\n%s",
			modBefore, modAfter)
	}

	// Core toolchain checks.
	runCmd(t, projectDir, "go", "build", "./...")
	runCmd(t, projectDir, "go", "vet", "./...")
	runCmd(t, projectDir, "go", "test", "./...")

	// Optional tools — skip with a log if they're not installed so the
	// test still works on a minimal dev box.
	if toolAvailable("golangci-lint") {
		runCmd(t, projectDir, "golangci-lint", "run", "./...")
	} else {
		t.Log("golangci-lint not available — skipping lint check")
	}
	if toolAvailable("buf") {
		runCmd(t, projectDir, "buf", "lint")
	} else {
		t.Log("buf not available — skipping proto lint check")
	}

	// ── File-existence guards ──────────────────────────────────────────
	// Every path here corresponds to an item in the spec. Keep this list
	// in sync with the scaffold; it's the checklist a user would run
	// after `forge new` to make sure nothing's missing.
	mustExist := []string{
		// cmd/ — one file per top-level concern
		"cmd/main.go",
		"cmd/server.go",
		"cmd/version.go",
		"cmd/otel.go",
		"cmd/db.go",

		// pkg/middleware — security-critical files. Any one of these
		// going missing silently degrades the scaffold's security
		// posture, so check each explicitly.
		"pkg/middleware/auth.go",
		"pkg/middleware/authz.go",
		"pkg/middleware/cors.go",
		"pkg/middleware/recovery.go",
		"pkg/middleware/audit.go",
		"pkg/middleware/http.go",
		"pkg/middleware/logging.go",
		"pkg/middleware/ratelimit.go",
		"pkg/middleware/claims.go",
		"pkg/middleware/security_headers.go",
		"pkg/middleware/permissive_authz.go",

		// Service + proto — the scaffold's raison d'être.
		"handlers/api/service.go",
		"proto/services/api/v1/api.proto",

		// Frontend — go.mod boundary marker plus real frontend files.
		"frontends/web/package.json",
		"frontends/web/go.mod",
		"frontends/web/buf.gen.yaml",

		// CI workflows.
		".github/workflows/ci.yml",
		".github/workflows/build-images.yml",
		".github/workflows/deploy.yml",
		".github/workflows/e2e.yml",

		// KCL deploy manifests.
		"deploy/kcl/schema.k",
		"deploy/kcl/base.k",
		"deploy/kcl/render.k",

		// Top-level project documentation.
		"LICENSE",
		"README.md",
		"CONTRIBUTING.md",
		"CHANGELOG.md",
	}
	for _, rel := range mustExist {
		assertPathExistsE2E(t, filepath.Join(projectDir, rel))
	}

	// handlers_gen.go only exists after `forge generate` runs against a
	// proto with RPCs. The scaffold's default api.proto has RPCs, so this
	// should be present. If a future template change stops emitting it,
	// this fails loudly here rather than silently downstream.
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "api", "handlers_gen.go"))

	// ── Byte-level anti-regression content guards ─────────────────────
	// Each guard below is paired with the bug that produced it. If you
	// need to remove a guard, first remove the corresponding bug risk.

	serverGo := readFileE2E(t, filepath.Join(projectDir, "cmd", "server.go"))
	// Past bug: server used a raw error compare on srv.Serve's return,
	// which misclassified wrapped errors. Regressing this causes the
	// server process to exit cleanly when it should have logged a real
	// error.
	if !strings.Contains(serverGo, "errors.Is(err, http.ErrServerClosed)") {
		t.Errorf("cmd/server.go must use errors.Is(err, http.ErrServerClosed); got:\n%s",
			excerpt(serverGo, "Serve", 400))
	}
	// Past bug: server used the `postgres://` URL directly with
	// database/sql, which has no registered driver named "postgres" in a
	// bare binary. pgx/v5/stdlib registers a "pgx" driver that accepts
	// postgres URLs; blank-importing it is the fix.
	if !strings.Contains(serverGo, `_ "github.com/jackc/pgx/v5/stdlib"`) {
		t.Errorf("cmd/server.go must blank-import github.com/jackc/pgx/v5/stdlib; got:\n%s",
			excerpt(serverGo, "import", 400))
	}

	authGo := readFileE2E(t, filepath.Join(projectDir, "pkg", "middleware", "auth.go"))
	// Past bug: the unauthenticated allow-list was implemented as
	// `strings.Contains(procedure, "Health")`, which matches any RPC with
	// "Health" anywhere in its name — e.g. a user-defined `HealthReport`
	// silently bypassed auth. The scaffold must use an exact-match map,
	// not substring matching.
	healthContains := regexp.MustCompile(`strings\.Contains\([^)]*Health`)
	if healthContains.MatchString(authGo) {
		t.Errorf("pkg/middleware/auth.go must not use strings.Contains(...Health...) for unauthenticated allow-list; use exact procedure matching instead. Got:\n%s",
			authGo)
	}

	configGo := readFileE2E(t, filepath.Join(projectDir, "pkg", "config", "config.go"))
	// Past bug: PORT was parsed with `strconv.Atoi` (int) which accepts
	// values outside the 16-bit port range (e.g. 99999) and then silently
	// truncates when assigned. `ParseUint(v, 10, 16)` range-checks at
	// parse time.
	if !strings.Contains(configGo, "ParseUint(v, 10, 16)") {
		t.Errorf("pkg/config/config.go must use strconv.ParseUint(v, 10, 16) for PORT parsing; got:\n%s",
			excerpt(configGo, "PORT", 400))
	}

	frontendBufGen := readFileE2E(t, filepath.Join(projectDir, "frontends", "web", "buf.gen.yaml"))
	// Past bug: `include_imports: true` was nested under `opt:` in buf
	// v2, which bufbuild/es rejects as an unknown option. It must be a
	// sibling of `out:` / `opt:`, not an element of the opt list.
	if !strings.Contains(frontendBufGen, "include_imports: true") {
		t.Errorf("frontends/web/buf.gen.yaml must set include_imports: true; got:\n%s",
			frontendBufGen)
	}
	assertIncludeImportsPlacement(t, frontendBufGen)

	gitignore := readFileE2E(t, filepath.Join(projectDir, ".gitignore"))
	// Past bug: `.gitignore` ignored `cmd/*.go` because those were
	// regenerated on every `forge generate`. After the "scaffold-once,
	// user-owns-it" refactor they must be tracked by git; re-ignoring
	// them breaks fresh clones.
	if hasGitignoreRule(gitignore, "cmd/*.go") {
		t.Errorf(".gitignore must not ignore cmd/*.go (user-owned after scaffold); got:\n%s",
			gitignore)
	}

	handlersGen := readFileE2E(t, filepath.Join(projectDir, "handlers", "api", "handlers_gen.go"))
	// Past bug: generated error strings were ALL-CAPS ("HANDLER FOR %s
	// NOT YET IMPLEMENTED") which violates Go's error-string convention
	// (lowercase, no trailing punctuation) and lint-fails on any
	// user-configured staticcheck.
	if hasUpperCaseErrorString(handlersGen) {
		t.Errorf("handlers_gen.go must use lowercase error strings (Go convention); got:\n%s",
			handlersGen)
	}
}

// assertIncludeImportsPlacement fails the test if `include_imports` appears
// as a list item under `opt:` instead of as a top-level plugin field.
//
// Layout expected (correct):
//
//	plugins:
//	  - remote: buf.build/bufbuild/es
//	    out: ...
//	    include_imports: true
//	    opt:
//	      - target=ts
//
// Layout rejected (past bug):
//
//	plugins:
//	  - remote: buf.build/bufbuild/es
//	    out: ...
//	    opt:
//	      - target=ts
//	      - include_imports=true
func assertIncludeImportsPlacement(t *testing.T, content string) {
	t.Helper()
	// If include_imports appears inside an `opt:` list entry (prefix `- `
	// after indentation) we've regressed.
	optListBug := regexp.MustCompile(`(?m)^\s*-\s*include_imports`)
	if optListBug.MatchString(content) {
		t.Errorf("frontends/web/buf.gen.yaml has include_imports under opt: as a list item; it must be a plugin-level field. Got:\n%s",
			content)
	}
}

// hasGitignoreRule reports whether pattern appears as a non-comment,
// non-blank rule in the .gitignore content. Commented-out patterns and
// patterns that are substrings of other patterns don't count.
func hasGitignoreRule(content, pattern string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == pattern {
			return true
		}
	}
	return false
}

// hasUpperCaseErrorString reports whether the generated handlers file
// contains an obviously ALL-CAPS error string in an errors.New /
// fmt.Errorf call. Matches strings of the form
//
//	fmt.Errorf("HANDLER FOR %s NOT YET IMPLEMENTED", ...)
//
// which was the exact shape of the past regression.
func hasUpperCaseErrorString(content string) bool {
	// Match a double-quoted string literal immediately following
	// fmt.Errorf( or errors.New( that is at least 10 chars long and
	// contains no lowercase letters. The 10-char floor keeps us from
	// flagging single-word identifiers.
	re := regexp.MustCompile(`(?:fmt\.Errorf|errors\.New)\("[^a-z"]{10,}"`)
	return re.Match([]byte(content))
}

// excerpt returns the line containing needle plus surrounding context
// (up to maxBytes). Used to make error messages pinpoint the offending
// region without dumping entire files.
func excerpt(content, needle string, maxBytes int) string {
	idx := strings.Index(content, needle)
	if idx < 0 {
		if len(content) > maxBytes {
			return content[:maxBytes] + "…(truncated)"
		}
		return content
	}
	start := idx - maxBytes/2
	if start < 0 {
		start = 0
	}
	end := idx + maxBytes/2
	if end > len(content) {
		end = len(content)
	}
	return content[start:end]
}

// Guard against build breakage caused by "unused" imports when we later
// whittle down this test file; keep the package list tight.
var (
	_ = bytes.NewBuffer
	_ = os.Getenv
	_ = exec.Command
)
