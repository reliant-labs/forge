// Fork-coherence groups.
//
// Some Tier-1 files are rendered as a coordinated set: they reference
// each other's generated symbols, so they are only correct *together*,
// at the same render generation. The canonical example is the pkg/app/
// wiring set — app_gen.go declares struct fields that bootstrap.go
// populates, wire_gen.go constructs deps that testing.go re-wires for
// the test harness.
//
// Forking ONE member of such a set is a time bomb: the forked file is
// frozen while its siblings keep regenerating with new symbols. That is
// exactly the .forge/backlog.md 2026-06-03 failure — forked
// bootstrap.go, regenerating app_gen.go grew a `Workers *Workers`
// field, build broke, and even `--force` couldn't fix it because fork
// is stickier than --force.
//
// The registry below names these sets so the pipeline can (a) warn at
// fork time ("you're forking one member of a set"), and (b) warn at
// generate time when a non-forked sibling's render changed while a
// forked member stayed frozen. We deliberately do NOT auto-fork the
// whole group — freezing more files makes the regeneration-loss problem
// worse, not better. Loud warnings + the unfork --merge reconcile path
// are the remedy.
//
// The AST-level complement is the forked-sibling dangling-reference
// check (internal/cli/generate_dangling_check.go), which catches the
// subset of incoherence visible as an unresolvable type name. The group
// warning here is coarser but fires earlier: at fork time and on any
// changed render, before the breakage is observable in the AST.
package checksums

import "path"

// CoherenceGroup is a named set of path patterns whose rendered files
// share generated symbols and must stay at the same render generation.
type CoherenceGroup struct {
	// Name is the stable group identifier recorded on forked entries
	// (FileChecksumEntry.Group) and printed in warnings.
	Name string
	// Patterns are project-relative, slash-separated path.Match
	// patterns. Plain paths (no metacharacters) match exactly.
	Patterns []string
}

// coherenceGroups is the registry. Keep groups small and only add sets
// with real cross-file symbol coupling — every member added here makes
// fork-time warnings noisier.
var coherenceGroups = []CoherenceGroup{
	{
		Name: "app-wiring",
		Patterns: []string{
			"pkg/app/bootstrap.go",
			"pkg/app/app_gen.go",
			"pkg/app/wire_gen.go",
			"pkg/app/testing.go",
		},
	},
}

// CoherenceGroups returns the registry. Callers must not mutate the
// returned slice.
func CoherenceGroups() []CoherenceGroup { return coherenceGroups }

// Matches reports whether relPath belongs to the group. relPath is
// project-relative with forward slashes (the checksum-manifest key
// shape).
func (g CoherenceGroup) Matches(relPath string) bool {
	for _, pat := range g.Patterns {
		if ok, err := path.Match(pat, relPath); err == nil && ok {
			return true
		}
	}
	return false
}

// SiblingPatterns returns the group's patterns excluding the one that
// matches relPath — i.e. "the other members" for warning messages.
func (g CoherenceGroup) SiblingPatterns(relPath string) []string {
	var out []string
	for _, pat := range g.Patterns {
		if ok, err := path.Match(pat, relPath); err == nil && ok {
			continue
		}
		out = append(out, pat)
	}
	return out
}

// CoherenceGroupFor returns the group containing relPath, if any.
func CoherenceGroupFor(relPath string) (CoherenceGroup, bool) {
	for _, g := range coherenceGroups {
		if g.Matches(relPath) {
			return g, true
		}
	}
	return CoherenceGroup{}, false
}
