package kclrender

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/forgecompat"
	"github.com/reliant-labs/forge/internal/kclvendor"
)

// The stale-vendor warning used to say "Run `forge generate`" in every case,
// including the one where generate refuses this binary: a forge newer than
// the project's pin. Following the advice led straight into that refusal.
func TestStaleVendorAdvice(t *testing.T) {
	const pinned = "v0.1.18-0.20260927050154-0aeff5d58f5c"
	const running = "v0.1.18-0.20260927210725-93ff5b3b19e6"

	t.Run("compatible binary advises generate", func(t *testing.T) {
		got := staleVendorAdvice(pinned, running, forgecompat.Assessment{Verdict: forgecompat.OK}, true, nil, false)
		if !strings.Contains(got, "Run `forge generate` to refresh") {
			t.Errorf("a compatible binary should be sent to generate:\n%s", got)
		}
	})

	t.Run("stale pin never advises generate with this binary", func(t *testing.T) {
		a := forgecompat.Assessment{Verdict: forgecompat.StalePin, ProjectVersion: pinned, BinaryVersion: running}
		for _, pinTask := range []bool{true, false} {
			got := staleVendorAdvice(pinned, running, a, true, nil, pinTask)
			if strings.Contains(got, "Run `forge generate` to refresh") {
				t.Errorf("generate would refuse; advising it is wrong:\n%s", got)
			}
			for _, want := range []string{"Do NOT run `forge generate` with this binary", "go install github.com/reliant-labs/forge/cmd/forge@" + pinned} {
				if !strings.Contains(got, want) {
					t.Errorf("advice must contain %q:\n%s", want, got)
				}
			}
			wantBump := "go get github.com/reliant-labs/forge@"
			if pinTask {
				wantBump = "task pin:forge"
			}
			if !strings.Contains(got, wantBump) {
				t.Errorf("pinTask=%v: advice must name %q:\n%s", pinTask, wantBump, got)
			}
		}
	})

	t.Run("unreleasable build against a published pin", func(t *testing.T) {
		a := forgecompat.Assessment{Verdict: forgecompat.UnreleasableNoBridge, ProjectVersion: pinned}
		got := staleVendorAdvice(pinned, running+"+dirty", a, true, nil, true)
		if strings.Contains(got, "Run `forge generate` to refresh") || !strings.Contains(got, "unreleased build") {
			t.Errorf("unexpected advice:\n%s", got)
		}
	})

	t.Run("newer vendored copy names the forge that wrote it", func(t *testing.T) {
		refusal := &kclvendor.DowngradeError{Stamped: running, Running: pinned}
		got := staleVendorAdvice(running, pinned, forgecompat.Assessment{}, false, refusal, false)
		if strings.Contains(got, "Run `forge generate` to refresh") || !strings.Contains(got, "cmd/forge@"+running) {
			t.Errorf("downgrade case must name the vendoring forge, not generate:\n%s", got)
		}
	})
}

func TestProjectHasTask(t *testing.T) {
	dir := t.TempDir()
	if projectHasTask(dir, "pin:forge") {
		t.Fatal("no Taskfile: no task")
	}
	taskfile := "version: '3'\ntasks:\n  pin:forge:\n    cmds:\n      - ./scripts/pin-sibling.sh forge\n"
	if err := os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(taskfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if !projectHasTask(dir, "pin:forge") {
		t.Error("declared task not found")
	}
	if projectHasTask(dir, "pin:reliant") {
		t.Error("undeclared task reported")
	}
}
