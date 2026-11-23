package cli

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestGenerateCacheImportedProtoRoots reads proto roots from every manifest
// in the snapshot — deduplicated, project-relative, and present or not —
// and records a module's buf inputs from disk alone. No git: it runs under
// -short.
func TestGenerateCacheImportedProtoRoots(t *testing.T) {
	root := t.TempDir()
	writeTreeFileT(t, filepath.Join(root, "frontends", "web", "package.json"), importedHooksManifest)
	writeTreeFileT(t, filepath.Join(root, "frontends", "admin", "package.json"),
		`{"forge": {"imported_hooks": [{"proto_root": "../../proto-vendor/other"}, {"proto_root": "../../missing"}]}}`+"\n")
	writeTreeFileT(t, filepath.Join(root, "frontends", "plain", "package.json"), "{}\n")
	writeTreeFileT(t, filepath.Join(root, "frontends", "web", "node_modules", "x", "package.json"), importedHooksManifest)
	writeTreeFileT(t, filepath.Join(root, "proto-vendor", "other", "buf.yaml"), "version: v2\n")
	writeTreeFileT(t, filepath.Join(root, "proto-vendor", "other", "other", "v1", "foo.proto"), "syntax = \"proto3\";\n")
	writeTreeFileT(t, filepath.Join(root, "proto-vendor", "other", "README.md"), "x\n")

	snap := &workTreeSnapshot{files: map[string]fileStamp{
		"frontends/web/package.json":                  {},
		"frontends/admin/package.json":                {},
		"frontends/plain/package.json":                {},
		"frontends/web/node_modules/x/package.json":   {},
		"proto-vendor/other/other/v1/unrelated.proto": {},
	}}
	got := generateCacheImportedProtoRoots(root, snap)
	if want := []string{"missing", "proto-vendor/other"}; !slices.Equal(got, want) {
		t.Fatalf("proto roots = %v, want %v", got, want)
	}

	fp := &generateFingerprint{files: map[string]string{}}
	for _, dir := range got {
		if err := fp.addProtoModule(root, dir); err != nil {
			t.Fatal(err)
		}
	}
	for key, want := range map[string]string{
		"missing/buf.yaml": "absent",
		"missing/":         "absent",
	} {
		if fp.files[key] != want {
			t.Errorf("%s = %q, want %q", key, fp.files[key], want)
		}
	}
	for _, key := range []string{"proto-vendor/other/buf.yaml", "proto-vendor/other/other/v1/foo.proto"} {
		if id := fp.files[key]; id == "" || id == "absent" {
			t.Errorf("%s must be recorded by content, got %q; keys: %v", key, id, keysOf(fp.files))
		}
	}
	if _, ok := fp.files["proto-vendor/other/README.md"]; ok {
		t.Errorf("a file buf does not read must not be recorded; keys: %v", keysOf(fp.files))
	}
}
