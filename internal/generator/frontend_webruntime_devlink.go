// Package generator — the DEV bridge from a project's frontends to a local
// @reliantlabs/forge-web-runtime checkout.
//
// This is the npm mirror of what forge already does for Go, and the shape is
// deliberately the same one:
//
//	Go: a gitignored `go.work` bridges the project to a sibling forge/pkg
//	    checkout. go.mod keeps its published pin. Nothing machine-specific is
//	    committed, and a clone that lacks the sibling still builds.
//
//	JS: a gitignored npm workspace ROOT (package.json + .forge-link/) bridges
//	    the project's frontends to a sibling forge/web-runtime checkout. Each
//	    frontend's package.json keeps its published range. Nothing
//	    machine-specific is committed, and a clone that lacks the sibling
//	    still installs.
//
// # Why the bridge cannot simply be a symlink
//
// npm owns node_modules. A bare symlink dropped into
// node_modules/@reliantlabs/ has no corresponding edge in the dependency
// graph, so the next install that reshapes the tree replaces it with the
// registry copy and the developer silently goes back to testing published
// code. The link has to be something npm itself computes.
//
// The previous design got that right and paid for it in the wrong currency:
// it rewrote the specifier to `file:<path>` inside frontends/<name>/package.json,
// a TRACKED file. That left every maintainer's `git status` permanently dirty,
// made the diff meaningless to review, and made it easy to commit a path that
// resolves only on a machine with a sibling forge checkout at exactly that
// depth — which breaks CI and every other developer.
//
// # The mechanism
//
// A workspace root ABOVE the frontends, both parts of it gitignored:
//
//	<project>/package.json      { "workspaces": ["frontends/*", ".forge-link/*"] }
//	<project>/.forge-link/web-runtime -> ../../forge/web-runtime   (symlink)
//
// npm reads the root, sees a workspace member whose package.json declares the
// name @reliantlabs/forge-web-runtime, and hoists it to <project>/node_modules
// as a link. The frontends' own `"^0.3.0"` requirement resolves against that
// member by NAME, so the tracked manifests never mention a path. Node's
// resolution walks up from frontends/<name>/, finds the hoisted link, and lands
// in the live checkout — verified by require.resolve pointing straight into
// forge/web-runtime/dist, and by an edit to that file being visible to the next
// import with no reinstall.
//
// Three properties this buys that the alternatives do not:
//
//   - The tracked package.json AND package-lock.json are both untouched. An
//     in-frontend overlay (workspaces + a link dir inside the frontend) also
//     keeps package.json clean, but npm records the member in the frontend's
//     OWN package-lock.json — measured — so the churn simply moves to the
//     other tracked file. Hoisting to a root the frontend does not own is what
//     keeps both clean.
//   - A stale registry copy already installed under frontends/<name>/node_modules
//     does not shadow the link: the root install REMOVES it (measured: "added 2
//     packages, and removed 5 packages", after which require.resolve pointed at
//     the checkout).
//   - `npm install` run from INSIDE a frontend still honours the root
//     workspace and leaves the nested lockfile byte-identical (measured).
//
// The symlink points at the checkout with a RELATIVE path, and the root
// manifest names no path at all, so — as with `go.work` — even these ignored
// files carry no username and no home directory.
//
// Everything here is gated on a dev build with a discoverable forge source
// root. A released binary writes none of it: the workspace root is a
// maintainer's dev-loop artifact, and scattering one into a user's project
// would change how their install resolves for no benefit.
package generator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// devLinkDir is the project-relative directory holding the workspace-member
// symlinks. Dot-prefixed because it is machine-local scaffolding, not source.
const devLinkDir = ".forge-link"

// devWorkspaceRootManifest is the gitignored root package.json. It is a
// workspace root and nothing else: no dependencies of its own, private so it
// can never be published, and carrying an explanation for whoever finds an
// untracked package.json at their project root and wonders what wrote it.
const devWorkspaceRootManifest = `{
  "name": "forge-dev-workspace-root",
  "private": true,
  "//": [
    "GITIGNORED, machine-local, written by a DEV build of forge. Not part of your project.",
    "This is the npm twin of a gitignored go.work: it bridges this project's frontends to a",
    "local @reliantlabs/forge-web-runtime checkout (symlinked under .forge-link/) so edits in",
    "that checkout are live here, with nothing published and nothing reinstalled.",
    "The frontends' own package.json files keep their published semver range and stay clean.",
    "Delete this file and .forge-link/ to go back to the registry copy; run npm install after."
  ],
  "workspaces": [
    "frontends/*",
    "` + devLinkDir + `/*"
  ]
}
`

// devBridgeIgnoreEntries are the paths forge must ensure are ignored before it
// writes any of them. Ensuring rather than assuming matters: the bridge is
// only safe because it CANNOT be committed, so a project whose .gitignore
// predates this feature must be brought up to date by forge, not by the
// developer noticing.
//
// Each is anchored with a leading "/" so it matches the project root only —
// a bare "package.json" would also ignore every frontend's manifest, which are
// exactly the tracked files this whole change exists to protect.
var devBridgeIgnoreEntries = []string{
	"/package.json",
	"/package-lock.json",
	"/node_modules/",
	"/" + devLinkDir + "/",
}

