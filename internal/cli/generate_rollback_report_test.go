// Tests for root-cause attribution in the failed-generate report.
//
// The observed failure (third dogfood run): `forge generate` aborted on a
// typecheck error in a GENERATED test helper (`invalid composite literal
// type tagging.Role`). The rollback then rewound 71 files, which restored an
// OLDER handlers_crud_ops_gen.go. Validation subsequently reported:
//
//	internal/handlers/documents/scoping.go:66:10: s.crudGetDocumentOp undefined
//
// four times — against a hand-written file that demonstrably compiles
// (`go build ./internal/handlers/documents/` exits 0). The user spent four
// turns debugging a correct file while the real fault was two files away.
//
// Separately, the restored tree did not build at all: compose.go was kept
// from an earlier run while internal/blobstore/middleware_gen.go was
// generated this run and then reverted, leaving two halves of one feature
// on opposite sides of the rollback boundary. forge nonetheless claimed the
// tree was "back to its pre-run state".
//
// Pinned contract:
//
//   - The report ALWAYS names the root cause — the error that aborted the
//     run — under an unmistakable marker, so the user knows which error to
//     act on.
//   - forge VERIFIES the tree it restored compiles. When it does not, the
//     "back to its pre-run state" claim is WITHHELD (it was false), the
//     reverted files are named, and the post-rollback errors are labelled
//     as artifacts rather than presented as the user's bug.
//   - A post-rollback error citing a file forge never wrote this run is
//     called out explicitly as correct-on-disk. That is the scoping.go
//     case, and it is the one that must never send a user debugging.
package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// rootCauseErr is the real abort from the dogfood run: a typecheck failure
// in a generated test helper, carrying its own output.
func rootCauseErr() error {
	return &validateBuildError{
		Output: "internal/tagging/helpers_gen_test.go:41:22: invalid composite literal type tagging.Role\n",
		err:    errors.New("forge generate (validate generated code): generated test files do not compile"),
	}
}

// postRollbackBuildOutput is what `go build ./...` printed against the
// tree the rollback restored. EVERY line here is an artifact: scoping.go
// is hand-written and forge never touched it, and compose.go was kept from
// an earlier run while its middleware_gen.go counterpart was reverted.
const postRollbackBuildOutput = "internal/handlers/documents/scoping.go:66:10: s.crudGetDocumentOp undefined (type *Service has no field or method crudGetDocumentOp)\n" +
	"internal/app/compose.go:98:26: undefined: blobstore.NewServiceWithForgeMiddleware\n"

func TestRollbackReport_NamesRootCauseWhenRestoredTreeBuilds(t *testing.T) {
	var sb strings.Builder
	writeRollbackReport(&sb, rollbackReport{
		Restored:    []string{"internal/tagging/helpers_gen_test.go"},
		StepErr:     rootCauseErr(),
		Consistency: rollbackConsistency{Checked: true, Builds: true},
	})
	out := sb.String()

	if !strings.Contains(out, rollbackRootCauseMarker) {
		t.Errorf("report must mark the root cause with %q:\n%s", rollbackRootCauseMarker, out)
	}
	if !strings.Contains(out, "invalid composite literal type tagging.Role") {
		t.Errorf("root cause output must be shown:\n%s", out)
	}
	// The restored tree compiles, so the reassurance is TRUE and should stand.
	if !strings.Contains(out, "pre-run state") {
		t.Errorf("a verified-clean rollback should still reassure the user:\n%s", out)
	}
}

