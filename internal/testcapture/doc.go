// Package testcapture records a project's own test lanes: `forge test begin`
// allocates a run directory and `forge test capture` runs one lane's command
// unchanged, storing what it produced beside it. The layout and the two
// manifests are pkg/testreport's (manifest.go); this package writes them.
//
// It never decides how tests run, and it never changes a verdict. The command
// is the project's own (its Taskfile's flags), its exit status is capture's,
// and a record that cannot be written is a warning: a lane must never fail, or
// pass, because of the thing recording it.
package testcapture
