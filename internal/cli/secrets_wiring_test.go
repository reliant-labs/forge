package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// sampleSecretProviderJSON is an entity render carrying a dotenv
// secret_provider declaration plus a mix of cluster/host services with
// declared secret_refs. Exercises the parse + wiring helpers.
const sampleSecretProviderJSON = `{
  "services": [
    {
      "name": "api",
      "deploy": {"type": "cluster", "cluster": "c", "namespace": "dev", "registry": "r"},
      "env_vars": [
        {"name": "GITHUB_CLIENT_ID", "secret_ref": "github-oauth", "secret_key": "client-id"},
        {"name": "TOKEN", "secret_ref": "creds"},
        {"name": "LOG_LEVEL", "value": "debug"}
      ]
    },
    {
      "name": "worker",
      "deploy": {"type": "host", "runner": "go-run"},
      "env_vars": [
        {"name": "STRIPE_KEY", "secret_ref": "stripe"}
      ]
    }
  ],
  "secret_provider": {"type": "dotenv", "path": ".env.dev.secrets"}
}`

func TestParseKCLEntities_SecretProvider(t *testing.T) {
	e, err := parseKCLEntities([]byte(sampleSecretProviderJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if e.SecretProvider == nil {
		t.Fatal("SecretProvider is nil")
	}
	if e.SecretProvider.Type != "dotenv" {
		t.Errorf("type: got %q, want dotenv", e.SecretProvider.Type)
	}
	if e.SecretProvider.Path != ".env.dev.secrets" {
		t.Errorf("path: got %q, want .env.dev.secrets", e.SecretProvider.Path)
	}
}

func TestParseKCLEntities_SecretProvider_AbsentNil(t *testing.T) {
	// The existing sample (no secret_provider key) must parse to nil.
	e, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if e.SecretProvider != nil {
		t.Errorf("SecretProvider should be nil when absent, got %+v", e.SecretProvider)
	}
}

func TestSecretRefsFromEntities(t *testing.T) {
	e, err := parseKCLEntities([]byte(sampleSecretProviderJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	refs := secretRefsFromEntities(e)
	// api: GITHUB_CLIENT_ID + TOKEN; worker: STRIPE_KEY. LOG_LEVEL has no
	// secret_ref so it's excluded.
	if len(refs) != 3 {
		t.Fatalf("got %d refs, want 3: %+v", len(refs), refs)
	}
	byName := map[string]struct{ secret, key string }{}
	for _, r := range refs {
		byName[r.EnvName] = struct{ secret, key string }{r.SecretName, r.SecretKey}
	}
	if got := byName["GITHUB_CLIENT_ID"]; got.secret != "github-oauth" || got.key != "client-id" {
		t.Errorf("GITHUB_CLIENT_ID ref wrong: %+v", got)
	}
	// SecretKey falls back to EnvName when KCL secret_key is empty.
	if got := byName["TOKEN"]; got.secret != "creds" || got.key != "TOKEN" {
		t.Errorf("TOKEN ref should default key to EnvName: %+v", got)
	}
	if _, ok := byName["LOG_LEVEL"]; ok {
		t.Error("LOG_LEVEL (no secret_ref) wrongly included")
	}
}

func TestSecretRefsForK8sServices(t *testing.T) {
	e, err := parseKCLEntities([]byte(sampleSecretProviderJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	refs := secretRefsForK8sServices(e)
	// Only the cluster service "api" contributes (2 refs). The host
	// service "worker" (STRIPE_KEY) is excluded.
	if len(refs) != 2 {
		t.Fatalf("got %d k8s refs, want 2: %+v", len(refs), refs)
	}
	for _, r := range refs {
		if r.EnvName == "STRIPE_KEY" {
			t.Error("host-service ref STRIPE_KEY wrongly included in k8s refs")
		}
	}
}

func TestSecretProviderFromEntities_DotenvPathResolved(t *testing.T) {
	dir := t.TempDir()
	// Write the dotenv so the provider loads it; assert the value resolves
	// (proving the relative path was joined against projectDir).
	if err := os.WriteFile(filepath.Join(dir, ".env.dev.secrets"),
		[]byte("GITHUB_CLIENT_ID=abc\nTOKEN=tok\nSTRIPE_KEY=sk\n"), 0o600); err != nil {
		t.Fatalf("write dotenv: %v", err)
	}
	e, err := parseKCLEntities([]byte(sampleSecretProviderJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	p, err := secretProviderFromEntities(e, dir)
	if err != nil {
		t.Fatalf("secretProviderFromEntities: %v", err)
	}
	if p.Kind() != "dotenv" {
		t.Fatalf("Kind: got %q, want dotenv", p.Kind())
	}
	if v, ok := p.Resolve("GITHUB_CLIENT_ID"); !ok || v != "abc" {
		t.Errorf("Resolve through resolved path: got (%q,%v), want (abc,true)", v, ok)
	}
}

func TestSecretProviderFromEntities_NoneNoop(t *testing.T) {
	e, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	p, err := secretProviderFromEntities(e, t.TempDir())
	if err != nil {
		t.Fatalf("secretProviderFromEntities: %v", err)
	}
	if p.Kind() != "none" {
		t.Errorf("Kind: got %q, want none", p.Kind())
	}
}
