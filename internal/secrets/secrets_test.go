package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDotenv writes a dotenv into a temp dir and returns its path.
func writeDotenv(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, ".env.secrets")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write dotenv: %v", err)
	}
	return p
}

func TestNewProvider_Nil_Noop(t *testing.T) {
	p, err := NewProvider(nil)
	if err != nil {
		t.Fatalf("NewProvider(nil): %v", err)
	}
	if p.Kind() != "none" {
		t.Errorf("Kind: got %q, want none", p.Kind())
	}
	if _, ok := p.Resolve("ANYTHING"); ok {
		t.Error("noop Resolve returned ok=true")
	}
	if p.All() != nil {
		t.Errorf("noop All: got %v, want nil", p.All())
	}
}

func TestNewProvider_External(t *testing.T) {
	p, err := NewProvider(&ProviderConfig{Type: "external"})
	if err != nil {
		t.Fatalf("NewProvider external: %v", err)
	}
	if p.Kind() != "external" {
		t.Errorf("Kind: got %q, want external", p.Kind())
	}
	if _, ok := p.Resolve("X"); ok {
		t.Error("external Resolve returned ok=true")
	}
	if p.All() != nil {
		t.Errorf("external All: got %v, want nil", p.All())
	}
}

func TestNewProvider_Dotenv_Present(t *testing.T) {
	path := writeDotenv(t, "GITHUB_CLIENT_ID=abc123\nSUPABASE_JWT_ISSUER=https://x\n# comment\n")
	p, err := NewProvider(&ProviderConfig{Type: "dotenv", Path: path})
	if err != nil {
		t.Fatalf("NewProvider dotenv: %v", err)
	}
	if p.Kind() != "dotenv" {
		t.Errorf("Kind: got %q, want dotenv", p.Kind())
	}
	v, ok := p.Resolve("GITHUB_CLIENT_ID")
	if !ok || v != "abc123" {
		t.Errorf("Resolve GITHUB_CLIENT_ID: got (%q,%v), want (abc123,true)", v, ok)
	}
	if _, ok := p.Resolve("MISSING"); ok {
		t.Error("Resolve MISSING returned ok=true")
	}
	all := p.All()
	if len(all) != 2 {
		t.Errorf("All: got %d entries, want 2 (%v)", len(all), all)
	}
}

func TestNewProvider_Dotenv_MissingFile_NonFatal(t *testing.T) {
	// Point at a path that doesn't exist: non-fatal, empty dotenv provider.
	missing := filepath.Join(t.TempDir(), "does-not-exist.env")
	p, err := NewProvider(&ProviderConfig{Type: "dotenv", Path: missing})
	if err != nil {
		t.Fatalf("NewProvider missing dotenv should be non-fatal, got err: %v", err)
	}
	if p.Kind() != "dotenv" {
		t.Errorf("Kind: got %q, want dotenv", p.Kind())
	}
	if len(p.All()) != 0 {
		t.Errorf("All on missing dotenv: got %v, want empty", p.All())
	}
}

func TestNewProvider_Dotenv_IsDirectory_Fatal(t *testing.T) {
	// A directory where a file is expected is NOT os.ErrNotExist -> fatal.
	dir := t.TempDir()
	_, err := NewProvider(&ProviderConfig{Type: "dotenv", Path: dir})
	if err == nil {
		t.Fatal("expected error reading a directory as dotenv, got nil")
	}
}

func TestNewProvider_UnknownType(t *testing.T) {
	_, err := NewProvider(&ProviderConfig{Type: "vault"})
	if err == nil {
		t.Fatal("expected error for unknown provider type")
	}
}

func TestValidateDeclaredRefs_AllPresent(t *testing.T) {
	path := writeDotenv(t, "A=1\nB=2\n")
	p, _ := NewProvider(&ProviderConfig{Type: "dotenv", Path: path})
	refs := []SecretRef{
		{EnvName: "A", SecretName: "s", SecretKey: "a"},
		{EnvName: "B", SecretName: "s", SecretKey: "b"},
	}
	if err := ValidateDeclaredRefs(p, refs, path); err != nil {
		t.Errorf("all-present should be nil, got: %v", err)
	}
}

