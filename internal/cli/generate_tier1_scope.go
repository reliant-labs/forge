// Tier-1 stomp-guard scoping.
//
// The pre-pipeline stomp guard (stepCheckTier1Drift) used to fail on
// ANY drifted Tier-1 file in `.forge/checksums.json`, even when the
// current `forge generate` invocation would never re-emit that file.
// In multi-lane migrations that hard-failed the guard for sibling work
// (e.g. agent A is porting internal/proxy/ and the guard rejected
// because agent B left pkg/app/migrate.go drifted in a separate
// changeset). FRICTION 2026-06-02: cp-forge dogfood pass.
//
// Resolution: the guard now filters drift to the set of paths whose
// owning emitter step would actually run for this pipelineContext. A
// drifted file emitted by a step whose Gate returns false this run is
// silently ignored — that step wouldn't touch the file, so its drift
// cannot manifest as a stomp.
//
// The path → owning-gate registry is intentionally explicit (small map
// at the bottom of this file). Adding a new Tier-1 emitter SHOULD add
// an entry here so the scoping logic stays accurate; the registry is
// fail-open (an unmapped path falls through to "in scope" so missing
// entries err on the safe side of preserving the loud-fail behavior).
//
// The registry is keyed by exact path or trailing-slash prefix. The
// matcher walks the entries in declaration order and returns the first
// match, so order from most-specific to least-specific.
package cli

import "strings"

// tier1OwnerGate returns the predicate function that decides whether
// the emitter for relPath would actually run for the current pipeline
// context. A nil return means "no registered owner" — the caller should
// treat this as in-scope (fail-closed for unknown paths so newly added
// emitters don't accidentally silence drift).
func tier1OwnerGate(relPath string) func(*pipelineContext) bool {
	for _, e := range tier1OwnerRegistry {
		if e.match(relPath) {
			return e.gate
		}
	}
	return nil
}

// tier1OwnerEntry pairs a path matcher with the gate function controlling
// the step that emits the file. `prefix` is matched as either an exact
// path or a directory-with-trailing-slash. Exactly one of `exact` or
// `prefix` is set per entry.
type tier1OwnerEntry struct {
	exact  string
	prefix string
	gate   func(*pipelineContext) bool
}

func (e tier1OwnerEntry) match(relPath string) bool {
	if e.exact != "" {
		return relPath == e.exact
	}
	if e.prefix != "" {
		return strings.HasPrefix(relPath, e.prefix)
	}
	return false
}

// tier1OwnerRegistry maps Tier-1 file paths (or prefixes) to the gate
// that controls their emitter step. Order matters — entries are tried
// most-specific first.
//
// Coverage is intentionally narrow: only the FRICTION-surfaced classes
// of file (pkg/app/* and db/embed.go) are registered. Other Tier-1
// paths fall through to "no registered owner" and remain in scope so
// the stomp guard fails loudly on drift, preserving the original
// behavior. As new friction surfaces, add the relevant emitter here.
var tier1OwnerRegistry = []tier1OwnerEntry{
	// pkg/app/migrate.go is emitted by stepBootstrapMigrate, gated on
	// the project having a database driver configured. A project
	// without a driver shouldn't see the migrate.go drift block its
	// `forge generate` runs.
	{exact: "pkg/app/migrate.go", gate: gateMigrateHasDriver},

	// db/embed.go is emitted alongside migrate.go (same step), gated
	// on the same predicate.
	{exact: "db/embed.go", gate: gateMigrateHasDriver},

	// pkg/app/bootstrap.go + pkg/app/testing.go are emitted by
	// stepBootstrap / stepBootstrapTesting. Both are gated on the
	// project having at least one entrypoint (services, workers,
	// operators). A pure-CLI project shouldn't see these blocking
	// the guard.
	{exact: "pkg/app/bootstrap.go", gate: gateCodegenHasAnyEntrypoint},
	{exact: "pkg/app/testing.go", gate: gateCodegenHasAnyEntrypoint},
}

// filterTier1DriftInScope returns the subset of `drift` whose owning
// emitter would run in the current pipeline context. Unknown paths
// (no entry in tier1OwnerRegistry) are passed through unchanged —
// fail-closed for the registry's blind spots.
func filterTier1DriftInScope[T any](ctx *pipelineContext, drift []T, path func(T) string) (inScope, outOfScope []T) {
	for _, d := range drift {
		gate := tier1OwnerGate(path(d))
		if gate == nil {
			inScope = append(inScope, d)
			continue
		}
		if gate(ctx) {
			inScope = append(inScope, d)
		} else {
			outOfScope = append(outOfScope, d)
		}
	}
	return inScope, outOfScope
}
