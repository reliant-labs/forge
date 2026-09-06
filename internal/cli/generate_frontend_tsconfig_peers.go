package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/webruntimepeers"
)

// The tsconfig `paths` peer-pin reconcile, for projects that predate it.
//
// WHY THIS EXISTS SEPARATELY FROM THE TEMPLATE. Both frontend tsconfig
// templates already pin the runtime's peerDependencies to the app's own copy
// (see the "peer dedupe note" in internal/templates/frontend/*/tsconfig.json.tmpl,
// pinned by TestFrontendTsconfigDedupesRuntimePeers). But tsconfig.json is
// scaffold-once — written at birth and never rewritten — so a project
// generated before that template change never receives it, and no amount of
// regenerating will hand it over.
//
// The hazard is forge's own: a dev build bridges @reliantlabs/forge-web-runtime
// with a `file:` specifier, which npm materialises as a SYMLINK into the forge
// checkout. That checkout carries its own node_modules, and npm cannot hoist a
// linked package's dependencies away, so the runtime's bare imports resolve
// THERE while the app's resolve here. Two physically distinct installs of the
// same version are two nominally distinct types to TypeScript:
//
//	src/lib/mock-transport_gen.ts(155,3): error TS2322: Type
//	'…/forge/web-runtime/node_modules/@connectrpc/connect/…'.Transport is not
//	assignable to type '…/frontends/internal-console/node_modules/…'.Transport
//
// The bundler configs (vite's resolve.dedupe, next.config.ts's resolver rule)
// already force one copy, and vitest.config.ts is reconciled by
// generate_frontend_dedupe.go — but none of those run under `tsc --noEmit`,
// which is what `forge lint` invokes. The failure therefore lands in a file
// forge GENERATED (mock-transport_gen.ts), caused by a dependency edge forge
// INTRODUCED, in a project that did nothing wrong. That is the same
// justification that lets reconcileFrontendDedupe touch a user-owned file, and
// this pass is held to the same limits: it adds only the missing peer keys to
// the existing paths block, changes nothing else, and does nothing at all when
// they are already present.
//
// A registry (non-dev) install hoists one copy and never had the problem, so
// these entries are inert there. They are written unconditionally anyway: the
// same project is bridged and unbridged at different times depending on who
// built the forge binary, and a config that only typechecks under one of them
// is worse than one that works under both.

// tsconfigPeerPins are the packages that must resolve to exactly one copy —
// the app's.
//
// DERIVED, not hand-written. This used to be a literal list "kept in step
// with" three other literal lists, and it had already drifted: it was missing
// nine real peers (every @opentelemetry/* except api, plus react). See
// internal/webruntimepeers for the source of truth.
func tsconfigPeerPins() []string { return webruntimepeers.TypePins() }

// pathsOpenRe matches the opening of the `"paths": {` object inside
// compilerOptions — the insertion point. Anchored on the quoted key so it
// cannot match a "paths" string appearing in a note or a value.
var pathsOpenRe = regexp.MustCompile(`(?m)^(\s*)"paths"\s*:\s*\{`)

// reconcileFrontendTsconfigPeers adds the missing peer pins to each web
// frontend's tsconfig.json.
//
// Best-effort and non-fatal throughout: a frontend with no tsconfig, an
// unreadable file, or one whose `paths` block this cannot find is skipped
// rather than failed. Idempotent — a config that already carries every pin is
// left byte-identical, which is what keeps `forge generate` twice in a row
// reporting no changes.
func reconcileFrontendTsconfigPeers(cfg *config.ProjectConfig, projectDir string) {
	if cfg == nil {
		return
	}

	var touched []string
	for _, fe := range cfg.Frontends {
		// Only the browser frontends link the web runtime and emit
		// mock-transport_gen.ts, which is where the two copies meet.
		if !isWebFrontendType(fe.Type) {
			continue
		}
		feDir, ok := fe.Dir(projectDir)
		if !ok {
			// No directory in this repository — a cross-repo
			// source pin, or a path outside the project root.
			continue
		}
		rel := filepath.Join(feDir, "tsconfig.json")
		// Which node_modules this frontend's deps actually live in is a
		// property of the tree on disk, not of the config: an npm workspace
		// (forge's dev bridge writes one) hoists them to the project root and
		// leaves the frontend-local directory absent. Read it per frontend,
		// because a project can mix layouts.
		hoisted := frontendDepsAreHoisted(projectDir, filepath.Join(projectDir, feDir))
		if addPeerPinsToTsconfig(filepath.Join(projectDir, rel), hoisted) {
			touched = append(touched, filepath.ToSlash(rel))
		}
	}

	if len(touched) > 0 {
		fmt.Printf("  ♻️  pinned web-runtime peers in %d tsconfig(s) — one copy of @connectrpc/@bufbuild under tsc\n", len(touched))
		for _, rel := range touched {
			fmt.Printf("      - %s\n", rel)
		}
	}
}