// EnsureDevWebRuntimeLink reconciles the gitignored dev bridge for projectDir.
//
// No-op unless this is a dev build that can locate its own forge checkout and
// that checkout actually carries web-runtime. Every failure is a warning: a
// bridge is a convenience for forge maintainers, and no dev-loop nicety
// justifies failing somebody's generate.
func EnsureDevWebRuntimeLink(projectDir string) {
	target, ok := devWebRuntimeCheckout()
	if !ok {
		return
	}
	if !hasFrontends(projectDir) {
		return // nothing to bridge
	}

	if err := ensureGitignoreEntries(filepath.Join(projectDir, ".gitignore"), devBridgeIgnoreEntries); err != nil {
		// Refuse to write files we could not first make uncommittable —
		// that is the failure mode this change exists to eliminate.
		fmt.Fprintf(os.Stderr, "warning: could not ignore the dev %s bridge in %s (%v); bridge not written\n",
			WebRuntimePackage, projectDir, err)
		return
	}

	if err := writeIfMissing(filepath.Join(projectDir, "package.json"), devWorkspaceRootManifest); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write the dev workspace root in %s: %v\n", projectDir, err)
		return
	}

	link := filepath.Join(projectDir, devLinkDir, "web-runtime")
	changed, err := ensureRelativeSymlink(link, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not link %s into %s: %v\n", WebRuntimePackage, link, err)
		return
	}
	if changed {
		fmt.Printf("🔗 Dev forge build: %s is bridged to the local checkout via %s/ "+
			"(gitignored; your frontends' package.json keeps %s). Run `npm install` at the project root.\n",
			WebRuntimePackage, devLinkDir, webRuntimePublishedRange)
	}
}

// devWebRuntimeCheckout returns the absolute web-runtime directory this dev
// build should bridge to, and whether a bridge is warranted at all.
func devWebRuntimeCheckout() (string, bool) {
	if !buildinfo.IsDevBuild() {
		return "", false
	}
	root := buildinfo.DevForgeRoot
	if root == "" {
		root = buildinfo.DiscoverDevForgeRootFromSource()
	}
	if root == "" {
		return "", false
	}
	target := filepath.Join(root, "web-runtime")
	if _, err := os.Stat(filepath.Join(target, "package.json")); err != nil {
		return "", false
	}
	return target, true
}

// hasFrontends reports whether the project has a frontends/ directory with at
// least one entry — the only shape where a web-runtime bridge means anything.
func hasFrontends(projectDir string) bool {
	entries, err := os.ReadDir(filepath.Join(projectDir, "frontends"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
	}
	return false
}

// ensureRelativeSymlink makes linkPath a symlink to targetDir, expressed
// relative to the link's own directory so the ignored file still names no
// absolute path. Reports whether anything changed, so a re-run is silent.
func ensureRelativeSymlink(linkPath, targetDir string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return false, err
	}
	rel, err := filepath.Rel(resolvePath(filepath.Dir(linkPath)), resolvePath(targetDir))
	if err != nil {
		rel = targetDir // absolute fallback: still gitignored, still correct
	}
	rel = filepath.ToSlash(rel)

	switch existing, err := os.Readlink(linkPath); {
	case err == nil && existing == rel:
		return false, nil // already correct
	case err == nil:
		// Points somewhere else (the checkout moved): replace it.
		if err := os.Remove(linkPath); err != nil {
			return false, err
		}
	case errors.Is(err, os.ErrNotExist):
		// Nothing there yet.
	default:
		// Exists but is not a symlink — a real directory somebody put here.
		// Leave it alone rather than deleting a developer's files.
		return false, fmt.Errorf("%s exists and is not a symlink", linkPath)
	}
	return true, os.Symlink(rel, linkPath)
}

// ensureGitignoreEntries appends any of want that the file does not already
// ignore, under a short explanatory header. Creates the file when absent.
func ensureGitignoreEntries(path string, want []string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body := string(existing)

	var missing []string
	for _, entry := range want {
		if !gitignoreHasEntry(body, entry) {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString(body)
	if body != "" && !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(`
# ── Dev web-runtime bridge (machine-local; NEVER commit) ──
# A dev build of forge writes an npm workspace ROOT here that links this
# project's frontends to a local @reliantlabs/forge-web-runtime checkout —
# the npm twin of a gitignored go.work. The frontends' own package.json files
# keep their published semver range, so nothing machine-specific is tracked.
# A committed root manifest would change how every other clone and CI resolves
# the runtime, which is exactly what these entries prevent.
`)
	for _, entry := range missing {
		b.WriteString(entry + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// gitignoreHasEntry reports whether body already ignores entry, ignoring
// comments, blank lines and surrounding whitespace. It also accepts the
// unanchored spelling ("package.json" for "/package.json") — a project that
// already ignores it more broadly needs nothing added.
func gitignoreHasEntry(body, entry string) bool {
	bare := strings.Trim(entry, "/")
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == entry || line == bare || line == bare+"/" || line == "/"+bare {
			return true
		}
	}
	return false
}
