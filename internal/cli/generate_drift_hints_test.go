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
		{"pkg/app/bootstrap.go", []string{"internal/app/providers.go", "internal/app/compose.go", "OpenInfra"}},
		{"pkg/app/app_gen.go", []string{"internal/app/providers.go", "internal/app/compose.go"}},
		{"pkg/app/wire_gen.go", []string{"internal/app/providers.go", "internal/app/compose.go"}},
		{"internal/handlers/echo/authorizer_gen.go", []string{"internal/handlers/echo/authorizer.go", "user-owned", "NewAuthorizer()"}},
		{"internal/handlers/orders/handlers_gen.go", []string{"contract.go", "proto"}},
		{"internal/handlers/orders/mock_gen.go", []string{"contract.go", "proto"}},
		// No designated extension point — no hint.
		{"pkg/app/testing.go", nil},
		{"pkg/config/config.go", nil},
		{"internal/cli/serve.go", nil},
		// Leading ./ normalized.
		{"./pkg/app/bootstrap.go", []string{"internal/app/providers.go"}},
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
// extension point leads, `forge disown` trails as the LAST resort and
// is described as a one-way permanent transfer, friction recording and
// --explain-drift are advertised.
func TestFormatTier1DriftReport(t *testing.T) {
	drift := []checksums.Tier1DriftEntry{
		{Path: "pkg/app/wire_gen.go", RecordedHash: "aaaa1111", OnDiskHash: "bbbb2222"},
		{Path: "pkg/config/config.go", RecordedHash: "cccc3333", OnDiskHash: "dddd4444"},
	}
	got := formatTier1DriftReport(drift)

	for _, want := range []string{
		"2 Tier-1 file(s) modified",
		"pkg/app/wire_gen.go",
		// The hash lines speak the self-certification vocabulary: the
		// EMBEDDED marker hash vs the recomputed CURRENT body hash.
		"embedded: aaaa1111",
		"current:  bbbb2222",
		"↪ custom wiring belongs in internal/app/providers.go (OpenInfra) + internal/app/compose.go (NewComponents) — the retired pkg/app DI unit no longer runs",
		"--explain-drift",
		"forge friction add",
		"forge disown <path> --reason",
		"ONE-WAY transfer",
		"Forge never updates the file again",
		"deleting the file and running `forge generate`",
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
	// disown one-way door.
	if strings.Index(got, "extension point") > strings.Index(got, "forge disown") {
		t.Errorf("extension-point guidance must precede the disown option:\n%s", got)
	}
}

// TestFormatTier1DriftReport_UnverifiedSentinel pins the wording for
// the legacy-migration sentinel: a file whose provenance could not be
// established when the project migrated off .forge/checksums.json is
// reported with the unverified-legacy marker value and an explanation,
// not the ordinary "hash stamped at the last forge write" line.
func TestFormatTier1DriftReport_UnverifiedSentinel(t *testing.T) {
	drift := []checksums.Tier1DriftEntry{
		{Path: "pkg/app/wire_gen.go", OnDiskHash: "bbbb2222", Unverified: true},
	}
	got := formatTier1DriftReport(drift)
	for _, want := range []string{
		"embedded: " + checksums.UnverifiedMarkerValue,
		"provenance unknown since the legacy checksums.json migration",
		"current:  bbbb2222",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unverified report missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hash stamped at the last forge write") {
		t.Errorf("unverified entry must not claim a recorded write hash:\n%s", got)
	}
}