// frontendDepsAreHoisted reports whether feDir's dependencies resolve from the
// PROJECT ROOT's node_modules rather than the frontend's own — the npm
// workspace layout, which is what forge's dev bridge creates.
//
// The question is answered from the DECLARATION first and the installed tree
// only second, and that ordering is the point. A workspace root says npm WILL
// hoist; whether it has done so yet depends on when someone last ran an
// install, which is not something generate can order. Reading the tree alone
// made the pin correct or stale depending on that timing — a scaffold whose
// `npm install` ran after the last generate kept frontend-local pins that no
// longer resolved, and failed tsc with the TS2322 this pass exists to prevent.
//
// A nested install still wins when one genuinely exists: node resolution
// prefers the nearest node_modules, so the pin must too.
func frontendDepsAreHoisted(projectDir, feDir string) bool {
	// Probe a specific PACKAGE, not the node_modules directory. An npm
	// workspace routinely leaves a frontend with a partial node_modules
	// holding only what could not be hoisted (a vite-spa frontend keeps
	// esbuild and vite locally while the peers live at the root), so "the
	// directory exists" does not mean "the peer is here" — and pinning at a
	// directory that lacks the package resolves to nothing, which tsc answers
	// by binding the linked runtime's copy instead.
	probe := filepath.FromSlash(pinLayoutProbePackage)
	if dirExists(filepath.Join(feDir, "node_modules", probe)) {
		return false // really installed here — nearest wins
	}
	if dirExists(filepath.Join(projectDir, "node_modules", probe)) {
		return true // hoisted to the root, where the pin must point
	}
	// Nothing installed yet: believe the declaration, since a workspace root
	// means npm WILL hoist.
	return projectDeclaresFrontendWorkspace(projectDir)
}

// pinLayoutProbePackage is the package the layout decision is probed against —
// a required (non-optional) peer of the runtime, so it is present in every
// real install.
const pinLayoutProbePackage = "@connectrpc/connect"