func TestValidateDeclaredRefs_SomeMissing_ListsAll(t *testing.T) {
	path := writeDotenv(t, "PRESENT=1\n")
	p, _ := NewProvider(&ProviderConfig{Type: "dotenv", Path: path})
	refs := []SecretRef{
		{EnvName: "PRESENT", SecretName: "ok", SecretKey: "k"},
		{EnvName: "GITHUB_CLIENT_ID", SecretName: "github-oauth", SecretKey: "client-id"},
		{EnvName: "SUPABASE_JWT_ISSUER", SecretName: "supabase", SecretKey: "jwt-issuer"},
	}
	err := ValidateDeclaredRefs(p, refs, ".env.dev.secrets")
	if err == nil {
		t.Fatal("expected error for missing refs, got nil")
	}
	msg := err.Error()
	// Must list ALL misses, not just the first.
	for _, want := range []string{
		"missing 2 declared value(s)",
		"GITHUB_CLIENT_ID",
		"github-oauth/client-id",
		"SUPABASE_JWT_ISSUER",
		"supabase/jwt-issuer",
		".env.dev.secrets",
		"fix:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
	// PRESENT must NOT be listed as missing.
	if strings.Contains(msg, "PRESENT") {
		t.Errorf("present ref wrongly listed as missing:\n%s", msg)
	}
}

func TestValidateDeclaredRefs_DefaultsKeyToEnvName(t *testing.T) {
	path := writeDotenv(t, "")
	p, _ := NewProvider(&ProviderConfig{Type: "dotenv", Path: path})
	refs := []SecretRef{{EnvName: "TOKEN", SecretName: "creds"}} // SecretKey empty
	err := ValidateDeclaredRefs(p, refs, path)
	if err == nil || !strings.Contains(err.Error(), "creds/TOKEN") {
		t.Errorf("expected key to default to EnvName (creds/TOKEN), got: %v", err)
	}
}

func TestValidateDeclaredRefs_External_Nil(t *testing.T) {
	p, _ := NewProvider(&ProviderConfig{Type: "external"})
	refs := []SecretRef{{EnvName: "X", SecretName: "s", SecretKey: "k"}}
	if err := ValidateDeclaredRefs(p, refs, ""); err != nil {
		t.Errorf("external validate should be nil (forge can't see values), got: %v", err)
	}
}

func TestValidateDeclaredRefs_Noop_Nil(t *testing.T) {
	p, _ := NewProvider(nil)
	refs := []SecretRef{{EnvName: "X", SecretName: "s"}}
	if err := ValidateDeclaredRefs(p, refs, ""); err != nil {
		t.Errorf("noop validate should be nil, got: %v", err)
	}
}

func TestRenderK8sSecrets_GroupsByName_MultiKey_Deterministic(t *testing.T) {
	path := writeDotenv(t, "CLIENT_ID=id\nCLIENT_SECRET=sec\nJWT=jwt\n")
	p, _ := NewProvider(&ProviderConfig{Type: "dotenv", Path: path})
	refs := []SecretRef{
		// Two refs into the same Secret "github-oauth".
		{EnvName: "CLIENT_SECRET", SecretName: "github-oauth", SecretKey: "client-secret"},
		{EnvName: "CLIENT_ID", SecretName: "github-oauth", SecretKey: "client-id"},
		// A separate Secret "supabase".
		{EnvName: "JWT", SecretName: "supabase", SecretKey: "jwt-issuer"},
		// Unresolved ref is skipped (not in dotenv).
		{EnvName: "MISSING", SecretName: "ignored", SecretKey: "x"},
	}
	manifests := RenderK8sSecrets(p, refs, "dev")
	if len(manifests) != 2 {
		t.Fatalf("got %d manifests, want 2 (one per Secret name)", len(manifests))
	}
	// Deterministic: sorted by Secret name -> github-oauth before supabase.
	if got := manifests[0]["metadata"].(map[string]any)["name"]; got != "github-oauth" {
		t.Errorf("first secret name: got %v, want github-oauth", got)
	}
	if got := manifests[1]["metadata"].(map[string]any)["name"]; got != "supabase" {
		t.Errorf("second secret name: got %v, want supabase", got)
	}
	// Shape checks on the multi-key Secret.
	gh := manifests[0]
	if gh["apiVersion"] != "v1" || gh["kind"] != "Secret" || gh["type"] != "Opaque" {
		t.Errorf("github-oauth manifest header wrong: %+v", gh)
	}
	if gh["metadata"].(map[string]any)["namespace"] != "dev" {
		t.Errorf("namespace: got %v, want dev", gh["metadata"].(map[string]any)["namespace"])
	}
	sd := gh["stringData"].(map[string]any)
	if sd["client-id"] != "id" || sd["client-secret"] != "sec" {
		t.Errorf("github-oauth stringData wrong: %+v", sd)
	}
	if len(sd) != 2 {
		t.Errorf("github-oauth should have 2 keys, got %d", len(sd))
	}
}

func TestRenderK8sSecrets_External_Nil(t *testing.T) {
	p, _ := NewProvider(&ProviderConfig{Type: "external"})
	refs := []SecretRef{{EnvName: "X", SecretName: "s", SecretKey: "k"}}
	if got := RenderK8sSecrets(p, refs, "prod"); got != nil {
		t.Errorf("external RenderK8sSecrets should be nil, got: %v", got)
	}
}

func TestRenderK8sSecrets_Noop_Nil(t *testing.T) {
	p, _ := NewProvider(nil)
	if got := RenderK8sSecrets(p, []SecretRef{{EnvName: "X", SecretName: "s"}}, "x"); got != nil {
		t.Errorf("noop RenderK8sSecrets should be nil, got: %v", got)
	}
}

func TestRenderK8sSecrets_NoResolvable_Nil(t *testing.T) {
	// dotenv provider but no ref resolves -> nil (nothing to render).
	path := writeDotenv(t, "OTHER=1\n")
	p, _ := NewProvider(&ProviderConfig{Type: "dotenv", Path: path})
	refs := []SecretRef{{EnvName: "MISSING", SecretName: "s", SecretKey: "k"}}
	if got := RenderK8sSecrets(p, refs, "dev"); got != nil {
		t.Errorf("no-resolvable RenderK8sSecrets should be nil, got: %v", got)
	}
}

// dotenvFor builds a dotenv provider over a temp .env file with body.
func dotenvFor(t *testing.T, body string) Provider {
	t.Helper()
	p, err := NewProvider(&ProviderConfig{Type: "dotenv", Path: writeDotenv(t, body)})
	if err != nil {
		t.Fatalf("NewProvider dotenv: %v", err)
	}
	return p
}

// secretStringData pulls the stringData map off a rendered Secret manifest.
func secretStringData(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	sd, ok := m["stringData"].(map[string]any)
	if !ok {
		t.Fatalf("manifest has no stringData: %v", m)
	}
	return sd
}

func TestRenderDeclaredSecrets_Dotenv(t *testing.T) {
	dot := dotenvFor(t, "DB_PASSWORD=hunter2\nDB_USER=admin\n")
	declared := []DeclaredSecret{{
		Name: "db-credentials",
		Keys: map[string]DeclaredSecretKey{
			"password": {From: "dotenv", Key: "DB_PASSWORD"},
			"username": {From: "dotenv", Key: "DB_USER"},
		},
	}}
	mans, err := RenderDeclaredSecrets(declared, dot, "dev", "myns")
	if err != nil {
		t.Fatalf("RenderDeclaredSecrets: %v", err)
	}
	if len(mans) != 1 {
		t.Fatalf("got %d manifests want 1", len(mans))
	}
	sd := secretStringData(t, mans[0])
	if sd["password"] != "hunter2" || sd["username"] != "admin" {
		t.Errorf("stringData wrong: %v", sd)
	}
	if md := mans[0]["metadata"].(map[string]any); md["namespace"] != "myns" || md["name"] != "db-credentials" {
		t.Errorf("metadata wrong: %v", md)
	}
}

func TestRenderDeclaredSecrets_MissingDotenvKey(t *testing.T) {
	dot := dotenvFor(t, "DB_USER=admin\n")
	declared := []DeclaredSecret{{
		Name: "db-credentials",
		Keys: map[string]DeclaredSecretKey{"password": {From: "dotenv", Key: "DB_PASSWORD"}},
	}}
	_, err := RenderDeclaredSecrets(declared, dot, "dev", "myns")
	if err == nil {
		t.Fatal("expected error for missing dotenv key")
	}
	if !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Errorf("error should name the missing key: %v", err)
	}
}

