// frontend_presentation_helpers_test.go — the presentation helpers every
// forge frontend needs must SHIP in the scaffold and be NAMED in the skill.
//
// Measured against a real build (dogfood run "roofloop"): four sub-agent
// threads independently hand-wrote whole-dollar money formatting twice and
// proto-Timestamp date formatting three times, and installed zero components
// from the 74-component library while hand-writing a stat block and a funnel.
// A helper that ships but is not named in the skill is a helper that gets
// rewritten.

package templates

import (
	"path/filepath"
	"strings"
	"testing"
)

// formatUtilsScaffold returns the shared format-utils.ts every frontend kind
// (next, vite-spa, react-native) scaffolds.
func formatUtilsScaffold(t *testing.T) string {
	t.Helper()
	b, err := FrontendTemplates().Get(filepath.Join("shared", "src", "lib", "format-utils.ts"))
	if err != nil {
		t.Fatalf("read shared/src/lib/format-utils.ts: %v", err)
	}
	return string(b)
}

// frontendSkill returns the shipped frontend SKILL.md.
func frontendSkill(t *testing.T) string {
	t.Helper()
	b, err := ProjectTemplates().Get("skills/forge/frontend/SKILL.md")
	if err != nil {
		t.Fatalf("read skills/forge/frontend/SKILL.md: %v", err)
	}
	return string(b)
}

// TestFormatUtilsShipsWholeDollarMoney pins the whole-dollar money helper.
//
// Money is int64 cents by convention, so every forge app needs both spellings:
// formatMoneyCents ("$272,000.00") for an invoice line that reconciles to the
// penny, and the whole-dollar one ("$272,000") for a dashboard headline where
// the trailing ".00" is noise. Only the first shipped, so the second was
// hand-written per-agent.
func TestFormatUtilsShipsWholeDollarMoney(t *testing.T) {
	t.Parallel()

	fu := formatUtilsScaffold(t)

	if !strings.Contains(fu, "export function formatMoneyWhole(") {
		t.Error("shared/src/lib/format-utils.ts does not export formatMoneyWhole — " +
			"agents hand-write the whole-dollar variant of formatMoneyCents instead " +
			"(measured: written twice in one build)")
	}

	// The behavioural contract, not just the name: whole dollars means the
	// fraction digits are pinned to zero, or it is just formatMoneyCents again.
	if !strings.Contains(fu, "maximumFractionDigits: 0") {
		t.Error("formatMoneyWhole does not pin maximumFractionDigits: 0 — " +
			"a whole-dollar formatter that still prints cents is the helper it replaces")
	}
}

// TestFormatUtilsShipsTimestampHelpers pins proto-Timestamp date formatting.
//
// A protobuf-es Timestamp is `{ seconds: bigint; nanos?: number }`, which no
// JS date API accepts. Converting it is universal to forge frontends and was
// hand-written in three separate modules of one app.
func TestFormatUtilsShipsTimestampHelpers(t *testing.T) {
	t.Parallel()

	fu := formatUtilsScaffold(t)

	for _, fn := range []string{
		"export function timestampToDate(",
		"export function formatDate(",
		"export function formatDateTime(",
		"export function formatAge(",
	} {
		if !strings.Contains(fu, fn) {
			t.Errorf("shared/src/lib/format-utils.ts does not carry %q — "+
				"agents re-derive protobuf-es Timestamp ({seconds: bigint}) conversion per module", fn)
		}
	}

	// timestampToDate must reject a non-finite result rather than minting an
	// Invalid Date that renders as the string "Invalid Date" in the UI.
	if !strings.Contains(fu, "Number.isFinite") {
		t.Error("timestampToDate does not guard with Number.isFinite — " +
			"an out-of-range Timestamp must return null, not an Invalid Date")
	}
}

// TestFrontendSkillNamesFormatUtilsExports — the skill must NAME the helpers.
//
// The scaffolded file was found only by reading generated page source. An
// export nobody can find is an export that gets rewritten.
func TestFrontendSkillNamesFormatUtilsExports(t *testing.T) {
	t.Parallel()

	skill := frontendSkill(t)

	for _, export := range []string{
		"formatMoneyCents",
		"formatMoneyWhole",
		"enumLabel",
		"enumOptions",
		"registerStatusVariants",
		"formatDate",
		"formatAge",
	} {
		if !strings.Contains(skill, export) {
			t.Errorf("frontend SKILL.md never names %q — "+
				"it ships in src/lib/format-utils.ts and agents hand-roll it instead", export)
		}
	}
}

// TestFrontendSkillPointsAtComponentLibraryAtDecisionPoint — the pointer has
// to sit where the decision is made.
//
// The skill already says "search it before you hand-write any UI" in its own
// section; the measured build still shipped a hand-written stat block and
// funnel while stat_grid, metric_card and funnel_chart sat uninstalled. The
// section header is not where an agent about to write a <div> is looking.
func TestFrontendSkillPointsAtComponentLibraryAtDecisionPoint(t *testing.T) {
	t.Parallel()

	skill := frontendSkill(t)

	// Name the components that were hand-written despite shipping, so the
	// search has a concrete noun to match.
	for _, name := range []string{"stat_grid", "metric_card", "empty_state"} {
		if !strings.Contains(skill, name) {
			t.Errorf("frontend SKILL.md never names the %q component — "+
				"it ships in the library and was hand-written in a measured build", name)
		}
	}

	// The library is opt-in beyond the core set: an agent that never runs
	// `forge component install` gets only what the scaffold auto-installed.
	// The skill must say that the installed set is NOT the catalog.
	if !strings.Contains(skill, "components/ui/` is not the catalog") {
		t.Error("frontend SKILL.md does not warn that src/components/ui/ is only the " +
			"auto-installed core set, not the whole catalog — agents read the directory, " +
			"conclude the library is exhausted, and hand-write the rest")
	}
}

// TestFrontendSkillStaysUnderCap — the delivered cap is 24000 bytes and the
// working cap is 15000.
func TestFrontendSkillStaysUnderCap(t *testing.T) {
	t.Parallel()

	if n := len(frontendSkill(t)); n >= 15000 {
		t.Errorf("frontend SKILL.md is %d bytes, over the 15000-byte working cap", n)
	}
}
