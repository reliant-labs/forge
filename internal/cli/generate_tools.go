package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
)

// devWorkspaceBridgesExternalModule reports whether the project's go.work
// bridges a module to a LOCAL directory OUTSIDE the project tree — the
// signature of a cross-repo dev bridge, which `forge project new` writes on a
// DEV forge build (`use <local-forge>/pkg`, see writeDevForgeGoWork).
//
// It matters for the go-mod-tidy steps: when the workspace deliberately
// overrides a published `require` with an UNPUBLISHED local checkout, a plain
// `go mod tidy` cannot succeed — tidy ignores go.work and resolves from the
// module proxy, so it 404s on the not-yet-published API and (fatally) aborts
// codegen. `go build`/`go vet` DO honor the workspace, so once tidy is
// softened the pipeline's final `go build (validate)` still guards
// correctness. This is a strict no-op for ordinary projects: the scaffold's
// starter go.work only `use`s `.` and `gen` (both inside the project), and
// released/CI builds never write an external `use` at all.
func devWorkspaceBridgesExternalModule(projectDir string) bool {
	data, err := os.ReadFile(filepath.Join(projectDir, "go.work"))
	if err != nil {
		return false
	}
	wf, err := modfile.ParseWork("go.work", data, nil)
	if err != nil {
		return false
	}
	isExternal := func(p string) bool {
		if p == "" {
			return false
		}
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(projectDir, p)
		}
		rel, err := filepath.Rel(projectDir, filepath.Clean(abs))
		if err != nil {
			return true // unrelated volume/root → outside the project
		}
		return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, u := range wf.Use {
		if isExternal(u.Path) {
			return true
		}
	}
	for _, r := range wf.Replace {
		// Only filesystem replaces (empty New.Version) point at a local dir.
		if r.New.Version == "" && isExternal(r.New.Path) {
			return true
		}
	}
	return false
}

// syncDevWorkspace runs `go work sync` in place of a proxy-resolving
// `go mod tidy` when a dev-forge go.work bridge is active. `go work sync`
// resolves against the workspace (so the local, unpublished bridged module is
// honored) and writes the go.sum / go.work.sum entries `go build` needs.
// Best-effort: a sync hiccup must not abort codegen, because the pipeline's
// final `go build (validate)` step is the real correctness gate under a
// workspace.
func syncDevWorkspace(projectDir, label string) error {
	fmt.Printf("🔗 %s: go.work bridges a local module — running `go work sync` instead of a proxy `go mod tidy`.\n", label)
	cmd := exec.Command("go", "work", "sync")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠️  go work sync failed (continuing; `go build (validate)` still gates correctness): %v\n", err)
	}
	return nil
}

// runSqlcGenerate runs sqlc generate if sqlc.yaml exists.
func runSqlcGenerate(projectDir string) error {
	if _, err := os.Stat(filepath.Join(projectDir, "sqlc.yaml")); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(projectDir, "sqlc.yml")); os.IsNotExist(err) {
			// No sqlc config found, skip silently
			return nil
		}
	}

	if _, err := exec.LookPath("sqlc"); err != nil {
		fmt.Println("  ⚠️  sqlc not found on PATH - skipping sqlc generate")
		return nil
	}

	fmt.Println("🔨 Running sqlc generate...")
	cmd := exec.Command("sqlc", "generate")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sqlc generate failed: %w", err)
	}

	fmt.Println("  ✅ sqlc queries generated")
	return nil
}