func TestRenderDeclaredSecrets_LiteralAllowedDevE2E(t *testing.T) {
	declared := []DeclaredSecret{{
		Name: "fixtures",
		Keys: map[string]DeclaredSecretKey{"ISSUER": {From: "literal", Value: "https://test.local/"}},
	}}
	for _, env := range []string{"dev", "e2e"} {
		mans, err := RenderDeclaredSecrets(declared, noopProvider{}, env, "myns")
		if err != nil {
			t.Fatalf("env=%s: literal should be allowed: %v", env, err)
		}
		if secretStringData(t, mans[0])["ISSUER"] != "https://test.local/" {
			t.Errorf("env=%s: literal value not rendered", env)
		}
	}
}

func TestRenderDeclaredSecrets_LiteralRejectedNonDev(t *testing.T) {
	declared := []DeclaredSecret{{
		Name: "leak",
		Keys: map[string]DeclaredSecretKey{"API_KEY": {From: "literal", Value: "sk-prod"}},
	}}
	for _, env := range []string{"prod", "staging", "preprod", ""} {
		_, err := RenderDeclaredSecrets(declared, noopProvider{}, env, "myns")
		if err == nil {
			t.Errorf("env=%q: literal must be rejected outside dev/e2e", env)
			continue
		}
		if !strings.Contains(err.Error(), "literal") {
			t.Errorf("env=%q: error should mention literal: %v", env, err)
		}
	}
}

func TestRenderDeclaredSecrets_Empty(t *testing.T) {
	mans, err := RenderDeclaredSecrets(nil, noopProvider{}, "dev", "myns")
	if err != nil || mans != nil {
		t.Errorf("empty declared => (nil, nil), got (%v, %v)", mans, err)
	}
}
