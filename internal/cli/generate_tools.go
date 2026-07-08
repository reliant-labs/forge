package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/packs"
)

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
// `forge new` renders gen-go.mod.tmpl as part of the initial scaffold, but a
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
	// Mirror the root module's forge/pkg replace into gen/, rebased for
	// gen/'s extra directory depth. Without this the gen/ submodule has no
	// way to resolve the unpublished forge/pkg the root replace points at
	// (the root replace does not cascade into a separate submodule), so
	// `go mod tidy` in gen/ fails on the placeholder v0.0.0. Best-effort:
	// a read error degrades to no gen replace (same as release flow); the
	// downstream tidy surfaces the canonical resolution error if one remains.
	genReplace, err := forgePkgGenReplaceTarget(projectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not derive gen/ forge/pkg replace from root go.mod: %v\n", err)
		genReplace = ""
	}
	data := struct {
		Module          string
		GoVersion       string
		ForgePkgVersion string // empty in the bootstrap path → template emits `forge/pkg v0.0.0`, resolved by the gen-local replace below (mirrors the root replace). A real pin is supplied on the forge-new render path.
		// ForgePkgGenReplace is the gen-relative forge/pkg replace target
		// (root replace rebased one dir deeper). Empty in release/no-sibling
		// mode → template emits no replace and tidy resolves from the proxy.
		ForgePkgGenReplace string
	}{
		Module:             modulePath,
		GoVersion:          goVersion,
		ForgePkgGenReplace: genReplace,
	}
	if err := assets.WriteTemplateWithData("gen-go.mod.tmpl", goMod, data); err != nil {
		return fmt.Errorf("render gen/go.mod: %w", err)
	}
	fmt.Println("🔧 Bootstrapped missing gen/go.mod (fresh worktree).")
	return nil
}

// reconcileGenForgePkgReplace keeps an EXISTING gen/go.mod's forge/pkg
// replace in sync with the root module's. ensureGenGoMod only bootstraps a
// MISSING file (and intentionally never touches an existing one — a
// hand-edited gen/go.mod must survive). But the generate pipeline rewrites
// the ROOT forge/pkg replace earlier in the run (stepSyncDevForgePkg
// vendors an absolute sibling path → ./.forge-pkg), and a project scaffolded
// without a sibling checkout starts with NO gen replace at all. In both
// cases the on-disk gen/go.mod can carry a stale or absent forge/pkg
// replace by the time `go mod tidy (gen/)` runs — which then fails to
// resolve the unpublished forge/pkg. This reconciler rewrites (or inserts)
// exactly the forge/pkg replace line in gen/go.mod to the rebased root
// target, leaving every other byte alone so it composes with hand edits.
//
// Best-effort and idempotent: no gen/go.mod → no-op (bootstrap handles
// that); no root replace (release/no-sibling) → leave gen as-is so tidy
// resolves a published version like the root; an already-correct line →
// no write.
func reconcileGenForgePkgReplace(projectDir string) error {
	genGoMod := filepath.Join(projectDir, "gen", "go.mod")
	data, err := os.ReadFile(genGoMod)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // missing → ensureGenGoMod's job, not ours
		}
		return fmt.Errorf("read gen/go.mod: %w", err)
	}

	want, err := forgePkgGenReplaceTarget(projectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not derive gen/ forge/pkg replace from root go.mod: %v\n", err)
		return nil
	}
	if want == "" {
		// Release / no-sibling flow: the root resolves forge/pkg from the
		// proxy, so gen/ should too. Don't strip an existing replace — a
		// user may have added one deliberately — just leave things alone.
		return nil
	}

	content := string(data)
	desired := "replace " + forgePkgModulePath + " => " + want
	if forgePkgReplaceLineRE.MatchString(content) {
		updated := forgePkgReplaceLineRE.ReplaceAllString(content, desired)
		if updated == content {
			return nil // already correct
		}
		if err := os.WriteFile(genGoMod, []byte(updated), 0o644); err != nil {
			return fmt.Errorf("rewrite gen/go.mod forge/pkg replace: %w", err)
		}
		fmt.Printf("  🔧 Synced gen/go.mod forge/pkg replace → %s\n", want)
		return nil
	}

	// No existing replace line → append one (with a trailing newline if the
	// file doesn't end in one).
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += desired + "\n"
	if err := os.WriteFile(genGoMod, []byte(content), 0o644); err != nil {
		return fmt.Errorf("insert gen/go.mod forge/pkg replace: %w", err)
	}
	fmt.Printf("  🔧 Added gen/go.mod forge/pkg replace → %s\n", want)
	return nil
}

