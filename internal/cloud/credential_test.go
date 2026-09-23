package cloud

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveCredential_PrecedenceFlagBeatsEnvBeatsFile pins the
// documented order: flag > env var > login file.
//
// All three credentials are present SIMULTANEOUSLY in each case, which
// is the point — a test that supplies only one proves nothing about
// precedence, only that each source works in isolation.
func TestResolveCredential_PrecedenceFlagBeatsEnvBeatsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FORGE_HOME", home)
	if err := WriteLogin(filepath.Join(home, "login.json"), StoredLogin{Token: "from-file"}); err != nil {
		t.Fatalf("seed login file: %v", err)
	}
	const tokenEnv = "ACME_DEPLOY_TOKEN"
	t.Setenv(tokenEnv, "from-env")

	// 1. Flag present alongside env var AND file -> flag wins.
	got, err := ResolveCredential("from-flag", tokenEnv)
	if err != nil {
		t.Fatalf("with flag: unexpected error: %v", err)
	}
	if got.Token != "from-flag" {
		t.Errorf("flag must beat env var and login file: got token %q, want %q", got.Token, "from-flag")
	}
	if got.Source != SourceFlag {
		t.Errorf("source: got %q, want %q", got.Source, SourceFlag)
	}

	// 2. No flag, env var present alongside file -> env var wins.
	got, err = ResolveCredential("", tokenEnv)
	if err != nil {
		t.Fatalf("without flag: unexpected error: %v", err)
	}
	if got.Token != "from-env" {
		t.Errorf("env var must beat the login file: got token %q, want %q", got.Token, "from-env")
	}
	if got.Source != SourceEnv {
		t.Errorf("source: got %q, want %q", got.Source, SourceEnv)
	}
	if got.From != tokenEnv {
		t.Errorf("From should name the env var so the user can see which one was read: got %q", got.From)
	}

	// 3. Neither flag nor env var -> the login file, the human default.
	t.Setenv(tokenEnv, "")
	got, err = ResolveCredential("", tokenEnv)
	if err != nil {
		t.Fatalf("file only: unexpected error: %v", err)
	}
	if got.Token != "from-file" {
		t.Errorf("login file is the fallback: got token %q, want %q", got.Token, "from-file")
	}
	if got.Source != SourceLogin {
		t.Errorf("source: got %q, want %q", got.Source, SourceLogin)
	}
}

// TestResolveCredential_HonorsDeclaredTokenEnv proves the variable read
// is the one the ENVIRONMENT declared, not a hardcoded name. Two envs
// declaring two variables is the reason token_env exists.
func TestResolveCredential_HonorsDeclaredTokenEnv(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv("STAGING_TOKEN", "staging-cred")
	t.Setenv("PROD_TOKEN", "prod-cred")

	staging, err := ResolveCredential("", "STAGING_TOKEN")
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	prod, err := ResolveCredential("", "PROD_TOKEN")
	if err != nil {
		t.Fatalf("prod: %v", err)
	}
	if staging.Token == prod.Token {
		t.Fatalf("two envs declaring different token_env must read different variables; both got %q", staging.Token)
	}
	if staging.Token != "staging-cred" || prod.Token != "prod-cred" {
		t.Errorf("wrong variables read: staging=%q prod=%q", staging.Token, prod.Token)
	}
}

// TestResolveCredential_MissingNamesLoginAndEnvVar is the message
// contract: a user with no credential must be told the two ways to get
// one, not handed a bare failure.
func TestResolveCredential_MissingNamesLoginAndEnvVar(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir()) // empty: no login file
	const tokenEnv = "ACME_DEPLOY_TOKEN"
	t.Setenv(tokenEnv, "")

	_, err := ResolveCredential("", tokenEnv)
	if err == nil {
		t.Fatal("expected an error when no credential can be resolved")
	}
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("error should satisfy errors.Is(err, ErrNoCredential); got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "forge login") {
		t.Errorf("message must name `forge login`; got:\n%s", msg)
	}
	if !strings.Contains(msg, tokenEnv) {
		t.Errorf("message must name the DECLARED env var %q, not a hardcoded default; got:\n%s", tokenEnv, msg)
	}
}

// TestResolveCredential_CorruptLoginFileIsAnError: a user who DID log in
// must not be told they did not. Falling through to "no credential"
// would send them round a loop that cannot fix the real problem.
func TestResolveCredential_CorruptLoginFileIsAnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FORGE_HOME", home)
	t.Setenv("ACME_DEPLOY_TOKEN", "")
	if err := os.WriteFile(filepath.Join(home, "login.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveCredential("", "ACME_DEPLOY_TOKEN")
	if err == nil {
		t.Fatal("a corrupt login file must be an error, not a silent 'not logged in'")
	}
	if errors.Is(err, ErrNoCredential) {
		t.Errorf("a corrupt file is NOT the same as no credential; got %v", err)
	}
}

// TestWriteLogin_IsNotWorldReadable — the file is a bearer credential.
func TestWriteLogin_IsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "login.json")
	if err := WriteLogin(path, StoredLogin{Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("login file mode %o grants group/other access to a bearer credential", mode)
	}
}

// TestResolveCredential_DefaultsToConventionalEnvVar: a caller with no
// declaration in hand still resolves FORGE_CONTROL_PLANE_TOKEN.
func TestResolveCredential_DefaultsToConventionalEnvVar(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv(DefaultTokenEnv, "conventional")

	got, err := ResolveCredential("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Token != "conventional" {
		t.Errorf("empty tokenEnv should fall back to %s; got %q", DefaultTokenEnv, got.Token)
	}
}
