// Tests for telling a rollback artifact from a pre-existing error in the
// failed-generate report (generate_rollback_attribution.go).
//
// The reported case: `forge generate --scope internal/assigner` with an
// error planted in internal/assigner AND an unrelated, pre-existing one in
// pkg/rangerclient/zz_broken.go. The report listed the rangerclient file
// under "ARTIFACTS OF THE ROLLBACK, not your bug: … unchanged on disk and
// correct". It was broken before the run.
package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// attributedAs attributes every error line in output to origin.
func attributedAs(origin errorOrigin, output string) *postRollbackAttribution {
	a := &postRollbackAttribution{lines: map[string]lineOrigin{}}
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); errorLineFile(line) != "" {
			a.lines[line] = lineOrigin{origin: origin}
		}
	}
	return a
}

const preExistingLine = "pkg/rangerclient/zz_broken.go:3:13: undefined: nope"

// sectionAfter returns out from marker on, up to the next section marker.
func sectionAfter(out, marker string) string {
	i := strings.Index(out, marker)
	if i == -1 {
		return ""
	}
	rest := out[i+len(marker):]
	for _, next := range []string{rollbackArtifactMarker, rollbackPreExistingMarker, "❔ Errors forge could not attribute"} {
		if j := strings.Index(rest, next); j != -1 {
			rest = rest[:j]
		}
	}
	return rest
}

func TestRollbackReport_PreExistingErrorIsNotCalledAnArtifact(t *testing.T) {
	output := postRollbackBuildOutput + preExistingLine + "\n"
	attr := attributedAs(originRollback, postRollbackBuildOutput)
	attr.lines[preExistingLine] = lineOrigin{origin: originPreExisting}

	var sb strings.Builder
	writeRollbackReport(&sb, rollbackReport{
		Restored:    []string{"internal/blobstore/middleware_gen.go"},
		StepErr:     rootCauseErr(),
		Consistency: rollbackConsistency{Checked: true, Output: output},
		Attribution: attr,
		Residue:     rollbackResidue{Checked: true},
	})
	out := sb.String()

	artifacts := sectionAfter(out, rollbackArtifactMarker)
	if !strings.Contains(artifacts, "scoping.go") {
		t.Errorf("scoping.go vanishes with the run's output: it is an artifact:\n%s", out)
	}
	if strings.Contains(artifacts, "zz_broken.go") {
		t.Errorf("a pre-existing error must not be listed as a rollback artifact (\"correct\" on disk):\n%s", out)
	}
	pre := sectionAfter(out, rollbackPreExistingMarker)
	if !strings.Contains(pre, "pkg/rangerclient/zz_broken.go") || !strings.Contains(pre, "undefined: nope") {
		t.Errorf("the pre-existing error must be listed under %q:\n%s", rollbackPreExistingMarker, out)
	}
	if strings.Contains(out, "the tree builds") {
		t.Errorf("with a pre-existing error left, re-running generate does not make the tree build:\n%s", out)
	}
	if !strings.Contains(pre, "Fix them separately") {
		t.Errorf("the report must say fixing the root cause will not clear a pre-existing error:\n%s", out)
	}
}

// With no attribution (the check could not run) an untouched file's error
// gets neither claim: not "correct", not "pre-existing".
func TestRollbackReport_UnattributedErrorGetsNeitherLabel(t *testing.T) {
	var sb strings.Builder
	writeRollbackReport(&sb, rollbackReport{
		Restored:    []string{"internal/blobstore/middleware_gen.go"},
		StepErr:     rootCauseErr(),
		Consistency: rollbackConsistency{Checked: true, Output: preExistingLine + "\n"},
		Attribution: &postRollbackAttribution{reason: "the check could not run: go list: exit status 1"},
		Residue:     rollbackResidue{Checked: true},
	})
	out := sb.String()
	for _, marker := range []string{rollbackArtifactMarker, rollbackPreExistingMarker} {
		if strings.Contains(out, marker) {
			t.Errorf("an unattributed error must not carry %q:\n%s", marker, out)
		}
	}
	if !strings.Contains(out, "could not attribute") || !strings.Contains(out, "the check could not run") ||
		!strings.Contains(out, "zz_broken.go") {
		t.Errorf("an unattributed error must be shown, with why it is unattributed:\n%s", out)
	}
}

func TestOriginUnder(t *testing.T) {
	const line = "internal/x/x.go:3:2: undefined: y"
	compiled := func(errText string) *postRunPackage {
		p := &postRunPackage{listedPackage: listedPackage{ImportPath: "ex.com/m/internal/x"}, Export: "compiled.a"}
		if errText != "" {
			p.Error = &listedPackageError{Err: "# ex.com/m/internal/x\n" + errText}
		}
		return p
	}
	for name, tc := range map[string]struct {
		pkg    *postRunPackage
		origin errorOrigin
		why    string
	}{
		"compiles with the run's output": {pkg: compiled(""), origin: originRollback},
		"same error with the run's output": {
			pkg: compiled(line + "\n"), origin: originPreExisting,
		},
		"a different error with the run's output": {
			pkg: compiled("internal/x/x.go:4:2: brand new\n"), why: "may hide other errors",
		},
		"a dependency fails": {
			pkg: &postRunPackage{listedPackage: listedPackage{DepsErrors: []*listedPackageError{{Err: "boom"}}}}, why: "imports does not compile",
		},
		"no compile evidence": {pkg: &postRunPackage{}, why: "did not produce a compiled package"},
		"load error is not a compiler diagnostic": {
			pkg: &postRunPackage{listedPackage: listedPackage{Error: &listedPackageError{Err: line}}}, why: "did not reach",
		},
		"not listed": {why: "not there"},
	} {
		t.Run(name, func(t *testing.T) {
			got := originUnder(tc.pkg, line)
			if got.origin != tc.origin || !strings.Contains(got.why, tc.why) {
				t.Errorf("originUnder = %+v, want origin %d with why containing %q", got, tc.origin, tc.why)
			}
		})
	}
}

