package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/pkg/release"
)

// runFloorReasonCmd executes cmd with args and returns its exitCodeError.
func runFloorReasonCmd(t *testing.T, cmd *cobra.Command, args ...string) exitCodeError {
	t.Helper()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	err := cmd.Execute()
	var exitErr exitCodeError
	if !errors.As(err, &exitErr) {
		t.Fatalf("%v: err = %v; want an exitCodeError", args, err)
	}
	return exitErr
}

// A reason over the ledger's cap is refused locally, naming the limit and the
// reason's length, by every verb that records one. The refusal comes before
// the ledger is chosen: these commands run with no env, project or network a
// ledger could be read from, and any other error would mean they went on.
func TestFloorVerbsRefuseAnOverlongReasonBeforeTheLedger(t *testing.T) {
	long := strings.Repeat("a", release.MaxFloorReasonBytes+19)
	want := fmt.Sprintf("is %d bytes; the floor ledger accepts at most %d (release.MaxFloorReasonBytes)", len(long), release.MaxFloorReasonBytes)
	for name, args := range map[string][]string{
		"set":           {"set", "nowhere", "v1.0.0", "--reason", long},
		"set --current": {"set", "nowhere", "--current", "--reason", long},
		"lower":         {"lower", "nowhere", "v1.0.0", "--reason", long},
		"clear":         {"clear", "nowhere", "--reason", long},
		"lift":          {"lift", "nowhere", "floor-1", "--reason", long},
	} {
		t.Run(name, func(t *testing.T) {
			got := runFloorReasonCmd(t, newEnvFloorCmd(), args...)
			if got.code != exitWrong || !strings.Contains(got.msg, "--reason "+want) {
				t.Fatalf("exit %d %q; want exit %d with %q", got.code, got.msg, exitWrong, "--reason "+want)
			}
		})
	}
	t.Run("deploy --floor", func(t *testing.T) {
		got := runFloorReasonCmd(t, newDeployCmd(), "nowhere", "v1.0.0", "--floor", long)
		if got.code != exitWrong || !strings.Contains(got.msg, "--floor "+want) {
			t.Fatalf("exit %d %q; want exit %d with %q", got.code, got.msg, exitWrong, "--floor "+want)
		}
	})
}

func TestFloorReasonBounds(t *testing.T) {
	reason := func(r string) error { return (&floorFlags{reason: r}).requireReason("set") }
	atLimit := strings.Repeat("a", release.MaxFloorReasonBytes)
	if err := reason(atLimit); err != nil {
		t.Fatalf("a reason of exactly %d bytes: %v; want it admitted", release.MaxFloorReasonBytes, err)
	}
	if err := reason(atLimit + "a"); err == nil {
		t.Fatalf("a reason of %d bytes was admitted", release.MaxFloorReasonBytes+1)
	}
	if err := reason("bad \xff byte"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8: %v; want a refusal naming UTF-8", err)
	}
}
