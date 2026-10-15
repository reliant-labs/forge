// Package affected decides which Go packages' tests a change can reach.
//
// Affected-package mode: on a branch, the gate's unit lane tests only the Go
// packages a change can reach. A whole-module `-race -count=1` run was measured
// at ~14 min on a shared machine, and a typical branch touches a handful of
// packages; testing the rest re-proves what the default branch already
// proved.
//
// A package P is tested when its test binary — P, its in-package and external
// tests, and everything they import, transitively — contains a changed
// package. "Changed" is decided from the files that differ between the work
// tree and `git merge-base HEAD origin/<default>`:
//
//   - a .go file maps to the package in its directory;
//   - a file a package embeds (go:embed, including test embeds) maps to that
//     package, so a template or fixture compiled into it counts;
//   - a file under a package's own testdata/ maps to that package.
//
// Two kinds of file reach only their own package's tests, never its importers:
// a package's testdata/ and a file only its tests embed. They are compiled or
// read by that package's test binary and by nothing that merely links it.
//
// ANY other changed file falls back to the full suite, because nothing proves
// which tests read it — unless ci.gate.affected_ignore names it (no test reads
// it) or a ci.gate.affected_rules entry claims it and says which packages it
// selects (affected_rules.go). A rule can also declare a full suite (a pin, a
// workflow). So does the default branch itself, a branch with no changes, a
// missing origin/<default>, and an explicit request (CI_FULL=1). The mode and
// its reason are always printed, and so is why each package runs.
package affected
