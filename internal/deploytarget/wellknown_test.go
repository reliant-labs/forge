package deploytarget

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteWellKnown(t *testing.T) {
	root := t.TempDir()
	if err := writeWellKnown(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, WellKnownDir)); !os.IsNotExist(err) {
		t.Error("a site with no association files grew a .well-known directory")
	}
	if err := writeWellKnown(root, map[string]string{"apple-app-site-association": "{}\n", "assetlinks.json": "[]\n"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"apple-app-site-association": "{}\n", "assetlinks.json": "[]\n"} {
		got, err := os.ReadFile(filepath.Join(root, ".well-known", name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
}

// The extensionless apple-app-site-association is why the content type is
// explicit: a bucket stores an object with no extension as octet-stream, which
// Apple refuses. The files are uploaded on their own pass, typed and no-cache;
// every other object keeps its rule's header.
func TestUploadRelease_TypesTheAssociationFiles(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	writeTree(t, staging, map[string]string{
		"index.html":                             "<html>",
		"assets/app.js":                          "x",
		".well-known/apple-app-site-association": "{}",
		".well-known/assetlinks.json":            "[]",
	})
	r := &uploadSnapshotRunner{fakeRunner: &fakeRunner{}, t: t, uploads: map[string]string{}}
	spec := StaticSiteSpec{Bucket: "b", AssetDir: "assets"}
	if err := uploadRelease(context.Background(), r, spec, staging, "deadbeef1234"); err != nil {
		t.Fatal(err)
	}
	if got := r.uploads[".well-known/apple-app-site-association"]; got != cacheRevalidate {
		t.Errorf("AASA Cache-Control = %q, want %q", got, cacheRevalidate)
	}
	if got := r.uploads["assets/app.js"]; got != cacheImmutable {
		t.Errorf("assets/app.js = %q: the association pass must not disturb the other rules", got)
	}
	var typed int
	for _, c := range r.fakeRunner.calls {
		if strings.Contains(c, "--content-type="+WellKnownContentType) {
			typed++
		}
	}
	if typed != 1 {
		t.Errorf("%d upload passes carried Content-Type %s, want exactly one (the association files)", typed, WellKnownContentType)
	}
}

func TestUploadRelease_NoAssociationFilesIsUnchanged(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	writeTree(t, staging, map[string]string{"index.html": "<html>"})
	r := &uploadSnapshotRunner{fakeRunner: &fakeRunner{}, t: t, uploads: map[string]string{}}
	if err := uploadRelease(context.Background(), r, StaticSiteSpec{Bucket: "b"}, staging, "deadbeef1234"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.fakeRunner.calls {
		if strings.Contains(c, "--content-type") {
			t.Errorf("a site with no association files set a content type: %v", c)
		}
	}
}

func TestRenderFirebaseJSON_PinsTheAssociationFilesContentType(t *testing.T) {
	spec := FirebaseHostingSpec{Project: "p", Site: "s", SPAFallback: "/index.html"}.
		withWellKnown(map[string]string{"apple-app-site-association": "{}", "assetlinks.json": "[]"})
	out, err := renderFirebaseJSON("/tmp/stage/public", spec)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hosting struct {
			Headers []struct {
				Source  string
				Headers []struct{ Key, Value string }
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, h := range doc.Hosting.Headers {
		for _, kv := range h.Headers {
			if kv.Key == "Content-Type" {
				got[h.Source] = kv.Value
			}
		}
	}
	for _, src := range []string{"/.well-known/apple-app-site-association", "/.well-known/assetlinks.json"} {
		if got[src] != "application/json" {
			t.Errorf("%s Content-Type = %q in %s", src, got[src], out)
		}
	}
	plain, _ := renderFirebaseJSON("/tmp/stage/public", FirebaseHostingSpec{Project: "p", Site: "s"})
	if strings.Contains(plain, "Content-Type") {
		t.Errorf("a site with no association files pinned a content type: %s", plain)
	}
}

func TestStagePlan_WritesWellKnownAfterTheCopies(t *testing.T) {
	proj := t.TempDir()
	writeTree(t, filepath.Join(proj, "fe", "dist"), map[string]string{"index.html": "<html>", ".well-known/assetlinks.json": "STALE"})
	plan, err := buildStagePlan(StageInput{
		Name: "web", Path: "fe", PublicDir: "dist", ProjectDir: proj, StagingRoot: filepath.Join(proj, "stage"),
		WellKnown: map[string]string{"assetlinks.json": "[]\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembleStaging(plan); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(proj, "stage", ".well-known", "assetlinks.json"))
	if err != nil || string(got) != "[]\n" {
		t.Errorf("the env's file did not overwrite the one in the build: %q, %v", got, err)
	}
}
