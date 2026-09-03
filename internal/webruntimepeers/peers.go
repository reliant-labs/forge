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
