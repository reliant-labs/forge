// Package webruntimepeers is the SINGLE source of truth for which packages
// must resolve to exactly one copy when a project links
// @reliantlabs/forge-web-runtime.
//
// WHY THIS PACKAGE EXISTS. The list previously lived in four places kept in
// step only by comments that said "keep the list in step with…":
//
//   - internal/cli/generate_frontend_tsconfig_peers.go  (tsconfig `paths` pins)
//   - internal/cli/generate_frontend_dedupe.go          (vitest resolve.dedupe)
//   - internal/templates/frontend/vite-spa/tsconfig.json.tmpl
//   - internal/templates/frontend/nextjs/tsconfig.json.tmpl
//
// They had ALREADY drifted. Measured against the runtime's real
// peerDependencies, the tsconfig list was missing 9 packages (every
// @opentelemetry/* except api, plus react) and the vitest list was missing 9
// (including @opentelemetry/api). A comment is not a mechanism.
//
// THE SOURCE OF TRUTH IS web-runtime/package.json's `peerDependencies`. That
// is the runtime's own declaration of "the consuming app supplies this copy",
// so it is the same fact the dedupe/pin lists are trying to state. The
// scaffolded vite.config.ts already derived its list that way at RUNTIME
// (Object.keys of the runtime's peerDependencies); this package gives the Go
// side the same derivation, so a peer added to the runtime is picked up by
// every consumer instead of by whichever list someone remembered.
//
// The embedded copy mirrors internal/buildinfo's VERSION pattern: go:embed
// cannot reach outside its own package directory, so peers.json is kept
// byte-equivalent to the runtime's peerDependencies by
// TestEmbeddedPeersMatchWebRuntime.
package webruntimepeers

import (
	_ "embed"
	"encoding/json"
	"sort"
)

//go:embed peers.json
var peersJSON []byte

// extraTypeOnlyPins are packages that are NOT peerDependencies of the runtime
// but must still resolve to one copy for TYPE identity.
//
// @tanstack/query-core: @tanstack/react-query re-exports its types FROM
// query-core, so pinning only the wrapper leaves the underlying copy split and
// tsc reports "Property #private in type Query refers to a different member".
// The runtime depends on react-query and never names query-core, so it can
// never appear in peerDependencies — it is a transitive type identity, which
// is exactly the kind of fact a derived list cannot discover on its own.
var extraTypeOnlyPins = []string{
	"@tanstack/query-core",
}

// extraBundlerDedupe are packages that must be deduped by a BUNDLER but are
// not type-identity concerns.
//
// react-dom: React's runtime pairs with react and two copies break hooks at
// runtime (mismatched dispatcher) while typechecking perfectly green. It is
// not in the runtime's peerDependencies because the runtime imports react, not
// react-dom — but a project that ends up with two react-doms has the same
// class of failure, so the bundler list carries it.
var extraBundlerDedupe = []string{
	"react-dom",
}

// typesOnlyDir maps a package whose TYPES ship separately, under @types/, to
// that types package. A tsconfig `paths` entry is a full override of module
// resolution, not a hint: once "react" maps to ./node_modules/react, tsc looks
// there and NOWHERE else, so it finds index.js, finds no bundled .d.ts, and
// never consults @types/react the way it would have unaided:
//
//	error TS7016: Could not find a declaration file for module 'react'.
//	'…/node_modules/react/index.js' implicitly has an 'any' type.
//
// Every other pinned package bundles its own typings, so pointing at the
// implementation directory is right for them and wrong only here. Pointing
// react's entry at @types/react keeps the dedupe intent — one copy of the type
// — while giving tsc a directory that actually declares them.
//
// Reproduced in isolation: with target/lib set and real react + @types/react
// installed, `"react": ["./node_modules/react"]` fails with the TS7016 above
// and `"react": ["./node_modules/@types/react"]` typechecks clean.
var typesOnlyDir = map[string]string{
	"react": "@types/react",
}