// projectDeclaresFrontendWorkspace reports whether the project root's
// package.json is an npm workspace root covering frontends/*. Forge's dev
// bridge writes exactly that (gitignored) manifest; a user may equally have
// one of their own, and both hoist identically.
func projectDeclaresFrontendWorkspace(projectDir string) bool {
	body, err := os.ReadFile(filepath.Join(projectDir, "package.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Workspaces []string `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		// A workspaces OBJECT form ({"packages": [...]}) fails this decode.
		// Fall back to the tree rather than guessing at a shape forge did not
		// write.
		return false
	}
	for _, pattern := range manifest.Workspaces {
		if strings.HasPrefix(pattern, "frontends/") {
			return true
		}
	}
	return false
}

// addPeerPinsToTsconfig reconciles one tsconfig's `paths` peer pins: it adds
// any that are missing and RETARGETS any that name the wrong node_modules for
// this project's layout. Returns true when the file changed.
//
// Retargeting matters as much as adding. tsconfig.json is scaffold-once, so a
// project whose layout changed — most commonly because a dev forge build wrote
// the workspace bridge and npm hoisted — keeps pins aimed at a directory that
// no longer exists. tsc does not report a pin that resolves to nothing; it
// silently falls back to the ordinary walk and finds the linked runtime's own
// copy, which is the TS2322 this pass exists to prevent.
func addPeerPinsToTsconfig(path string, hoisted bool) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false // no tsconfig here — nothing to reconcile
	}

	// First pass: retarget pins that are present but aimed at the other
	// layout. Done before the insert so `missing` below sees the final shape.
	retargeted := false
	for _, pkg := range tsconfigPeerPins() {
		want := webruntimepeers.TypePinPath(pkg, hoisted)
		re := pathsEntryRe(pkg)
		loc := re.FindSubmatchIndex(body)
		if loc == nil {
			continue // absent, or a shape this does not own — leave it
		}
		if string(body[loc[4]:loc[5]]) == want {
			continue // already correct
		}
		body = re.ReplaceAll(body, []byte(`${1}"`+want+`"${3}`))
		retargeted = true
	}

	missing := make([]string, 0, len(tsconfigPeerPins()))
	for _, pkg := range tsconfigPeerPins() {
		if !pathsKeyRe(pkg).Match(body) {
			missing = append(missing, pkg)
		}
	}
	if len(missing) == 0 {
		if !retargeted {
			return false // already handled
		}
		return writeTsconfig(path, body)
	}

	loc := pathsOpenRe.FindSubmatchIndex(body)
	if loc == nil {
		// A tsconfig with no `paths` block, or one restructured past
		// recognition. Forge does not invent a compilerOptions key in a file
		// it does not own — a `paths` block also requires `baseUrl` under
		// some configurations, and guessing there breaks more than it fixes.
		return false
	}
	// loc[2]:loc[3] is the indentation captured before `"paths"`; the new
	// entries sit one level deeper, matching the block they are joining.
	indent := string(body[loc[2]:loc[3]])
	insertAt := loc[1] // just past the `{`

	// Each inserted line is prefixed with "\n" rather than suffixed, so the
	// bytes already at insertAt keep the newline that followed the `{` and the
	// block's existing first entry stays where it was.
	var out bytes.Buffer
	out.Write(body[:insertAt])
	out.WriteString("\n" + indent + "  // Added by forge: a dev forge build links @reliantlabs/forge-web-runtime")
	out.WriteString("\n" + indent + "  // by path, so its peerDependencies would otherwise resolve from the")
	out.WriteString("\n" + indent + "  // link target's node_modules. tsc sees two distinct copies of the same")
	out.WriteString("\n" + indent + "  // type and fails mock-transport_gen.ts with TS2322. Pin them here.")
	for _, pkg := range missing {
		// The key is the import specifier; the VALUE is where its typings
		// live, which differs for packages typed by a separate @types/
		// package. Emitting the implementation dir for react pins tsc to a
		// directory with no .d.ts and fails every .tsx with TS7016.
		//
		// Exactly ONE element: SWC panics `next build` on a multi-element
		// value for a non-wildcard key, so the pin names the one layout this
		// project has rather than listing both. See
		// webruntimepeers.TypePinPath.
		fmt.Fprintf(&out, "\n%s  %q: [%q],", indent, pkg, webruntimepeers.TypePinPath(pkg, hoisted))
	}
	out.Write(body[insertAt:])

	return writeTsconfig(path, out.Bytes())
}

// writeTsconfig writes body back to path, preserving the file's own mode.
func writeTsconfig(path string, body []byte) bool {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, body, mode) == nil
}

// pathsEntryRe matches a single-element `paths` mapping for pkg, capturing the
// key-and-bracket prefix in group 1 and the bare path VALUE in group 2 so a
// rewrite can replace the value alone.
//
// Deliberately matches only the single-element form. A hand-written entry with
// several candidates, or one spread over multiple lines, is a shape forge did
// not write and does not rewrite.
func pathsEntryRe(pkg string) *regexp.Regexp {
	// Group 3 carries the closing bracket and any whitespace before it, so a
	// rewrite can put back exactly what it matched. Dropping it produced a
	// truncated entry and an unparseable tsconfig.
	return regexp.MustCompile(`("` + regexp.QuoteMeta(pkg) + `"\s*:\s*\[\s*)"([^"]*)"(\s*\])`)
}

// pathsKeyRe matches an existing mapping for pkg anywhere in the file — the
// signal that this config already pins it (either from a current scaffold or
// from a previous run of this pass).
func pathsKeyRe(pkg string) *regexp.Regexp {
	return regexp.MustCompile(`"` + regexp.QuoteMeta(pkg) + `"\s*:\s*\[`)
}
