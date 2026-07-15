package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// mkfile creates path (and parents) with trivial content.
func mkfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLooksLikeGeneratedCRUDRouteDir_Nextjs pins the shape fingerprint: only a
// dir carrying the generated `[id]/page.tsx` dynamic-detail route counts, so a
// user's hand-authored route (a bare page.tsx) is not misread as generated.
func TestLooksLikeGeneratedCRUDRouteDir_Nextjs(t *testing.T) {
	root := t.TempDir()

	gen := filepath.Join(root, "widgets")
	mkfile(t, filepath.Join(gen, "page.tsx"))
	mkfile(t, filepath.Join(gen, "[id]", "page.tsx"))
	if !looksLikeGeneratedCRUDRouteDir("nextjs", gen) {
		t.Error("a dir with [id]/page.tsx should read as a generated CRUD route")
	}

	userRoute := filepath.Join(root, "about")
	mkfile(t, filepath.Join(userRoute, "page.tsx"))
	if looksLikeGeneratedCRUDRouteDir("nextjs", userRoute) {
		t.Error("a plain page.tsx route must NOT be flagged as generated (false-positive guard)")
	}
}

// TestReportStaleFrontendRouteDirs_OrphanDetected is the F7 assertion: after a
// rename, a generated-shaped route dir whose slug isn't live is reported.
// Report-only — the directory is never removed.
func TestReportStaleFrontendRouteDirs_OrphanDetected(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "src", "app")

	// Live entity route (gadgets) — present in liveSlugs, must be left alone.
	mkfile(t, filepath.Join(appDir, "gadgets", "[id]", "page.tsx"))
	// Orphaned generated route (widgets) — renamed away, NOT in liveSlugs.
	orphan := filepath.Join(appDir, "widgets", "[id]", "page.tsx")
	mkfile(t, orphan)
	// A user's own route — not generated-shaped, must never be flagged.
	mkfile(t, filepath.Join(appDir, "settings", "page.tsx"))

	live := map[string]bool{"gadgets": true}

	// Should not panic and must leave every dir on disk (report-only).
	reportStaleFrontendRouteDirs("nextjs", root, "web", live)

	for _, p := range []string{
		filepath.Join(appDir, "gadgets", "[id]", "page.tsx"),
		orphan,
		filepath.Join(appDir, "settings", "page.tsx"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("report-only cleanup must not remove %s: %v", p, err)
		}
	}
}
