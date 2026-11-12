package deploytarget

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/testkit"
)

// Firebase's deploy/hosting/deploy.ts uploads listFiles(publicDir, ignore).
// listFiles.ts uses glob.sync("**/*", {dot:true,follow:true,nodir:true,ignore}).
// Exercise that real glob engine on disk, rather than interpreting its rules
// in Go or only asserting that firebase.json has Content-Type headers.
func TestFirebaseAssociationUploadSelection(t *testing.T) {
	testkit.Slow(t, "installs and executes Firebase uploader's glob engine with Node")
	for _, tool := range []string{"node", "npm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	root := t.TempDir()
	cmd := exec.Command("npm", "install", "--prefix", root, "--ignore-scripts", "--no-audit", "--no-fund", "glob@10.5.0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("npm registry unavailable for uploader selection harness: %v: %s", err, out)
	}
	public := filepath.Join(root, "public")
	writeTree(t, public, map[string]string{
		"index.html": "ok", ".env": "private", ".git/config": "private", ".DS_Store": "private",
		"nested/.secret/data": "private", "nested/.hidden": "private", "node_modules/pkg/index.js": "private",
		".well-known/apple-app-site-association": "{}", ".well-known/assetlinks.json": "[]",
		".well-known/private.json": "private", ".well-known/.secret": "private", ".well-known/subdir/data": "private",
		".well-known/assetlinks.json.extra": "private", "nested/.well-known/assetlinks.json": "private",
	})
	for _, names := range [][]string{{"apple-app-site-association", "assetlinks.json"}, {"apple-app-site-association"}} {
		spec := FirebaseHostingSpec{Project: "p", Site: "s", wellKnown: names}
		raw, err := renderFirebaseJSON(public, spec)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Hosting struct {
				Ignore []string `json:"ignore"`
			} `json:"hosting"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		ignored, _ := json.Marshal(doc.Hosting.Ignore)
		script := `const {sync}=require(process.argv[1]); const files=sync('**/*',{cwd:process.argv[2],dot:true,follow:true,nodir:true,posix:true,ignore:JSON.parse(process.argv[3])}).sort(); console.log(JSON.stringify(files));`
		out, err := exec.Command("node", "-e", script, filepath.Join(root, "node_modules/glob"), public, string(ignored)).CombinedOutput()
		if err != nil {
			t.Fatalf("real Firebase selection: %v %s", err, out)
		}
		var files []string
		if err := json.Unmarshal(out, &files); err != nil {
			t.Fatal(err)
		}
		expected := map[string]bool{"index.html": true}
		for _, n := range names {
			expected[".well-known/"+n] = true
		}
		if len(files) != len(expected) {
			t.Fatalf("generated %v selected %v, want %v (ignore %s)", names, files, expected, ignored)
		}
		for _, f := range files {
			if !expected[f] {
				t.Fatalf("unexpected upload %s with ignore %s", f, ignored)
			}
		}
		if strings.Contains(string(out), "private") {
			t.Fatal("private dotfiles selected")
		}
	}
}