func TestRollbackReport_NonCompilingTreeWithholdsPreRunClaim(t *testing.T) {
	var sb strings.Builder
	writeRollbackReport(&sb, rollbackReport{
		Restored: []string{"internal/blobstore/middleware_gen.go", "internal/tagging/helpers_gen_test.go"},
		StepErr:  rootCauseErr(),
		Consistency: rollbackConsistency{
			Checked: true,
			Builds:  false,
			Output:  postRollbackBuildOutput,
		},
	})
	out := sb.String()

	// THE false claim. It must not appear when the tree does not build.
	if strings.Contains(out, "back to its pre-run state") {
		t.Errorf("must NOT claim a clean pre-run tree when the restored tree does not compile:\n%s", out)
	}
	if !strings.Contains(out, "does NOT compile") {
		t.Errorf("a non-compiling restored tree must be stated plainly:\n%s", out)
	}
	// The root cause still has to be identifiable among the noise.
	if !strings.Contains(out, rollbackRootCauseMarker) ||
		!strings.Contains(out, "invalid composite literal type tagging.Role") {
		t.Errorf("root cause must still be named:\n%s", out)
	}
	// And the reverted files must be named, since their counterparts are
	// what the post-rollback errors are really about.
	if !strings.Contains(out, "internal/blobstore/middleware_gen.go") {
		t.Errorf("reverted files must be named:\n%s", out)
	}
}

// The core anti-regression: an error citing a file forge NEVER wrote must
// be labelled as an artifact, never presented as the user's bug.
func TestRollbackReport_FlagsErrorsCitingUntouchedFiles(t *testing.T) {
	var sb strings.Builder
	writeRollbackReport(&sb, rollbackReport{
		Restored: []string{"internal/blobstore/middleware_gen.go"},
		StepErr:  rootCauseErr(),
		Consistency: rollbackConsistency{
			Checked: true,
			Builds:  false,
			Output:  postRollbackBuildOutput,
		},
	})
	out := sb.String()

	if !strings.Contains(out, rollbackArtifactMarker) {
		t.Errorf("post-rollback errors must carry the artifact marker %q:\n%s", rollbackArtifactMarker, out)
	}
	// scoping.go is hand-written, was never written this run, and compiles.
	// The report must say so rather than leaving the user to discover it.
	if !strings.Contains(out, "scoping.go") {
		t.Errorf("the artifact section must name the wrongly-cited file:\n%s", out)
	}
	if !strings.Contains(out, "forge did not write") {
		t.Errorf("report must state these files were not written by this run (so they are correct on disk):\n%s", out)
	}
	// It must actively steer the user AWAY from debugging them.
	if !strings.Contains(out, "Do not debug") {
		t.Errorf("report must tell the user not to debug the cited files:\n%s", out)
	}
}

func TestClassifyPostRollbackErrors_SplitsTouchedFromUntouched(t *testing.T) {
	touched := map[string]bool{"internal/blobstore/middleware_gen.go": true}
	output := "internal/handlers/documents/scoping.go:66:10: s.crudGetDocumentOp undefined\n" +
		"internal/blobstore/middleware_gen.go:12:1: syntax error\n" +
		"# some/package\n"

	untouched, other := classifyPostRollbackErrors(output, touched)

	if len(untouched) != 1 || !strings.Contains(untouched[0], "scoping.go") {
		t.Errorf("scoping.go was never written this run — it must classify as untouched, got %v", untouched)
	}
	for _, line := range other {
		if strings.Contains(line, "scoping.go") {
			t.Errorf("scoping.go must not appear in the non-artifact bucket: %v", other)
		}
	}
	if len(other) == 0 {
		t.Errorf("an error citing a file forge DID write belongs in the other bucket, got none")
	}
}

