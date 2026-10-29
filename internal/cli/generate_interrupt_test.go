//go:build unix

package cli

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/checksums"
)

// An interrupted `forge generate` is a failed one: it must leave the tree as
// it found it. It used to die where it stood — Ctrl-C, or a harness's SIGTERM
// at its timeout — and the tree kept whatever the run had written so far:
// generated files in their pre-goimports form, an ORM file beside hand code
// that did not match it.
func TestGenerate_InterruptRollsTheTreeBack(t *testing.T) {
	dir := t.TempDir()
	mockRel := "internal/store/mock_gen.go"
	stamped, _ := checksums.Stamp(mockRel, []byte("package store\n\n// the committed render\n"))
	writeTreeFileT(t, filepath.Join(dir, filepath.FromSlash(mockRel)), string(stamped))
	before := snapshotTree(t, dir)

	// The test's own registration makes the self-signal observable (and
	// keeps it from killing the test binary whatever the pipeline does).
	seen := make(chan os.Signal, 1)
	signal.Notify(seen, os.Interrupt)
	t.Cleanup(func() { signal.Stop(seen) })

	writer := GenStep{
		Name: "internal package contracts", Gate: always, Tag: "codegen",
		Run: func(ctx *pipelineContext) error {
			_, err := checksums.WriteDerivedFileOutcome(ctx.AbsPath, mockRel, []byte("package store\n\n// this run's render\n"), &checksums.FileChecksums{}, true)
			return err
		},
	}
	interrupt := GenStep{
		Name: "a step during which the user presses Ctrl-C", Gate: always, Tag: "tools",
		Run: func(*pipelineContext) error {
			if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
				return err
			}
			select {
			case <-seen:
			case <-time.After(5 * time.Second):
				t.Error("SIGINT was never delivered")
			}
			return nil
		},
	}
	ranAfter := false
	after := GenStep{
		Name: "a step that must not run", Gate: always, Tag: "codegen",
		Run: func(*pipelineContext) error { ranAfter = true; return nil },
	}
	withPipelineSteps(t, []GenStep{writer, interrupt, after})
	out, runErr := runPipelineCapturingOutput(t, dir)

	var ie *generateInterruptedError
	if !errors.As(runErr, &ie) {
		t.Fatalf("an interrupted run must fail as interrupted, got %v\n%s", runErr, out)
	}
	if ranAfter {
		t.Error("the run must stop at the step it was interrupted in")
	}
	if diffs := diffTrees(before, snapshotTree(t, dir)); len(diffs) > 0 {
		t.Errorf("an interrupted generate must leave the tree byte-identical, changed:\n  %s\n%s", strings.Join(diffs, "\n  "), out)
	}
	if !strings.Contains(out, "your tree is back to its pre-run state") {
		t.Errorf("the rollback must report the restore:\n%s", out)
	}
}
