package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// TestReportForkedSkips_OneLoudLinePerFile pins the item-2 output
// contract: every forked-skipped path gets its own clearly-formatted
// line naming the side-render location and the reconcile command — on
// every run, with no flag gating.
func TestReportForkedSkips_OneLoudLinePerFile(t *testing.T) {
	checksums.ResetPerRunState()
	defer checksums.ResetPerRunState()

	root := t.TempDir()
	cs := &checksums.FileChecksums{Files: make(map[string]checksums.FileChecksumEntry)}
	for _, p := range []string{"pkg/app/wire_gen.go", "pkg/app/bootstrap.go"} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package app // fork\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cs.RecordFile(p, []byte("package app // fork\n"))
		entry := cs.Files[p]
		entry.Tier = 1
		entry.Forked = true
		cs.Files[p] = entry
		// Trigger the skip the way the pipeline does.
		if wrote, err := checksums.WriteGeneratedFile(root, p, []byte("package app // fresh\n"), cs, false); err != nil || wrote {
			t.Fatalf("expected forked skip for %s (wrote=%v err=%v)", p, wrote, err)
		}
	}

	var b strings.Builder
	reportForkedSkips(&b)
	out := b.String()

	for _, want := range []string{
		"⚠ forked (not regenerated): pkg/app/wire_gen.go",
		"fresh render at .forge/render/pkg/app/wire_gen.go",
		"forge unfork --merge pkg/app/wire_gen.go",
		"⚠ forked (not regenerated): pkg/app/bootstrap.go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q; got:\n%s", want, out)
		}
	}
}

// TestReportForkedSkips_SilentWhenClean: no forked skips → no output at
// all (the common case must not add noise).
func TestReportForkedSkips_SilentWhenClean(t *testing.T) {
	checksums.ResetPerRunState()
	var b strings.Builder
	reportForkedSkips(&b)
	if b.Len() != 0 {
		t.Errorf("expected no output, got %q", b.String())
	}
}