// ensureGenGoMod bootstraps `gen/go.mod` when it's missing.
//
// `forge project new` renders gen-go.mod.tmpl as part of the initial scaffold, but a
// git worktree carved from a checkout that has never run `forge generate`
// won't have `gen/go.mod` on disk — the file is typically gitignored or has
// simply not been committed. Every subsequent `go build` / `go test` /
// `go list` then fails with:
//
//	go: cannot load module gen listed in go.work file:
//	open gen/go.mod: no such file or directory
//
// because the project's go.work declares `use gen`. The pipeline runs `buf
// generate` and `go list ./...` before the post-codegen `go mod tidy (gen/)`
// step, so we have to render the file before any of those steps fire.
//
// Best-effort: anything that prevents us from synthesizing the file (no
// go.mod in the project root to derive the module path from, template
// render error) returns nil with a warning — the downstream step that
// actually needed the module will surface a clearer error.
func ensureGenGoMod(projectDir string) error {
	genDir := filepath.Join(projectDir, "gen")
	goMod := filepath.Join(genDir, "go.mod")
	if _, err := os.Stat(goMod); err == nil {
		return nil // already present
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat gen/go.mod: %w", err)
	}

	// Only bootstrap when go.work declares `use gen` — if there's no
	// gen workspace member, the missing file is by design.
	workData, err := os.ReadFile(filepath.Join(projectDir, "go.work"))
	if err != nil {
		// No go.work at all → no workspace → no need for gen/go.mod.
		return nil
	}
	if !strings.Contains(string(workData), "gen") {
		return nil
	}

	modulePath, err := codegen.GetModulePath(projectDir)
	if err != nil {
		// Intentional soft warning: this is a best-effort bootstrap for
		// fresh-checkout worktrees. If we can't read the module path the
		// project is unusable anyway — the downstream `go list ./...`
		// step (which the pipeline runs before any codegen) will surface
		// the canonical "module path missing" error with full context.
		// Promoting here would only produce a noisier, less actionable
		// failure for the same underlying cause.
		fmt.Fprintf(os.Stderr, "Warning: bootstrap gen/go.mod skipped (cannot read project module path): %v\n", err)
		return nil
	}

	goVersion := goVersionFromProject(projectDir)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return fmt.Errorf("create gen/: %w", err)
	}
	data := struct {
		Module    string
		GoVersion string
		// ForgePkgVersion mirrors the root module's forge/pkg pin so both
		// submodules resolve the SAME published forge/pkg from the proxy.
		// Empty when the root has no forge/pkg require (the template then
		// omits the line and `go mod tidy` resolves it if the generated
		// code imports it).
		ForgePkgVersion string
	}{
		Module:          modulePath,
		GoVersion:       goVersion,
		ForgePkgVersion: rootForgePkgVersion(projectDir),
	}
	if err := assets.WriteTemplateWithData("gen-go.mod.tmpl", goMod, data); err != nil {
		return fmt.Errorf("render gen/go.mod: %w", err)
	}
	fmt.Println("🔧 Bootstrapped missing gen/go.mod (fresh worktree).")
	return nil
}

// forgePkgRequireRE matches the forge/pkg `require` line in a go.mod —
// either the block form (`\tgithub.com/reliant-labs/forge/pkg vX.Y.Z`) or
// the single-line form (`require github.com/... vX.Y.Z`). It does NOT match
// a `replace` line (module path is not the first token there).
var forgePkgRequireRE = regexp.MustCompile(
	`(?m)^[\t ]*(?:require[\t ]+)?github\.com/reliant-labs/forge/pkg[\t ]+(v[^\s]+)[\t ]*$`)

// rootForgePkgVersion returns the forge/pkg version pinned in the project's
// root go.mod, or "" when the root has no forge/pkg require (or no go.mod).
// Used to mirror the root pin into a freshly bootstrapped gen/go.mod so the
// two submodules resolve forge/pkg to the same published version.
func rootForgePkgVersion(projectDir string) string {
	data, err := os.ReadFile(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		return ""
	}
	m := forgePkgRequireRE.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// goVersionFromProject reads the `go <version>` directive out of the
// project's root go.mod and returns it, falling back to a conservative
// default that matches the rest of the scaffolder. Kept local so the cli
// package doesn't have to import the generator's GoVersion helpers (which
// are oriented around the ProjectGenerator struct).
func goVersionFromProject(projectDir string) string {
	data, err := os.ReadFile(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		return "1.27.0"
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "go ") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "go "))
		if v != "" {
			return v
		}
	}
	return "1.27.0"
}

