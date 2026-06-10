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

import "path"

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
