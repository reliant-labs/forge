package deploytarget

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"oras.land/oras-go/v2/content/memory"
)

func writeSite(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// forge#2231: the stamp names the tree it is written into — every file but
// itself — so the same site stamps the same digest, a changed file stamps
// another, and a stamp carried in from public/ is replaced, not trusted.
func TestStampStaticSiteTree(t *testing.T) {
	site := map[string]string{"index.html": "<h1>hi</h1>", "assets/app.js": "x()"}
	a, err := StampStaticSiteTree(writeSite(t, site))
	if err != nil {
		t.Fatal(err)
	}
	stale := map[string]string{"index.html": "<h1>hi</h1>", "assets/app.js": "x()", StaticSiteStampPath: `{"schema":"forge.static-site-stamp/v1","tree":"sha256:old"}`}
	dir := writeSite(t, stale)
	b, err := StampStaticSiteTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("the same site stamped %s and %s (a stale stamp leaked into the digest)", a, b)
	}
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(StaticSiteStampPath)))
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := ParseStaticSiteStamp(body)
	if err != nil || stamp.Tree != b {
		t.Fatalf("stamp = %+v, %v; want tree %s", stamp, err, b)
	}
	// Restamping a stamped tree is stable.
	if again, _ := StampStaticSiteTree(dir); again != b {
		t.Errorf("restamp = %s, want %s", again, b)
	}
	site["assets/app.js"] = "y()"
	if c, _ := StampStaticSiteTree(writeSite(t, site)); c == a {
		t.Error("a changed file stamped the same tree")
	}
	if _, err := ParseStaticSiteStamp([]byte("<html>")); err == nil {
		t.Error("an HTML page parsed as a stamp")
	}
}

// The manifest names the same tree, and stays a pure function of its inputs.
func TestPushStaticSite_AnnotatesTheTree(t *testing.T) {
	store := memory.New()
	layer := []byte("layer-bytes")
	d1, err := pushStaticSite(context.Background(), store, layer, "sha256:tree")
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := pushStaticSite(context.Background(), store, layer, "sha256:tree")
	if d1 != d2 {
		t.Fatalf("re-push changed the digest: %s != %s", d1, d2)
	}
	rc, err := store.Fetch(context.Background(), manifestDescFor(t, store, d1))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	got, err := staticSiteTreeOfManifest(bytes.NewReader(raw))
	if err != nil || got != "sha256:tree" {
		t.Fatalf("tree = %q, %v", got, err)
	}
	unstamped, _ := pushStaticSite(context.Background(), store, layer, "")
	rc, _ = store.Fetch(context.Background(), manifestDescFor(t, store, unstamped))
	raw, _ = io.ReadAll(rc)
	if got, _ := staticSiteTreeOfManifest(bytes.NewReader(raw)); got != "" {
		t.Errorf("an unstamped release declared tree %q", got)
	}
}
