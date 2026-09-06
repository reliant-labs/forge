package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/webruntimepeers"
)

// Retargeting a frontend's tsconfig peer pins at the node_modules layout the
// project ACTUALLY has, after an install has decided that layout.
//
// WHY THIS LIVES HERE AND NOT ONLY IN THE GENERATE PIPELINE. `forge generate`
// runs the same reconcile, but the `forge scaffold frontend` verb finishes
// with an `npm install` — and that install is what decides the layout. The
// tsconfig it wrote moments earlier had to guess. When the project is an npm
// workspace (forge's dev bridge makes it one) npm hoists the dependencies to
// the project root and creates no frontends/<name>/node_modules at all, so the
// pins name a directory that does not exist.
//
// A pin that resolves to nothing is not an error tsc reports. It silently
// no-ops, resolution falls back to the ordinary upward walk, and that finds
// the LINKED runtime's own private copy of @connectrpc/connect first:
//
//	src/lib/mock-transport_gen.ts(54,3): error TS2322: Type
//	'…/forge/web-runtime/node_modules/@connectrpc/connect/…'.Transport is not
//	assignable to type '…/<project>/node_modules/@connectrpc/connect/…'.Transport
//
// So a freshly added frontend failed its very first `tsc --noEmit`, before the
// user had a chance to run generate again.

// tsconfigPinEntryRe matches a single-element `paths` mapping for pkg,
// capturing the key-and-bracket prefix, the bare path value, and the closing
// bracket, so a rewrite can replace the value and put the rest back verbatim.
func tsconfigPinEntryRe(pkg string) *regexp.Regexp {
	return regexp.MustCompile(`("` + regexp.QuoteMeta(pkg) + `"\s*:\s*\[\s*)"([^"]*)"(\s*\])`)
}

// ReconcileFrontendTsconfigPeers retargets every frontend's tsconfig peer pins
// to the node_modules layout now on disk. Best-effort and non-fatal: a missing
// or unrecognised tsconfig is skipped rather than failed, and a file already
// correct is left byte-identical so a re-run reports nothing.
func ReconcileFrontendTsconfigPeers(projectDir string) {
	entries, err := os.ReadDir(filepath.Join(projectDir, "frontends"))
	if err != nil {
		return
	}
	var touched []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		feDir := filepath.Join(projectDir, "frontends", entry.Name())
		path := filepath.Join(feDir, "tsconfig.json")
		if retargetTsconfigPins(path, frontendPinsAreHoisted(projectDir, feDir)) {
			touched = append(touched, entry.Name())
		}
	}
	if len(touched) > 0 {
		fmt.Printf("  ♻️  retargeted web-runtime peer pins for %s (npm hoisted this project's dependencies)\n",
			strings.Join(touched, ", "))
	}
}

// pinLayoutProbe is the package the layout decision is made against. Any
// pinned peer would do; this one is a required (non-optional) peer of the
// runtime, so it is present in every real install.
const pinLayoutProbe = "@connectrpc/connect"

// frontendPinsAreHoisted reports whether feDir's PEER dependencies resolve
// from the project root rather than the frontend's own node_modules.
//
// The question is asked about a specific package, not about the existence of a
// node_modules DIRECTORY, and that distinction is the whole fix. An npm
// workspace routinely leaves a frontend with a partial node_modules holding
// only the packages that could not be hoisted — a vite-spa frontend gets
// esbuild and vite locally while @connectrpc/connect and @bufbuild/protobuf
// live at the root. Treating "the directory exists" as "the deps are here"
// pinned those peers at a directory that does not contain them; the pin then
// resolved to nothing, tsc fell back to the ordinary walk, and it bound the
// linked runtime's own copy — TS2322, in a frontend that had just been added.
//
// A genuine nested copy still wins, because node resolution prefers the
// nearest.
func frontendPinsAreHoisted(projectDir, feDir string) bool {
	probe := filepath.FromSlash(pinLayoutProbe)
	if isDir(filepath.Join(feDir, "node_modules", probe)) {
		return false // really installed here — nearest wins
	}
	if isDir(filepath.Join(projectDir, "node_modules", probe)) {
		return true // hoisted to the root, where the pin must point
	}
	// Nothing installed yet either way: believe the declaration, since a
	// workspace root means npm WILL hoist once someone installs.
	return rootDeclaresFrontendWorkspace(projectDir)
}

// rootDeclaresFrontendWorkspace reports whether the project root's
// package.json is an npm workspace root covering frontends/*.
func rootDeclaresFrontendWorkspace(projectDir string) bool {
	body, err := os.ReadFile(filepath.Join(projectDir, "package.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Workspaces []string `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return false
	}
	for _, pattern := range manifest.Workspaces {
		if strings.HasPrefix(pattern, "frontends/") {
			return true
		}
	}
	return false
}

// retargetTsconfigPins rewrites each present peer pin in path to the layout
// hoisted selects. Reports whether the file changed.
func retargetTsconfigPins(path string, hoisted bool) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	changed := false
	for _, pkg := range webruntimepeers.TypePins() {
		want := webruntimepeers.TypePinPath(pkg, hoisted)
		re := tsconfigPinEntryRe(pkg)
		loc := re.FindSubmatchIndex(body)
		if loc == nil || string(body[loc[4]:loc[5]]) == want {
			continue
		}
		body = re.ReplaceAll(body, []byte(`${1}"`+want+`"${3}`))
		changed = true
	}
	if !changed {
		return false
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, body, mode) == nil
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
