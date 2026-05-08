package generator

import "fmt"

// MemoryFormat identifies which AI tool's memory-file convention to use
// for the top-level project memory file. The default is "reliant".
type MemoryFormat string

const (
	MemoryFormatReliant MemoryFormat = "reliant"
	MemoryFormatClaude  MemoryFormat = "claude"
	MemoryFormatCursor  MemoryFormat = "cursor"
	MemoryFormatCopilot MemoryFormat = "copilot"
	MemoryFormatCodex   MemoryFormat = "codex"
)

// ValidMemoryFormats lists all accepted --memory values. Returned as a
// getter to satisfy the no-exported-vars rule; callers should treat the
// returned slice as read-only (it is rebuilt on each call).
func ValidMemoryFormats() []MemoryFormat {
	return []MemoryFormat{
		MemoryFormatReliant,
		MemoryFormatClaude,
		MemoryFormatCursor,
		MemoryFormatCopilot,
		MemoryFormatCodex,
	}
}

// MemoryFilePath returns the project-root-relative path for the top-level
// memory file corresponding to the given format.
func (m MemoryFormat) MemoryFilePath() string {
	switch m {
	case MemoryFormatClaude:
		return "CLAUDE.md"
	case MemoryFormatCursor:
		return ".cursorrules"
	case MemoryFormatCopilot:
		return ".github/copilot-instructions.md"
	case MemoryFormatCodex:
		return "AGENTS.md"
	default: // reliant or unset
		return "reliant.md"
	}
}

// ParseMemoryFormat validates and normalises a user-supplied string.
func ParseMemoryFormat(s string) (MemoryFormat, error) {
	switch MemoryFormat(s) {
	case MemoryFormatReliant, MemoryFormatClaude, MemoryFormatCursor, MemoryFormatCopilot, MemoryFormatCodex:
		return MemoryFormat(s), nil
	case "":
		return MemoryFormatReliant, nil
	default:
		return "", fmt.Errorf("unknown memory format %q; valid formats: reliant, claude, cursor, copilot, codex", s)
	}
}
