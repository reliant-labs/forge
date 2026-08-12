package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authParityProject lays out a project whose dev env declares the given
// backend issuer ("" = none) and, for each frontend, the ISSUER it is
// declared to sign in against ("" = the frontend declares none).
//
// Both halves are written the way `forge generate` scaffolds them: typed
// instances in the env's own deploy/kcl/dev/config.k, the backend's on
// app_config and each frontend's on its frontend-config instance.
func authParityProject(t *testing.T, backendIssuerURL string, frontends map[string]string) string {
	t.Helper()
	root := t.TempDir()
	kclDir := filepath.Join(root, "deploy", "kcl", "dev")
	if err := os.MkdirAll(kclDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var body strings.Builder
	body.WriteString("import config_gen\n\napp_config: config_gen.AppConfig = {\n")
	if backendIssuerURL != "" {
		body.WriteString("    jwt_issuer = \"" + backendIssuerURL + "\"\n")
	}
	body.WriteString("}\n")
	for name, issuer := range frontends {
		if issuer == "" {
			continue
		}
		body.WriteString("\n" + name + "_config: frontend_config_gen.WebConfig = {\n")
		body.WriteString("    oidc_issuer = \"" + issuer + "\"\n")
		body.WriteString("}\n")
	}
	if err := os.WriteFile(filepath.Join(kclDir, "config.k"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func runParity(t *testing.T, root string) CheckResult {
	t.Helper()
	return CheckAuthParity(context.Background(), &Environment{ProjectDir: root, Env: "dev"})
}

// The scaffold's default — no identity anywhere — is a clean SKIP, not a
// warning. No key material is the correct closed-and-bootable state.
func TestAuthParity_UnconfiguredSkips(t *testing.T) {
	root := authParityProject(t, "", map[string]string{"web": ""})
	if got := runParity(t, root); got.Status != StatusSkip {
		t.Fatalf("status = %q (%s), want skip", got.Status, got.Message)
	}
}

// Agreeing issuers pass, and a trailing slash is not a disagreement.
func TestAuthParity_MatchingIssuersPass(t *testing.T) {
	root := authParityProject(t,
		"http://localhost:3001/oidc",
		map[string]string{"web": "http://localhost:3001/oidc/"})
	if got := runParity(t, root); got.Status != StatusPass {
		t.Fatalf("status = %q (%s), want pass", got.Status, got.Message)
	}
}

// The failure this check exists for: both sides configured, pointing at
// different issuers. Sign-in succeeds and every RPC still 401s.
func TestAuthParity_MismatchWarns(t *testing.T) {
	root := authParityProject(t,
		"https://issuer.example.com/oidc",
		map[string]string{"web": "http://localhost:3001/oidc"})
	got := runParity(t, root)
	if got.Status != StatusWarn {
		t.Fatalf("status = %q (%s), want warn", got.Status, got.Message)
	}
	if got.Evidence == "" {
		t.Error("a mismatch must report both issuers as evidence")
	}
}

// One side configured alone is also a warning — each direction has its own
// message, because the fix differs.
func TestAuthParity_OneSidedWarns(t *testing.T) {
	backendOnly := authParityProject(t,
		"http://localhost:3001/oidc", map[string]string{"web": ""})
	if got := runParity(t, backendOnly); got.Status != StatusWarn {
		t.Errorf("backend-only status = %q, want warn", got.Status)
	}

	frontendOnly := authParityProject(t, "",
		map[string]string{"web": "http://localhost:3001/oidc"})
	if got := runParity(t, frontendOnly); got.Status != StatusWarn {
		t.Errorf("frontend-only status = %q, want warn", got.Status)
	}
}

// The KCL reader has to handle both spellings the frontend issuer appears
// in — a dict entry in the generated identity fragment and a schema field
// in a hand-authored config — plus comments, which a naive scan would
// happily read a commented-out issuer from.
func TestReadKCLStringValue(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			// A dict entry, the spelling a projection map uses. Kept
			// readable alongside the schema-field form below because the
			// check must not care which of the two a project authored.
			name: "dict entry",
			body: "# \"OIDC_ISSUER\" = \"http://commented-out\"\n" +
				"identity = {\n    \"OIDC_ISSUER\" = \"http://real/oidc\"\n}\n",
			want: "http://real/oidc",
		},
		{
			// The proto spells the field oidc_issuer and the projection key
			// OIDC_ISSUER; matching case-insensitively means a hand-authored
			// config counts too, rather than the check going quiet on
			// exactly the setup most likely to have drifted.
			name: "schema field (hand-authored config)",
			body: "web_config: fc.WebConfig = {\n    oidc_issuer = \"http://schema/oidc\"\n}\n",
			want: "http://schema/oidc",
		},
		{
			name: "absent",
			body: "other = \"x\"\n",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".k")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := readKCLStringValue(path, "OIDC_ISSUER"); got != tc.want {
				t.Errorf("readKCLStringValue = %q, want %q", got, tc.want)
			}
		})
	}
}

// The backend's issuer now comes from the SAME declaration file as the
// frontend's — a jwt_issuer field on the env's app_config instance, not a
// JWT_ISSUER line in a dotenv. This pins that move: a project whose only
// issuer is declared in config.k must be discoverable, or the check reports
// "the frontend names an issuer but the backend validates against none" for
// every correctly-configured project.
func TestBackendIssuerReadsEnvConfigK(t *testing.T) {
	root := authParityProject(t, "http://localhost:8080", nil)
	if got := backendIssuer(root, "dev"); got != "http://localhost:8080" {
		t.Errorf("backendIssuer = %q, want %q", got, "http://localhost:8080")
	}
	if got := backendIssuer(t.TempDir(), "dev"); got != "" {
		t.Errorf("a project with no config.k = %q, want empty", got)
	}
}
