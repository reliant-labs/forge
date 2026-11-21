// Package testlocal reads the local test record, .forge/tests/runs/, for
// `forge test report`, `coverage`, `runs` and `ingest`.
//
// It turns run directories into the inputs of pkg/testreport's documents:
// the lanes' parsed results, their coverage merged across lanes (refused
// across trees, builds or source versions), each covered file's repo path and
// git blob id, and the diff's changed lines. The numbers are pkg/testreport's
// (the same locally and hosted); this package only finds and reads what they
// are computed from.
//
// Git is reached only through the [Git] seam, so everything here is testable
// on run directories written into a temp dir with a fake git.
package testlocal
