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

// TestWarnForkCoherenceOnAccept: forking a coherence-group member names
// the still-regenerating siblings; ungrouped paths stay silent.
func TestWarnForkCoherenceOnAccept(t *testing.T) {
	tests := []struct {
		name     string
		accepted []string
		wantSubs []string
		wantNone bool
	}{
		{
			name:     "group member warns and names siblings",
			accepted: []string{"pkg/app/bootstrap.go"},
			wantSubs: []string{
				`fork-coherence group "app-wiring"`,
				"pkg/app/app_gen.go",
				"pkg/app/wire_gen.go",
				"pkg/app/testing.go",
				"break the build",
			},
		},
		{
			name:     "ungrouped path is silent",
			accepted: []string{"pkg/config/config.go"},
			wantNone: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			warnForkCoherenceOnAccept(&b, tt.accepted)
			out := b.String()
			if tt.wantNone {
				if out != "" {
					t.Errorf("expected silence, got:\n%s", out)
				}
				return
			}
			for _, want := range tt.wantSubs {
				if !strings.Contains(out, want) {
					t.Errorf("warning missing %q; got:\n%s", want, out)
				}
			}
		})
	}
}

// TestWarnIncoherentForkGroups pins the generate-time pairing: warn only
// when a group has BOTH a forked member and a non-forked sibling whose
// fresh render changed this run.
func TestWarnIncoherentForkGroups(t *testing.T) {
	mkCS := func(forkedPaths ...string) *checksums.FileChecksums {
		cs := &checksums.FileChecksums{Files: make(map[string]checksums.FileChecksumEntry)}
		for _, p := range forkedPaths {
			cs.Files[p] = checksums.FileChecksumEntry{Hash: "x", Tier: 1, Forked: true}
		}
		return cs
	}

	t.Run("forked member + changed sibling warns", func(t *testing.T) {
		checksums.ResetPerRunState()
		defer checksums.ResetPerRunState()

		root := t.TempDir()
		cs := mkCS("pkg/app/bootstrap.go", "pkg/app/wire_gen.go")
		// app_gen.go renders with NEW content this run (not forked).
		if _, err := checksums.WriteGeneratedFile(root, "pkg/app/app_gen.go", []byte("package app // new symbols\n"), cs, false); err != nil {
			t.Fatal(err)
		}

		var b strings.Builder
		warnIncoherentForkGroups(&b, cs)
		out := b.String()
		for _, want := range []string{
			`fork-coherence group "app-wiring"`,
			"changed this run: pkg/app/app_gen.go",
			"pkg/app/bootstrap.go, pkg/app/wire_gen.go",
			"build-break risk",
			"forge unfork --merge",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("warning missing %q; got:\n%s", want, out)
			}
		}
	})

	t.Run("no changed sibling — silent", func(t *testing.T) {
		checksums.ResetPerRunState()
		defer checksums.ResetPerRunState()

		cs := mkCS("pkg/app/bootstrap.go")
		var b strings.Builder
		warnIncoherentForkGroups(&b, cs)
		if b.Len() != 0 {
			t.Errorf("expected silence, got:\n%s", b.String())
		}
	})

	t.Run("changed sibling but nothing forked — silent", func(t *testing.T) {
		checksums.ResetPerRunState()
		defer checksums.ResetPerRunState()

		root := t.TempDir()
		cs := &checksums.FileChecksums{Files: make(map[string]checksums.FileChecksumEntry)}
		if _, err := checksums.WriteGeneratedFile(root, "pkg/app/app_gen.go", []byte("package app\n"), cs, false); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		warnIncoherentForkGroups(&b, cs)
		if b.Len() != 0 {
			t.Errorf("expected silence, got:\n%s", b.String())
		}
	})
}
