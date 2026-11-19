package generator

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Vite scaffolds' dev-log receiver: inlined as src/lib/devlog-plugin.ts
// until the runtime floor ships forgeDevLog, imported from the runtime after.
//
// forgeDevLog() is exported from @reliantlabs/forge-web-runtime/vite in-tree,
// but a new project installs the PUBLISHED runtime at webRuntimePublishedRange.
// Importing it before the floor ships it would break every new Vite project
// (Vite fails to load vite.config.ts on the missing export); keeping the
// inlined copy after the floor ships it scaffolds a duplicate that never gets
// the runtime's fixes. So the scaffold follows the floor, in both directions,
// and this test is the switch: it fails the moment the two disagree.
//
// When it fails because the floor ships forgeDevLog, flip the scaffold:
//   - vite-spa/vite.config.ts.tmpl and site/vite/vite.config.ts: import
//     forgeDevLog from "@reliantlabs/forge-web-runtime/vite" and use
//     forgeDevLog() in plugins, instead of ./src/lib/devlog-plugin;
//   - stop writing src/lib/devlog-plugin.ts into new projects (the vite-spa
//     template file, site_starter.go's file list and the tests naming it).
// Existing projects keep their copy: it is scaffold-once, theirs, and works.
//
// NETWORK, like TestTemplatesOnlyImportPublishedWebRuntimeExports: it reads
// the floor's published vite.d.ts, so it skips under -short and offline.
func TestViteDevLogScaffoldFollowsTheRuntimeFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("network + npm; skipped in -short (runs in the full suite and CI)")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}
	floor, ok := minimumVersion(webRuntimePublishedRange)
	if !ok {
		t.Fatalf("invalid runtime floor %q", webRuntimePublishedRange)
	}
	floor = strings.TrimPrefix(floor, "v")
	floorShips := publishedWebRuntimeExports(t, floor, "vite")["forgeDevLog"]

	for _, rel := range viteDevLogScaffolds {
		body, err := os.ReadFile(filepath.Join("..", "templates", "frontend", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		imports, inlines := viteDevLogReceiver(string(body))
		switch {
		case floorShips && inlines:
			t.Errorf("%s still wires the inlined ./src/lib/devlog-plugin, but the runtime floor %s ships forgeDevLog: "+
				"import it from %s/vite and stop scaffolding devlog-plugin.ts (see this test's comment)", rel, floor, WebRuntimePackage)
		case !floorShips && imports:
			t.Errorf("%s imports forgeDevLog, which the runtime floor %s does not export: every new Vite project "+
				"would fail to load vite.config.ts. Keep the inlined devlog-plugin.ts until the floor ships it", rel, floor)
		case !imports && !inlines:
			t.Errorf("%s wires no dev-log receiver at all; installDevLogging() would post into a 404", rel)
		}
	}
}

// viteDevLogScaffolds are the Vite configs forge scaffolds, relative to
// internal/templates/frontend.
var viteDevLogScaffolds = []string{
	"vite-spa/vite.config.ts.tmpl",
	"site/vite/vite.config.ts",
}

var (
	forgeDevLogImportRe = regexp.MustCompile(`import\s*\{[^{}]*\bforgeDevLog\b[^{}]*\}\s*from\s*"@reliantlabs/forge-web-runtime/vite"`)
	inlinedDevLogRe     = regexp.MustCompile(`from\s*"\./src/lib/devlog-plugin"`)
)

// viteDevLogReceiver reports how a vite config wires the dev-log receiver.
func viteDevLogReceiver(config string) (importsRuntime, inlines bool) {
	return forgeDevLogImportRe.MatchString(config), inlinedDevLogRe.MatchString(config)
}

// The scanner, pinned offline: a regex that silently matched nothing would
// make the network test above pass whatever the templates say.
func TestViteDevLogReceiverScanner(t *testing.T) {
	for _, rel := range viteDevLogScaffolds {
		body, err := os.ReadFile(filepath.Join("..", "templates", "frontend", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		imports, inlines := viteDevLogReceiver(string(body))
		if !imports && !inlines {
			t.Errorf("%s: found neither the inlined devlog-plugin import nor forgeDevLog; the scanner drifted", rel)
		}
	}
	imports, inlines := viteDevLogReceiver(`import { forgeDevLog, forgeOtelProxy } from "@reliantlabs/forge-web-runtime/vite";`)
	if !imports || inlines {
		t.Errorf("runtime import misread: imports=%v inlines=%v", imports, inlines)
	}
}
