package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

func TestTier1ExtensionPointHint(t *testing.T) {
	tests := []struct {
		path     string
		wantSubs []string // all must appear in the hint; empty slice → want ""
	}{
		{"pkg/app/bootstrap.go", []string{"setup.go", "post_bootstrap.go", "app_extras.go", "user-owned"}},
		{"pkg/app/app_gen.go", []string{"setup.go", "post_bootstrap.go", "app_extras.go"}},
		{"pkg/app/wire_gen.go", []string{"setup.go", "post_bootstrap.go", "app_extras.go"}},
		{"handlers/echo/authorizer_gen.go", []string{"handlers/echo/authorizer.go", "user-owned", "NewAuthorizer()"}},
		{"handlers/orders/handlers_gen.go", []string{"contract.go", "proto"}},
		{"handlers/orders/mock_gen.go", []string{"contract.go", "proto"}},
		// No designated extension point — no hint.
		{"pkg/app/testing.go", nil},
		{"pkg/config/config.go", nil},
		{"cmd/server.go", nil},
		// Leading ./ normalized.
		{"./pkg/app/bootstrap.go", []string{"setup.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := tier1ExtensionPointHint(tt.path)
			if len(tt.wantSubs) == 0 {
				if got != "" {
					t.Errorf("hint for %s = %q, want none", tt.path, got)
				}
				return
			}
			for _, want := range tt.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("hint for %s missing %q; got %q", tt.path, want, got)
				}
			}
		})
	}
}

// TestFormatTier1DriftReport pins the message-design contract: the
// extension point leads, --accept trails and is described as permanent
// ownership ("never update this file again"), and --explain-drift is
// advertised.
func TestFormatTier1DriftReport(t *testing.T) {
	drift := []checksums.Tier1DriftEntry{
		{Path: "pkg/app/wire_gen.go", RecordedHash: "aaaa1111", OnDiskHash: "bbbb2222", HistoryDepth: 3},
		{Path: "pkg/config/config.go", RecordedHash: "cccc3333", OnDiskHash: "dddd4444", HistoryDepth: 1},
	}
	got := formatTier1DriftReport(drift)

	for _, want := range []string{
		"2 Tier-1 file(s) modified",
		"pkg/app/wire_gen.go",
		"↪ custom wiring belongs in pkg/app/setup.go / post_bootstrap.go / app_extras.go (user-owned)",
		"--explain-drift",
		"PERMANENT ownership",
		"forge will never update this file again",
		"forge unfork --merge",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q; got:\n%s", want, got)
		}
	}

	// Hint lines only appear for mapped paths — the config.go entry must
	// not carry a wiring hint.
	if strings.Count(got, "↪") != 1 {
		t.Errorf("expected exactly 1 hint line, got %d:\n%s", strings.Count(got, "↪"), got)
	}

	// Option ordering: the extension-point option must come before the
	// --accept option.
	if strings.Index(got, "extension point") > strings.Index(got, "--accept") {
		t.Errorf("extension-point guidance must precede --accept:\n%s", got)
	}
}
