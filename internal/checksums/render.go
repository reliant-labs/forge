// Side-render bookkeeping for forked files.
//
// When a Tier-1 file is forked, forge keeps regenerating the template
// content on every run — it just can't write it over the user's file.
// Discarding that fresh render throws away the only artifact that lets
// the user (or an LLM agent) reconcile the fork later. Instead the
// render is parked under `.forge/render/<relpath>` ("theirs", refreshed
// every run) and the FIRST render after the fork is also copied to
// `.forge/render-base/<relpath>` (an approximate merge base — the
// closest thing we have to "what the template produced when the user
// forked"). `forge unfork --merge` three-way-merges ours/base/theirs
// from these files.
//
// Both directories sit under `.forge/`, which the scaffolded project
// .gitignore already excludes (only `.forge/checksums.json` is
// negated back in) — side renders are per-developer state, never
// committed.
package checksums

import (
	"os"
	"path"
	"path/filepath"
)

// Side-render directory roots, project-relative. Exposed as constants
// so the cli layer can print them in messages without re-deriving.
const (
	// RenderDir holds the latest fresh render for each forked path,
	// refreshed on every `forge generate` run ("theirs").
	RenderDir = ".forge/render"
	// RenderBaseDir holds the first fresh render captured after the
	// fork — the approximate merge base for `forge unfork --merge`.
	RenderBaseDir = ".forge/render-base"
)

// SideRenderRelPath returns the project-relative location of the latest
// side render for relPath (`.forge/render/<relpath>`).
func SideRenderRelPath(relPath string) string {
	return path.Join(RenderDir, relPath)
}

// SideRenderBaseRelPath returns the project-relative location of the
// merge-base side render for relPath (`.forge/render-base/<relpath>`).
func SideRenderBaseRelPath(relPath string) string {
	return path.Join(RenderBaseDir, relPath)
}

// WriteSideRender parks the fresh render for a forked relPath:
//
//   - `.forge/render/<relpath>` is (over)written on every call — it
//     always holds the LATEST render ("theirs" for a future merge).
//   - `.forge/render-base/<relpath>` is written only if absent — the
//     first render captured after the fork is the closest approximation
//     of the content the user forked FROM, so it serves as the merge
//     base. Later renders must not clobber it or the three-way merge
//     would see template evolution as user edits.
//
// Failures creating the side-render are returned (not swallowed): a
// broken `.forge/` is a project-state problem the user should see, and
// the caller skips the real write either way.
func WriteSideRender(root, relPath string, content []byte) error {
	renderPath := filepath.Join(root, RenderDir, relPath)
	if err := os.MkdirAll(filepath.Dir(renderPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(renderPath, content, 0o644); err != nil {
		return err
	}

	basePath := filepath.Join(root, RenderBaseDir, relPath)
	if _, err := os.Stat(basePath); err == nil {
		return nil // base already captured — never overwrite
	}
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(basePath, content, 0o644)
}

// WriteSideRenderNoBase writes ONLY `.forge/render/<relpath>` — no
// merge-base seeding. Used by the `--explain-drift` redirect, where the
// path is drifted but NOT forked: seeding render-base for a non-forked
// path would leave a stale base behind that poisons the three-way merge
// if the user forks the file later (the base must capture the render at
// fork time, nothing earlier).
func WriteSideRenderNoBase(root, relPath string, content []byte) error {
	renderPath := filepath.Join(root, RenderDir, relPath)
	if err := os.MkdirAll(filepath.Dir(renderPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(renderPath, content, 0o644)
}

// CleanSideRenders removes both side-render files for relPath. Called
// when a path is unforked (plain or via --merge) — once forge owns the
// file again the parked renders are stale and would only confuse a
// later merge. Missing files are fine; any other removal error is
// returned.
func CleanSideRenders(root, relPath string) error {
	for _, p := range []string{
		filepath.Join(root, RenderDir, relPath),
		filepath.Join(root, RenderBaseDir, relPath),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
