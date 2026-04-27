package generator

import "testing"

func TestParseMemoryFormat(t *testing.T) {
	tests := []struct {
		input   string
		want    MemoryFormat
		wantErr bool
	}{
		{"reliant", MemoryFormatReliant, false},
		{"claude", MemoryFormatClaude, false},
		{"cursor", MemoryFormatCursor, false},
		{"copilot", MemoryFormatCopilot, false},
		{"codex", MemoryFormatCodex, false},
		{"", MemoryFormatReliant, false},
		{"unknown", "", true},
		{"Claude", "", true}, // case-sensitive
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseMemoryFormat(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseMemoryFormat(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseMemoryFormat(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMemoryFilePath(t *testing.T) {
	tests := []struct {
		format MemoryFormat
		want   string
	}{
		{MemoryFormatReliant, "reliant.md"},
		{MemoryFormatClaude, "CLAUDE.md"},
		{MemoryFormatCursor, ".cursorrules"},
		{MemoryFormatCopilot, ".github/copilot-instructions.md"},
		{MemoryFormatCodex, "AGENTS.md"},
		{"", "reliant.md"}, // zero-value defaults to reliant
	}
	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			got := tt.format.MemoryFilePath()
			if got != tt.want {
				t.Fatalf("MemoryFormat(%q).MemoryFilePath() = %q, want %q", tt.format, got, tt.want)
			}
		})
	}
}