// TestRollbackGeneratedTree_DogfoodScenarioEndToEnd reconstructs the real
// dogfood failure on a REAL tree, through the REAL rollback path, with a
// REAL `go build` verdict — no hand-fed consistency struct.
//
// The tree is the one that burned four turns:
//
//   - scoping.go       hand-written, calls crudGetDocumentOp, forge never
//     writes it, compiles against this run's output.
//   - ..._crud_ops_gen.go  generated; the PRE-RUN copy lacks the method, so
//     reverting to it is what breaks scoping.go.
//   - compose.go       kept from an EARLIER run; needs the blobstore ctor.
//   - middleware_gen.go generated THIS run (defines that ctor), reverted.
//
// Post-rollback the tree cannot build and every error cites a file that is
// either untouched or from an earlier run. Not one of them is the fault.
func TestRollbackGeneratedTree_DogfoodScenarioEndToEnd(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("go.mod", "module example.com/app\n\ngo 1.22\n")
	write("internal/handlers/documents/scoping.go", "package documents\n\ntype Service struct{}\n\nfunc (s *Service) Scope() string { return s.crudGetDocumentOp() }\n")
	write("internal/handlers/documents/handlers_crud_ops_gen.go", "package documents\n")
	write("internal/app/compose.go", "package app\n\nimport \"example.com/app/internal/blobstore\"\n\nfunc Compose() any { return blobstore.NewServiceWithForgeMiddleware() }\n")
	write("internal/blobstore/blobstore.go", "package blobstore\n")

	checksums.ResetSkipWrite()
	checksums.ResetPerRunState()
	t.Cleanup(checksums.ResetPerRunState)
	checksums.BeginRollbackJournal(root)
	t.Cleanup(checksums.CommitRollback)

	if _, err := checksums.WriteGeneratedFile(root,
		filepath.Join("internal", "handlers", "documents", "handlers_crud_ops_gen.go"),
		[]byte("package documents\n\nfunc (s *Service) crudGetDocumentOp() string { return \"ok\" }\n"),
		&checksums.FileChecksums{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := checksums.WriteGeneratedFile(root,
		filepath.Join("internal", "blobstore", "middleware_gen.go"),
		[]byte("package blobstore\n\nfunc NewServiceWithForgeMiddleware() any { return nil }\n"),
		&checksums.FileChecksums{}, false); err != nil {
		t.Fatal(err)
	}

	stderr, restore := captureStderr(t)
	rollbackGeneratedTree(root, rootCauseErr())
	restore()
	out := stderr.String()

	// (a) The root cause is named, and it is the typecheck error.
	if !strings.Contains(out, rollbackRootCauseMarker) ||
		!strings.Contains(out, "invalid composite literal type tagging.Role") {
		t.Errorf("root cause must be named:\n%s", out)
	}

	// (b) The false claim is gone — this tree genuinely does not compile.
	if strings.Contains(out, "back to its pre-run state") {
		t.Errorf("tree does not compile; the pre-run-state claim must be withheld:\n%s", out)
	}
	if !strings.Contains(out, "does NOT compile") {
		t.Errorf("the non-compiling restored tree must be stated:\n%s", out)
	}

	// (c) THE regression: scoping.go is correct on disk and must be
	//     labelled an artifact, never presented as the user's bug.
	if !strings.Contains(out, rollbackArtifactMarker) {
		t.Errorf("post-rollback errors must be marked as artifacts:\n%s", out)
	}
	artifactIdx := strings.Index(out, rollbackArtifactMarker)
	scopingIdx := strings.Index(out, "scoping.go")
	if scopingIdx == -1 || scopingIdx < artifactIdx {
		t.Errorf("scoping.go must appear only inside the artifact section:\n%s", out)
	}
	if !strings.Contains(out, "Do not debug") {
		t.Errorf("user must be told not to debug the cited files:\n%s", out)
	}

	// (d) The root cause must be the LAST thing on screen (tail-visibility),
	//     and the artifacts must never be what gets repeated there.
	tail := out[strings.LastIndex(out, "Compiler output"):]
	if !strings.Contains(tail, "tagging.Role") {
		t.Errorf("the repeated tail block must carry the ROOT CAUSE:\n%s", tail)
	}
	if strings.Contains(tail, "crudGetDocumentOp") {
		t.Errorf("rollback artifacts must NOT be repeated as the final error:\n%s", tail)
	}
}

// A non-Go project (or one with no module) must not have a spurious build
// failure invented for it: the check reports "not checked", and the report
// falls back to the plain reverted message without either claim.
func TestRollbackBuildCheck_SkipsWhenNoGoModule(t *testing.T) {
	cons := goBuildRestoredTree(t.TempDir())
	if cons.Checked {
		t.Errorf("no go.mod means no build verdict, got %+v", cons)
	}

	var sb strings.Builder
	writeRollbackReport(&sb, rollbackReport{
		Restored:    []string{"deploy/kcl/dev/main.k"},
		StepErr:     errors.New("some non-build step failed"),
		Consistency: cons,
	})
	out := sb.String()
	if strings.Contains(out, "does NOT compile") {
		t.Errorf("unchecked tree must not be reported as non-compiling:\n%s", out)
	}
}