// runGoModTidyGen runs `go mod tidy` inside the gen/ directory to keep deps fresh.
func runGoModTidyGen(projectDir string) error {
	genDir := filepath.Join(projectDir, "gen")
	goMod := filepath.Join(genDir, "go.mod")
	if _, err := os.Stat(goMod); os.IsNotExist(err) {
		// No go.mod in gen/, nothing to tidy
		return nil
	}

	// A dev-forge go.work bridge deliberately overrides a published require
	// (forge/pkg) with an unpublished local checkout — a proxy `go mod tidy`
	// would 404 and abort codegen. Sync the workspace instead.
	if devWorkspaceBridgesExternalModule(projectDir) {
		return syncDevWorkspace(projectDir, "gen/ tidy")
	}

	fmt.Println("🔨 Running go mod tidy in gen/...")
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = genDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy in gen/ failed: %w", err)
	}

	fmt.Println("  ✅ gen/go.mod tidied")
	return nil
}

func runGoModTidyRoot(projectDir string) error {
	goMod := filepath.Join(projectDir, "go.mod")
	if _, err := os.Stat(goMod); os.IsNotExist(err) {
		return nil
	}

	// See runGoModTidyGen: under a dev-forge go.work bridge, sync the
	// workspace instead of a proxy `go mod tidy` that cannot resolve the
	// unpublished local module.
	if devWorkspaceBridgesExternalModule(projectDir) {
		return syncDevWorkspace(projectDir, "root tidy")
	}

	fmt.Println("🔨 Running go mod tidy in project root...")
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy in project root failed: %w", err)
	}

	fmt.Println("  ✅ go.mod tidied")
	return nil
}

// runGoimportsOnGenerated runs goimports over the Go files forge WROTE this
// run, fixing import grouping in its own output.
//
// Scoped to the written set, NOT to the cmd/pkg/gen/internal trees. Passing
// the directories made `forge generate` rewrite every hand-owned Go file in
// the project — forge editing what it does not own, which is the one thing the
// tier model exists to prevent. It is not merely cosmetic either: goimports
// RESOLVES unknown identifiers by guessing a package, so a file forge has no
// business touching can silently acquire an import nobody asked for. That is
// not hypothetical — it happened to a linter fixture in this repo, which
// gained an `html/template` import purely because `forge generate` ran.
//
// checksums.WrittenThisRun is the right scope because every forge write goes
// through the checksums chokepoint, including the in-place reconcilers that
// splice into compose.go / lifecycle.go.
func runGoimportsOnGenerated(projectDir, modulePath string) error {
	goimportsPath, err := exec.LookPath("goimports")
	if err != nil {
		fmt.Println("  ⚠️  goimports not found — skipping import formatting")
		fmt.Println("     Install with: go install golang.org/x/tools/cmd/goimports@latest")
		return nil
	}

	targets := writtenGoFiles(projectDir)
	if len(targets) == 0 {
		return nil
	}

	fmt.Println("🔨 Running goimports on generated code...")
	// Batch the argv: a large project can write hundreds of files in one run
	// and the whole set at once risks E2BIG.
	const batchSize = 200
	for start := 0; start < len(targets); start += batchSize {
		end := start + batchSize
		if end > len(targets) {
			end = len(targets)
		}
		args := append([]string{"-local", modulePath, "-w"}, targets[start:end]...)
		cmd := exec.Command(goimportsPath, args...)
		cmd.Dir = projectDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("goimports failed: %w", err)
		}
	}

	fmt.Println("  ✅ Imports formatted")
	return nil
}

// writtenGoFiles returns the project-relative .go paths forge wrote this run,
// sorted so the goimports invocation is deterministic. Paths that no longer
// exist are dropped: a file can be written and then removed by the cleanup
// sweep within the same run.
func writtenGoFiles(projectDir string) []string {
	var out []string
	for relPath := range checksums.WrittenPaths() {
		if filepath.Ext(relPath) != ".go" {
			continue
		}
		if _, err := os.Stat(filepath.Join(projectDir, relPath)); err != nil {
			continue
		}
		out = append(out, relPath)
	}
	sort.Strings(out)
	return out
}