// goVersionFromProject reads the `go <version>` directive out of the
// project's root go.mod and returns it, falling back to a conservative
// default that matches the rest of the scaffolder. Kept local so the cli
// package doesn't have to import the generator's GoVersion helpers (which
// are oriented around the ProjectGenerator struct).
func goVersionFromProject(projectDir string) string {
	data, err := os.ReadFile(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		return "1.26.2"
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
	return "1.26.2"
}

// runGoModTidyGen runs `go mod tidy` inside the gen/ directory to keep deps fresh.
func runGoModTidyGen(projectDir string) error {
	genDir := filepath.Join(projectDir, "gen")
	goMod := filepath.Join(genDir, "go.mod")
	if _, err := os.Stat(goMod); os.IsNotExist(err) {
		// No go.mod in gen/, nothing to tidy
		return nil
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

// runGoimportsOnGenerated runs goimports on generated Go files to fix import grouping.
func runGoimportsOnGenerated(projectDir, modulePath string) error {
	goimportsPath, err := exec.LookPath("goimports")
	if err != nil {
		fmt.Println("  ⚠️  goimports not found — skipping import formatting")
		fmt.Println("     Install with: go install golang.org/x/tools/cmd/goimports@latest")
		return nil
	}

	dirs := []string{"cmd", "pkg", "gen", "internal"}
	var targets []string
	for _, d := range dirs {
		if dirExists(filepath.Join(projectDir, d)) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	fmt.Println("🔨 Running goimports on generated code...")
	args := []string{"-local", modulePath, "-w"}
	args = append(args, targets...)
	cmd := exec.Command(goimportsPath, args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("goimports failed: %w", err)
	}

	fmt.Println("  ✅ Imports formatted")
	return nil
}

// runPackGenerateHooks processes generate hooks for all installed packs
// in dependency-respecting order: producers (depended-on packs) before
// consumers. This matters when one pack's generate hook references
// another pack's generated output — without the topo sort, hook order
// is whatever cfg.Packs happens to list and the dependent hook can run
// against stale (or absent) producer output.
func runPackGenerateHooks(projectDir string, cfg *config.ProjectConfig, cs *checksums.FileChecksums) error {
	// Topo-sort first; fall back to cfg.Packs order on any sort error
	// (a cycle or missing manifest is rare and we don't want to block
	// generate on it — the dep is still likely to render fine).
	order, err := packs.SortInstalledByDependencies(cfg.Packs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ pack dep sort failed (%v); falling back to cfg.Packs order\n", err)
		order = cfg.Packs
	}

	for _, name := range order {
		p, err := packs.GetPack(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: installed pack %q not found: %v\n", name, err)
			continue
		}
		if len(p.Generate) == 0 {
			continue
		}
		fmt.Printf("\n🔌 Running generate hooks for pack '%s'...\n", p.Name)
		// cs threads the checksum ledger so generate-hook output is written
		// as self-certifying Tier-1 code (forge:hash marker + tracked),
		// keeping pack-emitted _gen.go files out of the audit orphan bucket.
		if err := p.RenderGenerateFiles(projectDir, cfg, cs); err != nil {
			return fmt.Errorf("pack %s generate hooks: %w", p.Name, err)
		}
	}
	return nil
}
