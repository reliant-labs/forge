package generator

import "fmt"

// Harness identifies which AI tool's conventions to scaffold for at
// project creation time: the top-level memory file's path, and (for
// harnesses that have a native skills concept) the on-disk directory
// where forge skills should be emitted. The default is "reliant".
type Harness string

const (
	HarnessReliant Harness = "reliant"
	HarnessClaude  Harness = "claude"
	HarnessCursor  Harness = "cursor"
	HarnessCopilot Harness = "copilot"
	HarnessCodex   Harness = "codex"
)

// ValidHarnesses lists all accepted --harness values. Returned as a
// getter to satisfy the no-exported-vars rule; callers should treat the
// returned slice as read-only (it is rebuilt on each call).
func ValidHarnesses() []Harness {
	return []Harness{
		HarnessReliant,
		HarnessClaude,
		HarnessCursor,
		HarnessCopilot,
		HarnessCodex,
	}
}

// MemoryFilePath returns the project-root-relative path for the top-level
// memory file corresponding to the given harness.
func (h Harness) MemoryFilePath() string {
	switch h {
	case HarnessClaude:
		return "CLAUDE.md"
	case HarnessCursor:
		return ".cursorrules"
	case HarnessCopilot:
		return ".github/copilot-instructions.md"
	case HarnessCodex:
		return "AGENTS.md"
	default: // reliant or unset
		return "reliant.md"
	}
}

// SkillsDir returns the project-root-relative directory where forge
// skills should be written on `forge new` for this harness, or "" when
// the harness has no native skills concept. Reliant returns "" because
// the reliant CLI auto-discovers forge skills via the project's
// forge.yaml (no on-disk emission needed); copilot/codex return ""
// because they have no native skills mechanism.
func (h Harness) SkillsDir() string {
	switch h {
	case HarnessClaude:
		return ".claude/skills"
	default:
		return ""
	}
}

// ParseHarness validates and normalises a user-supplied string.
func ParseHarness(s string) (Harness, error) {
	switch Harness(s) {
	case HarnessReliant, HarnessClaude, HarnessCursor, HarnessCopilot, HarnessCodex:
		return Harness(s), nil
	case "":
		return HarnessReliant, nil
	default:
		return "", fmt.Errorf("unknown harness %q; valid harnesses: reliant, claude, cursor, copilot, codex", s)
	}
}
