// File: internal/cli/lint/lint_format.go
//
// The deterministic-safe half of `forge lint`'s auto-fix-then-gate default.
//
// Forensics (long overnight builds): the dominant `forge lint` failures were
// all trivially mechanical — goimports/gofmt import-grouping and whitespace,
// re-introduced every time a file was hand-edited after `forge generate`.
// `forge lint --fix` existed but was opt-in and never used; agents instead
// hand-ran `gofmt -w`/`goimports -w` and hand-fixed every finding. Mechanical
// formatting has no business gating a build.
//
// formatGoTree applies the canonical Go formatter IN WRITE MODE across the
// project's Go source — generated AND owned/hand-edited files alike — before
// the gating pipeline runs, so import grouping and gofmt whitespace never
// surface as a gate. It is invoked by runAllLinters unless the caller opted
// out with `--no-fix` (or is in `--json` detect-only mode).
//
// Why the in-process formatter (not the `goimports` binary): CanonicalGoSource
// is goimports' formatting engine in FormatOnly mode with the project module
// as the local-import prefix — provably byte-identical to what the scaffolded
// .golangci.yml's `goimports` formatter (local-prefixes: [<module>]) gates on
// (see internal/checksums/goformat.go). So the pre-pass fixes exactly what the
// gate would flag, needs no `goimports` on PATH, and is deterministic and
// hermetic. It is also idempotent: already-canonical files (every clean
// generated file — WriteGeneratedFile canonicalizes before it stamps) are a
// no-op and are never rewritten, so the pre-pass cannot disturb Tier-1
// forge:hash stamps.

package lint

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

// goFormatDirs are the source trees the format pre-pass walks — the same set
// the generate pipeline's post-write goimports pass covers
// (runGoimportsOnGenerated). Walking whole trees (rather than only files
// forge wrote) is the point: it catches the owned/hand-edited files
// (compose.go after `forge project disown`, providers.go, contract bodies) that
// re-introduce formatting drift between generate runs.
var goFormatDirs = []string{"cmd", "pkg", "gen", "internal"}

// formatGoTree canonicalizes every .go file under the project's source trees
// and rewrites those that were not already canonical. Returns the repo-
// relative paths it changed, in walk order. Best-effort per file: a file that
// does not parse (a hand-edit that broke the syntax) is left untouched — the
// real compiler error surfaces at the pipeline's build/gate step, not here.
func formatGoTree(root string) ([]string, error) {
	prefix := checksums.GoImportsLocalPrefix(root)
	var changed []string
	for _, d := range goFormatDirs {
		base := filepath.Join(root, d)
		if !dirExists(base) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, de fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if de.IsDir() {
				switch de.Name() {
				case "node_modules", "vendor", ".git":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			rel, cerr := formatGoFile(root, prefix, path)
			if cerr != nil {
				return cerr
			}
			if rel != "" {
				changed = append(changed, rel)
			}
			return nil
		})
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// formatGoFile rewrites path in place if canonical formatting changes its
// bytes, preserving the file's existing mode. Returns the repo-relative path
// when it rewrote the file, or "" when the file was already canonical or
// could not be parsed (left untouched). Only I/O errors are returned.
func formatGoFile(root, prefix, path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	formatted, err := checksums.CanonicalGoSource(prefix, path, src)
	if err != nil {
		// Unparseable (broken edit) — not this pass's job to fix; leave it
		// for the compiler / gate to report with a real error.
		return "", nil //nolint:nilerr // intentional: skip, don't fail the pre-pass
	}
	if bytes.Equal(src, formatted) {
		return "", nil
	}
	mode := fs.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, formatted, mode); err != nil {
		return "", err
	}
	rel, relErr := filepath.Rel(root, path)
	if relErr != nil {
		rel = path
	}
	return rel, nil
}

// reportFormatPrePass runs formatGoTree and prints a concise summary of the
// files it rewrote. A pre-pass error is advisory: it prints a ⚠️ line and
// never gates, because the deterministic gate (golangci-lint's goimports
// formatter) still runs behind it and will surface anything the pre-pass
// could not apply.
func reportFormatPrePass(cwd string) {
	changed, err := formatGoTree(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  auto-format pre-pass: %v\n", err)
		return
	}
	if len(changed) == 0 {
		return
	}
	fmt.Printf("🔧 Auto-formatted %d Go file(s) before gating (goimports/gofmt):\n", len(changed))
	for _, c := range changed {
		fmt.Printf("   • %s\n", c)
	}
	fmt.Println()
}
