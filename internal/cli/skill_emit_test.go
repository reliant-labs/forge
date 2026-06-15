package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseFrontmatter_ParsesEmitField verifies the `emit:` frontmatter
// field flows through to skillMeta.Emit verbatim.
func TestParseFrontmatter_ParsesEmitField(t *testing.T) {
	cases := []struct {
		emit string
		want SkillEmit
	}{
		{"forge", SkillEmitForge},
		{"general", SkillEmitGeneral},
		{"both", SkillEmitBoth},
	}
	for _, tc := range cases {
		t.Run(tc.emit, func(t *testing.T) {
			body := []byte("---\nname: x\ndescription: y\nemit: " + tc.emit + "\n---\nbody\n")
			got := parseFrontmatter(body)
			if got.Emit != tc.want {
				t.Errorf("emit=%q: got Emit=%q, want %q", tc.emit, got.Emit, tc.want)
			}
		})
	}
}

// TestParseFrontmatter_EmitDefaultsToEmpty verifies an absent `emit:` key
// leaves Emit as the zero value. The audience matcher treats "" as
// SkillEmitForge for back-compat; that is asserted by TestEmitMatchesAudience.
func TestParseFrontmatter_EmitDefaultsToEmpty(t *testing.T) {
	body := []byte("---\nname: x\ndescription: y\n---\nbody\n")
	got := parseFrontmatter(body)
	if got.Emit != "" {
		t.Errorf("expected empty Emit when frontmatter omits the field, got %q", got.Emit)
	}
}

// TestEmitMatchesAudience covers every (emit, audience) combination
// including the empty-emit-defaults-to-forge back-compat rule.
func TestEmitMatchesAudience(t *testing.T) {
	cases := []struct {
		emit     SkillEmit
		audience SkillAudience
		want     bool
	}{
		// All audience: every skill passes regardless of emit.
		{SkillEmitForge, SkillAudienceAll, true},
		{SkillEmitGeneral, SkillAudienceAll, true},
		{SkillEmitBoth, SkillAudienceAll, true},
		{"", SkillAudienceAll, true},

		// General audience: only general + both pass.
		{SkillEmitGeneral, SkillAudienceGeneral, true},
		{SkillEmitBoth, SkillAudienceGeneral, true},
		{SkillEmitForge, SkillAudienceGeneral, false},
		{"", SkillAudienceGeneral, false}, // empty defaults to forge → drop for general

		// Forge audience: only forge + both pass.
		{SkillEmitForge, SkillAudienceForge, true},
		{SkillEmitBoth, SkillAudienceForge, true},
		{SkillEmitGeneral, SkillAudienceForge, false},
		{"", SkillAudienceForge, true}, // empty defaults to forge → keep for forge
	}
	for _, tc := range cases {
		got := emitMatchesAudience(tc.emit, tc.audience)
		if got != tc.want {
			t.Errorf("emit=%q audience=%q: got %v want %v", tc.emit, tc.audience, got, tc.want)
		}
	}
}

const sampleSkillBody = `# Heading

Before the block.

<!-- @forge-only:start -->
## Forge Tools

forge run --debug
<!-- @forge-only:end -->

After the block.
`

// TestRenderSkillForAudience_General strips the @forge-only block.
func TestRenderSkillForAudience_General(t *testing.T) {
	out := string(RenderSkillForAudience([]byte(sampleSkillBody), SkillAudienceGeneral))
	if strings.Contains(out, "Forge Tools") {
		t.Errorf("expected @forge-only content removed, got:\n%s", out)
	}
	if strings.Contains(out, "forge run --debug") {
		t.Errorf("expected forge command removed, got:\n%s", out)
	}
	if strings.Contains(out, "@forge-only") {
		t.Errorf("expected marker lines removed, got:\n%s", out)
	}
	if !strings.Contains(out, "Before the block.") || !strings.Contains(out, "After the block.") {
		t.Errorf("expected surrounding content preserved, got:\n%s", out)
	}
}

// TestRenderSkillForAudience_Forge keeps everything.
func TestRenderSkillForAudience_Forge(t *testing.T) {
	out := string(RenderSkillForAudience([]byte(sampleSkillBody), SkillAudienceForge))
	if out != sampleSkillBody {
		t.Errorf("expected verbatim body for forge audience, got:\n%s", out)
	}
}

// TestRenderSkillForAudience_All is a no-op.
func TestRenderSkillForAudience_All(t *testing.T) {
	out := string(RenderSkillForAudience([]byte(sampleSkillBody), SkillAudienceAll))
	if out != sampleSkillBody {
		t.Errorf("expected verbatim body for all audience, got:\n%s", out)
	}
}

