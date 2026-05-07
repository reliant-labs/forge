package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
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

// rehashTrackedFiles refreshes the on-disk-content checksum for every entry
// in cs.Files. Called after goimports (or any other post-write formatter)
// runs over the project so that audit doesn't flag goimports-induced
// import-group rearrangements as user edits. Missing files are dropped from
// the tracker rather than left with stale hashes — they'll be re-recorded
// next generate cycle if forge still emits them.
//
// This complements `WriteGeneratedFile`: WriteGeneratedFile records the
// hash of the pre-formatter content (so the History entry survives), and
// this pass updates the *current* Hash to match the formatter's output.
func rehashTrackedFiles(projectDir string, cs *generator.FileChecksums) {
	if cs == nil || len(cs.Files) == 0 {
		return
	}
	for rel := range cs.Files {
		full := filepath.Join(projectDir, rel)
		content, err := os.ReadFile(full)
		if err != nil {
			// File was removed (e.g. cleanup of stale codegen). Drop the
			// stale checksum so audit doesn't keep reporting it.
			if os.IsNotExist(err) {
				delete(cs.Files, rel)
			}
			continue
		}
		entry := cs.Files[rel]
		entry.Hash = generator.HashContent(content)
		cs.Files[rel] = entry
	}
}

// runGoimportsOnGenerated runs goimports on generated Go files to fix import grouping.
func runGoimportsOnGenerated(projectDir, modulePath string) error {
	goimportsPath, err := exec.LookPath("goimports")
	if err != nil {
		fmt.Println("  ⚠️  goimports not found — skipping import formatting")
		fmt.Println("     Install with: go install golang.org/x/tools/cmd/goimports@latest")
		return nil
	}

	dirs := []string{"cmd", "pkg", "gen", "handlers"}
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
func runPackGenerateHooks(projectDir string, cfg *config.ProjectConfig) error {
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
		if err := p.RenderGenerateFiles(projectDir, cfg); err != nil {
			return fmt.Errorf("pack %s generate hooks: %w", p.Name, err)
		}
	}
	return nil
}