// A changed dependency can fix an unchanged consumer or prevent its compiler
// from running at all. Only the first case is a proven rollback artifact.
func TestRollbackAttribution_CrossPackage(t *testing.T) {
	testkit.Slow(t, "compiles a cross-package overlay with go")
	for _, brokenDependency := range []bool{false, true} {
		t.Run(fmt.Sprint(brokenDependency), func(t *testing.T) {
			root := t.TempDir()
			writeRollbackFile(t, root, "go.mod", "module example.com/app\n\ngo 1.22\n")
			writeRollbackFile(t, root, "dep/output.go", "package dep\nconst Value = 1\n")
			writeRollbackFile(t, root, "consumer/use.go", "package consumer\nimport \"example.com/app/dep\"\nvar _ string = dep.Value\n")
			body := "package dep\nconst Value = \"ok\"\n"
			if brokenDependency {
				body = "package dep\nconst Value = missing\n"
			}
			const line = "consumer/use.go:3:16: cannot use dep.Value (untyped int constant 1) as string value in variable declaration"
			inputs := postRunGoInputs{files: map[string]postRunFile{"dep/output.go": {exists: true, content: []byte(body)}}}
			attr := attributePostRollbackErrors(root, inputs, []string{"dep/output.go"}, line)
			want := originRollback
			if brokenDependency {
				want = originUnknown
			}
			if got := attr.of(line); got.origin != want {
				t.Fatalf("attribution = %+v, want %v", got, want)
			}
			// The replay includes the full restored-tree scope, even though
			// only the consumer's directory appeared in the diagnostic.
			writeRollbackFile(t, root, "unrelated/other.go", "package unrelated\nconst X = 1\n")
			pkgs, err := compileWithPostRunOverlay(root, inputs.files)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range pkgs {
				if p.ImportPath == "example.com/app/unrelated" && p.Export != "" {
					return
				}
			}
			t.Fatal("replay omitted a package in ./...")
		})
	}
}

func TestRollbackGeneratedTree_PreExistingErrorIsNotAnArtifact(t *testing.T) {
	testkit.Slow(t, "execs git and go")
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		writeRollbackFile(t, root, rel, body)
	}
	write("go.mod", "module example.com/app\n\ngo 1.22\n")
	write("internal/handlers/jobs/rpc.go", "package jobs\n\ntype Service struct{}\n\nfunc (s *Service) Get() string { return s.op() }\n")
	write("internal/handlers/jobs/handlers_ops_gen.go", "package jobs\n")
	write("internal/handlers/jobs/noop_gen.go", "package jobs\n")
	write("pkg/rangerclient/client.go", "package rangerclient\n")
	write("pkg/rangerclient/zz_broken.go", "package rangerclient\n\nvar _ = nope\n")
	write("internal/assigner/assigner.go", "package assigner\n")

	checksums.ResetSkipWrite()
	checksums.ResetPerRunState()
	t.Cleanup(checksums.ResetPerRunState)
	checksums.BeginRollbackJournal(root)
	t.Cleanup(checksums.CommitRollback)
	preRun := snapshotWorkTree(root)

	for rel, body := range map[string]string{
		"internal/handlers/jobs/handlers_ops_gen.go": "package jobs\n\nfunc (s *Service) op() string { return \"ok\" }\n",
		"internal/assigner/mock_gen.go":              "package assigner\n\nvar _ = undefinedInMock\n",
	} {
		if _, err := checksums.WriteGeneratedFile(root, filepath.FromSlash(rel), []byte(body), &checksums.FileChecksums{}, false); err != nil {
			t.Fatal(err)
		}
	}
	// Restore lists identical rewrites too; they need no overlay.
	journaledWrite(t, root, "internal/handlers/jobs/noop_gen.go", "package jobs\n")
	stepErr := &validateBuildError{
		Output: "internal/assigner/mock_gen.go:3:9: undefined: undefinedInMock\n",
		err:    errors.New("forge generate (validate generated code): generated code does not compile"),
	}

	stderr, restore := captureStderr(t)
	rollbackGeneratedTreeVerified(root, stepErr, false, preRun)
	restore()
	out := stderr.String()

	artifacts := sectionAfter(out, rollbackArtifactMarker)
	if !strings.Contains(artifacts, "internal/handlers/jobs/rpc.go") {
		t.Errorf("rpc.go fails only against the reverted handlers_ops_gen.go: it must be an artifact:\n%s", out)
	}
	if strings.Contains(artifacts, "zz_broken.go") {
		t.Errorf("zz_broken.go was broken before the run; it must not be called a rollback artifact:\n%s", out)
	}
	pre := sectionAfter(out, rollbackPreExistingMarker)
	if !strings.Contains(pre, "pkg/rangerclient/zz_broken.go") {
		t.Errorf("zz_broken.go must be labelled pre-existing (%q):\n%s", rollbackPreExistingMarker, out)
	}
	if strings.Contains(pre, "rpc.go") {
		t.Errorf("rpc.go is an artifact, not pre-existing:\n%s", out)
	}
	// The overlay never touches the tree: the revert's bytes stay.
	if got, _ := os.ReadFile(filepath.Join(root, "internal/handlers/jobs/handlers_ops_gen.go")); string(got) != "package jobs\n" {
		t.Errorf("handlers_ops_gen.go must be back to its pre-run bytes, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "internal/assigner/mock_gen.go")); !os.IsNotExist(err) {
		t.Errorf("mock_gen.go was created this run and must be gone after the revert: %v", err)
	}
}
