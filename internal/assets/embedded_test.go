// File: internal/assets/embedded_test.go
//
// The embedded forge.proto under internal/assets/proto/forge/v1/ is a
// hand-maintained copy of the source-of-truth at proto/forge/v1/. They
// MUST stay byte-equivalent modulo the `go_package` option line (the
// embedded copy points at the published forge module path; the source
// points at the local internal/ path). Any other drift means the
// scaffolded project gets a stale schema — silently. E2E validation of
// the Tier 2 typed-errors work surfaced exactly this class of bug:
// the source proto declared `repeated string errors = 6` but the
// embedded copy didn't, so `forge new` projects couldn't use the new
// annotation. This test pins the sync as a build-time invariant so
// future schema bumps can't ship half-applied.
package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// embeddedGoPackageLine is the canonical go_package option in the
	// embedded copy — it points at the PUBLISHED forge module path
	// (no `internal/`) because new scaffolded projects import the
	// public forge module, not the internal source layout. Diff
	// strips both files' go_package lines so only that one line is
	// allowed to differ; every other byte must match.
	embeddedGoPackageLine = `option go_package = "github.com/reliant-labs/forge/gen/forge/v1;forgev1";`
	sourceGoPackageLine   = `option go_package = "github.com/reliant-labs/forge/internal/gen/forge/v1;forgev1";`
)

// TestEmbeddedForgeProtoMatchesSource compares the source-of-truth proto
// against the embedded copy. The only legal difference is the
// go_package line — that's a deliberate rewrite the WriteForgeV1Proto
// helper handles per-project. Every other byte must match.
//
// FRICTION (cp-forge, 2026-06-09): the embedded copy missed the
// `errors = 6` field bump, so `forge new` projects couldn't use the
// new typed-errors proto annotation even though forge codegen
// fully supported it. The bug surfaced only during end-to-end
// validation against a freshly-scaffolded project — none of the
// codegen-side unit tests caught it because they tested against the
// source proto. This invariant test makes that whole class of skew
// loud-at-build-time.
func TestEmbeddedForgeProtoMatchesSource(t *testing.T) {
	// Walk up from internal/assets/ to the forge repo root so we can
	// reach the source-of-truth at proto/forge/v1/forge.proto. Test
	// runs under `go test ./internal/assets/` so the cwd is the
	// package dir.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/assets → ../../  → repo root
	repoRoot := filepath.Join(cwd, "..", "..")

	sourcePath := filepath.Join(repoRoot, "proto", "forge", "v1", "forge.proto")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read source proto at %s: %v", sourcePath, err)
	}

	embedded, err := GetForgeV1Proto()
	if err != nil {
		t.Fatalf("read embedded proto: %v", err)
	}

	// Strip both go_package lines so the diff only flags real schema
	// drift. We strip exact-line matches rather than splice the file
	// because the embedded copy is hand-maintained and any whitespace
	// reformatting around the go_package line would otherwise look
	// like drift.
	srcStripped := stripLine(string(source), sourceGoPackageLine)
	embStripped := stripLine(string(embedded), embeddedGoPackageLine)

	if srcStripped == embStripped {
		return // in sync
	}

	// Out of sync. Render a useful diff: which lines exist in one but
	// not the other. We deliberately don't pull in a diff library — a
	// hand-rolled per-line scan is plenty for a config-shape mismatch
	// and surfaces the specific schema gaps that a future drift might
	// introduce.
	srcLines := strings.Split(srcStripped, "\n")
	embLines := strings.Split(embStripped, "\n")
	srcSet := setOf(srcLines)
	embSet := setOf(embLines)

	var missingFromEmbed, missingFromSource []string
	for _, line := range srcLines {
		if !embSet[line] {
			missingFromEmbed = append(missingFromEmbed, line)
		}
	}
	for _, line := range embLines {
		if !srcSet[line] {
			missingFromSource = append(missingFromSource, line)
		}
	}

	var b strings.Builder
	b.WriteString("embedded forge.proto is OUT OF SYNC with the source-of-truth.\n")
	b.WriteString("Sync by copying proto/forge/v1/forge.proto over internal/assets/proto/forge/v1/forge.proto,\n")
	b.WriteString("then changing the go_package line to:\n  ")
	b.WriteString(embeddedGoPackageLine)
	b.WriteString("\n\n")
	if len(missingFromEmbed) > 0 {
		b.WriteString("Lines in SOURCE but missing from EMBEDDED:\n")
		for _, l := range missingFromEmbed {
			b.WriteString("  - ")
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	if len(missingFromSource) > 0 {
		b.WriteString("Lines in EMBEDDED but missing from SOURCE (likely stale or experimental):\n")
		for _, l := range missingFromSource {
			b.WriteString("  + ")
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	t.Fatal(b.String())
}

func stripLine(s, line string) string {
	out := make([]string, 0, strings.Count(s, "\n")+1)
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == strings.TrimSpace(line) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func setOf(lines []string) map[string]bool {
	out := make(map[string]bool, len(lines))
	for _, l := range lines {
		out[l] = true
	}
	return out
}
