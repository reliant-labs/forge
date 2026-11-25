package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cloud"
)

// On a Reliant machine the credential helper counts as signed in, so
// --claimable is refused; the refusal names how to deploy anonymously on
// purpose (obs 88c94bfa).
func TestClaimableEscapeNamesTheCredentialToDrop(t *testing.T) {
	t.Setenv("FORGE_CREDENTIAL_HELPER", "reliant auth forge-credential")
	t.Setenv(cloud.DefaultTokenEnv, "")
	if got := claimableEscape(cloud.SourceHelper, "prod"); !strings.Contains(got, "env -u FORGE_CREDENTIAL_HELPER ") || !strings.Contains(got, "env deploy prod --claimable") {
		t.Errorf("helper escape = %q", got)
	}
	// A login AND a helper: both are removed, or the helper refuses next.
	got := claimableEscape(cloud.SourceLogin, "prod")
	if !strings.Contains(got, "-u FORGE_CREDENTIAL_HELPER") || !strings.Contains(got, "FORGE_HOME=<empty dir>") || !strings.Contains(got, "keep that FORGE_HOME") {
		t.Errorf("login escape = %q", got)
	}
	t.Setenv("FORGE_CREDENTIAL_HELPER", "")
	if got := claimableEscape(cloud.SourceEnv, "prod"); !strings.Contains(got, "-u "+cloud.DefaultTokenEnv) || strings.Contains(got, "FORGE_CREDENTIAL_HELPER") {
		t.Errorf("env escape = %q", got)
	}
	if got := claimableEscape(cloud.SourceFlag, "prod"); got != "" {
		t.Errorf("flag escape = %q, want none (drop --token)", got)
	}
}