// The two node_modules layouts a forge frontend can have, and the reason a pin
// must name the RIGHT one rather than listing both.
//
// A `paths` entry whose key has no `*` wildcard must be an array of EXACTLY
// ONE element. TypeScript itself tolerates more and falls through to the next
// candidate, but Next.js resolves imports with SWC, whose tsc-compatible
// resolver asserts the single-element rule and PANICS the whole build:
//
//	thread '<unnamed>' panicked at swc_ecma_loader/src/resolvers/tsc.rs:82:21:
//	assertion `left == right` failed: value of `paths.@bufbuild/protobuf`
//	should be an array with one element because the src path does not
//	contains * (wildcard)
//
// So "list both and let the toolchain pick" is not available: it typechecks
// green under tsc and takes `next build` down. The pin has to be resolved to
// the layout the project actually has.
//
// WHICH LAYOUT, AND WHY IT VARIES. Ordinarily each frontend installs its own
// dependencies and localPinPrefix is right. But forge's dev bridge makes the
// project an npm WORKSPACE ROOT (internal/generator/frontend_webruntime_devlink.go),
// and npm hoists every member's dependencies to the root — in a bridged
// project frontends/<name>/node_modules does not exist at all. A pin left
// naming the frontend-local path then points at nothing, which tsc does not
// report as an error: the pin silently no-ops, resolution falls back to the
// ordinary upward walk, and it finds the LINKED runtime's own private copies
// first, producing exactly the duplicate-identity failure the pins exist to
// prevent:
//
//	src/lib/mock-transport_gen.ts(54,3): error TS2322: Type
//	'…/forge/web-runtime/node_modules/@connectrpc/connect/…'.Transport is not
//	assignable to type '…/<project>/node_modules/@connectrpc/connect/…'.Transport
//
// Measured in a scaffolded bridged project: frontend-local pin → 2 TS2322
// errors; hoisted pin → 0, with no tsc setting relaxed.
const (
	// localPinPrefix — the frontend installed its own dependencies.
	localPinPrefix = "./node_modules/"
	// hoistedPinPrefix — an npm workspace hoisted them to the project root,
	// two levels up from frontends/<name>/.
	hoistedPinPrefix = "../../node_modules/"
)

// TypePinTarget returns the node_modules-relative directory a tsconfig `paths`
// entry for name should resolve to. It is the package itself for anything that
// bundles its typings, and the @types/ package for anything that does not.
func TypePinTarget(name string) string {
	if types, ok := typesOnlyDir[name]; ok {
		return types
	}
	return name
}

// TypePinPath returns the single `paths` value for one pinned package, for a
// frontend whose dependencies are hoisted to the project root when hoisted is
// true and installed locally otherwise.
//
// Exactly one element, never two — see the layout constants above for why a
// candidate list panics `next build`. Every emitter goes through this so the
// layout decision lands in one place rather than in each template's inline
// string.
func TypePinPath(name string, hoisted bool) string {
	prefix := localPinPrefix
	if hoisted {
		prefix = hoistedPinPrefix
	}
	return prefix + TypePinTarget(name)
}

// decodePeers returns the runtime's declared peer dependency names.
func decodePeers() []string {
	var doc struct {
		PeerDependencies map[string]string `json:"peerDependencies"`
	}
	if err := json.Unmarshal(peersJSON, &doc); err != nil {
		return nil
	}
	out := make([]string, 0, len(doc.PeerDependencies))
	for name := range doc.PeerDependencies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// merged returns peers plus extra, deduplicated and sorted, so callers get a
// stable order and a generated file does not churn between runs.
func merged(extra ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(names []string) {
		for _, n := range names {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	add(decodePeers())
	for _, e := range extra {
		add(e)
	}
	sort.Strings(out)
	return out
}

// TypePins is the set for TypeScript `paths` pinning: every runtime peer plus
// the transitive type-identity packages. Two copies of any of these makes tsc
// report two nominally-distinct versions of the same type — the TS2322
// "Type Transport is not assignable to type Transport" failure.
func TypePins() []string { return merged(extraTypeOnlyPins) }

// BundlerDedupe is the set for a bundler's dedupe/alias list: every runtime
// peer plus the runtime-only pairings. Two copies here ship the library twice
// and split its module-level state, which builds and typechecks green.
func BundlerDedupe() []string { return merged(extraBundlerDedupe) }
