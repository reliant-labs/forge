// File: internal/cli/generate_forked_skip_test.go
//
// Tests the end-of-pipeline summary that surfaces silently-skipped
// forked Tier-1 writes. See checksums.SkippedForkedThisRun and
// reportForkedSkips() in generate.go for the why.
package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// mkForkedChecksums builds an in-memory FileChecksums where every path
// is marked Forked: true and (optionally) Accepted: true, so the
// reportForkedSkips tests can exercise the "first run loud / later
// runs silent" contract without going through the full generate
// pipeline.
func mkForkedChecksums(accepted bool, paths ...string) *checksums.FileChecksums {
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{}}
	for _, p := range paths {
		cs.Files[p] = checksums.FileChecksumEntry{
			Hash:     "deadbeef",
			Tier:     1,
			Forked:   true,
			Accepted: accepted,
		}
	}
	return cs
}

func TestReportForkedSkips_PrintsSummary(t *testing.T) {
	defer checksums.ResetSkipWrite()

	// Simulate two forked-skip events recorded by WriteGeneratedFile.
	checksums.SkippedForkedThisRun = []string{
		"pkg/app/wire_gen.go",
		"pkg/app/bootstrap.go",
		// Duplicate — multiple emit steps may target the same path; the
		// reporter is responsible for dedup before printing.
		"pkg/app/wire_gen.go",
	}
	cs := mkForkedChecksums(false, "pkg/app/wire_gen.go", "pkg/app/bootstrap.go")

	buf, restore := captureStderr(t)
	reportForkedSkips(cs)
	restore()

	out := buf.String()
	if !strings.Contains(out, "pkg/app/wire_gen.go") {
		t.Errorf("missing wire_gen.go path: %s", out)
	}
	if !strings.Contains(out, "pkg/app/bootstrap.go") {
		t.Errorf("missing bootstrap.go path: %s", out)
	}
	if !strings.Contains(out, "forge generate unfork") {
		t.Errorf("missing unfork command hint: %s", out)
	}
	// The summary must dedupe wire_gen.go even though it was recorded
	// twice — otherwise the unfork command line ends up with the same
	// path twice and looks broken.
	if c := strings.Count(out, "pkg/app/wire_gen.go"); c != 2 {
		// Count = 2 because the path appears once in the bullet list
		// and once in the unfork command line at the end.
		t.Errorf("wire_gen.go appears %d time(s), want 2 (bullet + command); output:\n%s", c, out)
	}
	// Count line should say "2 forked file(s)" not 3 — duplicates merged.
	if !strings.Contains(out, "2 forked file(s)") {
		t.Errorf("expected `2 forked file(s)` in summary header, got:\n%s", out)
	}
}

func TestReportForkedSkips_NoopWhenEmpty(t *testing.T) {
	defer checksums.ResetSkipWrite()
	checksums.SkippedForkedThisRun = nil

	buf, restore := captureStderr(t)
	reportForkedSkips(nil)
	restore()

	if got := buf.String(); got != "" {
		t.Errorf("expected silent no-op when no skips recorded; got: %q", got)
	}
}

// TestReportForkedSkips_AcceptedAreSilent: established forks (Accepted:
// true) MUST NOT print. This is the cp-forge friction: 11 long-standing
// forked files were reporting on every generate run, drowning out new
// fork detections.
func TestReportForkedSkips_AcceptedAreSilent(t *testing.T) {
	defer checksums.ResetSkipWrite()
	checksums.SkippedForkedThisRun = []string{
		"pkg/app/wire_gen.go",
		"pkg/app/bootstrap.go",
	}
	// All entries already marked Accepted = true (the user has been
	// informed about these forks in a prior run).
	cs := mkForkedChecksums(true, "pkg/app/wire_gen.go", "pkg/app/bootstrap.go")

	buf, restore := captureStderr(t)
	reportForkedSkips(cs)
	restore()

	if got := buf.String(); got != "" {
		t.Errorf("accepted forks must not print; got:\n%s", got)
	}
}

// TestReportForkedSkips_MixedAcceptedAndNew: when one path is already
// Accepted and a NEW fork shows up, only the new one prints. This
// keeps the new-fork signal loud without re-nagging on established
// forks.
func TestReportForkedSkips_MixedAcceptedAndNew(t *testing.T) {
	defer checksums.ResetSkipWrite()
	checksums.SkippedForkedThisRun = []string{
		"pkg/app/wire_gen.go",   // accepted (old)
		"pkg/app/bootstrap.go",  // not accepted (new fork)
	}
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{
		"pkg/app/wire_gen.go":  {Hash: "x", Forked: true, Accepted: true},
		"pkg/app/bootstrap.go": {Hash: "y", Forked: true, Accepted: false},
	}}

	buf, restore := captureStderr(t)
	reportForkedSkips(cs)
	restore()

	out := buf.String()
	if strings.Contains(out, "pkg/app/wire_gen.go") {
		t.Errorf("accepted path must not appear; got:\n%s", out)
	}
	if !strings.Contains(out, "pkg/app/bootstrap.go") {
		t.Errorf("unaccepted (new) path must appear; got:\n%s", out)
	}
	if !strings.Contains(out, "1 forked file(s)") {
		t.Errorf("expected `1 forked file(s)` in header (only the new one), got:\n%s", out)
	}
}

// TestReportForkedSkips_FlipsAcceptedAfterFirstReport pins the
// auto-quiet contract: after the report fires for a path, the checksum
// entry's Accepted flag is set so the NEXT run's report stays silent.
// The deferred SaveChecksums in runGeneratePipeline persists this to
// .forge/checksums.json.
func TestReportForkedSkips_FlipsAcceptedAfterFirstReport(t *testing.T) {
	defer checksums.ResetSkipWrite()
	checksums.SkippedForkedThisRun = []string{"pkg/app/wire_gen.go"}
	cs := mkForkedChecksums(false, "pkg/app/wire_gen.go")

	buf, restore := captureStderr(t)
	reportForkedSkips(cs)
	restore()

	// First call: must have printed.
	if !strings.Contains(buf.String(), "pkg/app/wire_gen.go") {
		t.Fatalf("first report should print path; got:\n%s", buf.String())
	}
	// And must have flipped Accepted to true in-memory so SaveChecksums
	// persists it.
	if !cs.Files["pkg/app/wire_gen.go"].Accepted {
		t.Errorf("Accepted not set after first report; got entry=%+v", cs.Files["pkg/app/wire_gen.go"])
	}

	// Second call (same checksums, same SkippedForkedThisRun): silent.
	buf2, restore2 := captureStderr(t)
	reportForkedSkips(cs)
	restore2()
	if got := buf2.String(); got != "" {
		t.Errorf("second report (Accepted: true) must be silent; got:\n%s", got)
	}
}

// TestReportForkedSkips_NilChecksumsFallsBack guards the defensive nil
// path. The pipeline always passes a real *FileChecksums today, but
// future refactors should not silently break test fixtures that call
// reportForkedSkips with nil.
func TestReportForkedSkips_NilChecksumsFallsBack(t *testing.T) {
	defer checksums.ResetSkipWrite()
	checksums.SkippedForkedThisRun = []string{"pkg/app/wire_gen.go"}

	buf, restore := captureStderr(t)
	reportForkedSkips(nil) // nil checksums — must not panic, must still report.
	restore()

	if !strings.Contains(buf.String(), "pkg/app/wire_gen.go") {
		t.Errorf("nil checksums should fall back to legacy report-every-time; got:\n%s", buf.String())
	}
}