// TestRenderSkillForAudience_CollapsesBlankRuns ensures stripping a block
// doesn't leave a double-blank gap in the output.
func TestRenderSkillForAudience_CollapsesBlankRuns(t *testing.T) {
	out := string(RenderSkillForAudience([]byte(sampleSkillBody), SkillAudienceGeneral))
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("expected blank-line runs collapsed, got:\n%q", out)
	}
}

// TestRenderSkillForAudience_WhitespaceTolerantMarkers verifies extra
// whitespace inside or around the HTML comment markers still parses.
func TestRenderSkillForAudience_WhitespaceTolerantMarkers(t *testing.T) {
	body := []byte("keep\n   <!--   @forge-only:start   -->   \ndrop\n<!--@forge-only:end-->\nkeep\n")
	out := string(RenderSkillForAudience(body, SkillAudienceGeneral))
	if strings.Contains(out, "drop") {
		t.Errorf("expected drop content removed, got:\n%s", out)
	}
	if !strings.Contains(out, "keep") {
		t.Errorf("expected keep content retained, got:\n%s", out)
	}
}

// TestRenderSkillForAudience_UnterminatedBlock drops to EOF when the end
// marker is missing — authors notice via missing content rather than
// silent passthrough.
func TestRenderSkillForAudience_UnterminatedBlock(t *testing.T) {
	body := []byte("keep\n<!-- @forge-only:start -->\ndrop1\ndrop2\n")
	out := string(RenderSkillForAudience(body, SkillAudienceGeneral))
	if strings.Contains(out, "drop1") || strings.Contains(out, "drop2") {
		t.Errorf("expected unterminated block content removed, got:\n%s", out)
	}
}

// TestRenderSkillForAudience_MultipleBlocks handles more than one block
// in a single body.
func TestRenderSkillForAudience_MultipleBlocks(t *testing.T) {
	body := []byte("a\n<!-- @forge-only:start -->\nb\n<!-- @forge-only:end -->\nc\n<!-- @forge-only:start -->\nd\n<!-- @forge-only:end -->\ne\n")
	out := string(RenderSkillForAudience(body, SkillAudienceGeneral))
	for _, drop := range []string{"b", "d"} {
		if strings.Contains(out, "\n"+drop+"\n") {
			t.Errorf("expected %q removed, got:\n%s", drop, out)
		}
	}
	for _, keep := range []string{"a", "c", "e"} {
		if !strings.Contains(out, keep) {
			t.Errorf("expected %q retained, got:\n%s", keep, out)
		}
	}
}

// TestWriteSkills_GeneralAudience drives the full pipeline through the
// embedded templates: only emit:general|both skills survive the filter,
// and @forge-only blocks are stripped from the bodies that do.
//
// debug/SKILL.md ships with emit: both and a @forge-only "Forge-Specific
// Debug Tools" section — so it's the canonical witness for both the
// filter and the renderer.
func TestWriteSkills_GeneralAudience(t *testing.T) {
	dir := t.TempDir()
	n, err := WriteSkills(dir, SkillWriteStyleClaude, SkillAudienceGeneral)
	if err != nil {
		t.Fatalf("WriteSkills: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one general-audience skill (debug)")
	}

	// Spot-check debug — it must survive the filter, and the
	// @forge-only block must be stripped.
	body, err := readSkillFile(dir, "debug")
	if err != nil {
		t.Fatalf("read debug: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "@forge-only") {
		t.Errorf("debug skill still has @forge-only markers after general render:\n%s", s)
	}
	if strings.Contains(s, "Forge-Specific Debug Tools") {
		t.Errorf("debug skill still contains forge-only section after general render:\n%s", s)
	}
	if !strings.Contains(s, "Triage First") {
		t.Errorf("debug skill missing methodology body:\n%s", s)
	}

	// At least one emit:forge (or emit-unset) skill must NOT have been
	// written — e.g. proto is unambiguously framework-only. If proto
	// ever moves to emit:general this assertion needs to follow.
	if _, err := readSkillFile(dir, "proto"); err == nil {
		t.Errorf("emit:forge skill 'proto' should not be written for general audience")
	}
}

// readSkillFile is a tiny helper to read the SKILL.md a claude-style
// WriteSkills wrote for the given skill path.
func readSkillFile(dir, skillPath string) ([]byte, error) {
	flat := strings.ReplaceAll(skillPath, "/", "-")
	return os.ReadFile(filepath.Join(dir, flat, "SKILL.md"))
}
