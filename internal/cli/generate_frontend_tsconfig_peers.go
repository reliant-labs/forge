package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

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
		if addPeerPinsToTsconfig(filepath.Join(projectDir, rel)) {
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

// addPeerPinsToTsconfig inserts any missing pin into one tsconfig's `paths`
// object. Returns true when the file changed.
func addPeerPinsToTsconfig(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false // no tsconfig here — nothing to reconcile
	}

	missing := make([]string, 0, len(tsconfigPeerPins()))
	for _, pkg := range tsconfigPeerPins() {
		if !pathsKeyRe(pkg).Match(body) {
			missing = append(missing, pkg)
		}
	}
	if len(missing) == 0 {
		return false // already handled
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
		fmt.Fprintf(&out, "\n%s  %q: [%q],", indent, pkg, "./node_modules/"+pkg)
	}
	out.Write(body[insertAt:])

	info, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, out.Bytes(), mode) == nil
}

// pathsKeyRe matches an existing mapping for pkg anywhere in the file — the
// signal that this config already pins it (either from a current scaffold or
// from a previous run of this pass).
func pathsKeyRe(pkg string) *regexp.Regexp {
	return regexp.MustCompile(`"` + regexp.QuoteMeta(pkg) + `"\s*:\s*\[`)
}
