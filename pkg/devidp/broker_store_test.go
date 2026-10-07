package devidp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// stubBrokerIssuer answers the calls ProvisionLoginBroker makes: the broker
// account already exists and holds its role, /auth/v1/users/me answers per
// `me` (keyed by the presented bearer token), and every token mint is
// counted.
func stubBrokerIssuer(t *testing.T, me func(bearer string) (int, string)) (*Client, *int) {
	t.Helper()
	mints := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/users":
			_, _ = w.Write([]byte(`{"result":[{"userId":"broker-1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/_search"):
			_, _ = w.Write([]byte(`{"result":[{"userId":"broker-1","roles":["IAM_LOGIN_CLIENT"]}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/auth/v1/users/me":
			status, body := me(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodPost && r.URL.Path == "/management/v1/users/broker-1/pats":
			mints++
			_, _ = w.Write([]byte(`{"token":"fresh-token"}`))
		default:
			t.Errorf("unexpected issuer call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return client, &mints
}

// The whole point: a token the issuer still accepts as the broker's is kept.
// Minting on every run is what printed a fresh credential on every
// `forge env up` and rotated it under a running server.
func TestProvisionLoginBroker_KeepsAStoredTokenTheIssuerAccepts(t *testing.T) {
	client, mints := stubBrokerIssuer(t, func(bearer string) (int, string) {
		if bearer == "stored-token" {
			return http.StatusOK, `{"user":{"id":"broker-1","userName":"app-login-broker"}}`
		}
		return http.StatusUnauthorized, `{"code":16}`
	})

	cred, minted, err := client.ProvisionLoginBroker(context.Background(), "app-login-broker", "stored-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if minted || *mints != 0 {
		t.Fatalf("a valid stored token was replaced (minted=%v, mint calls=%d)", minted, *mints)
	}
	if cred.Token != "stored-token" || cred.UserID != "broker-1" {
		t.Fatalf("expected the stored token back, got %#v", cred)
	}
}

func TestProvisionLoginBroker_MintsWhenNothingIsStored(t *testing.T) {
	client, mints := stubBrokerIssuer(t, func(string) (int, string) {
		t.Error("no stored token: nothing to validate, so /auth/v1/users/me must not be called")
		return http.StatusOK, `{}`
	})

	cred, minted, err := client.ProvisionLoginBroker(context.Background(), "app-login-broker", "  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !minted || *mints != 1 || cred.Token != "fresh-token" {
		t.Fatalf("expected exactly one mint, got minted=%v mints=%d cred=%#v", minted, *mints, cred)
	}
}

// A reset IdP (or a revoked token) rejects the stored value; the step must
// heal by minting rather than hand the server a dead credential.
func TestProvisionLoginBroker_MintsWhenTheIssuerRejectsTheStoredToken(t *testing.T) {
	client, mints := stubBrokerIssuer(t, func(string) (int, string) {
		return http.StatusUnauthorized, `{"code":16,"message":"Errors.Token.Invalid"}`
	})

	cred, minted, err := client.ProvisionLoginBroker(context.Background(), "app-login-broker", "dead-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !minted || *mints != 1 || cred.Token != "fresh-token" {
		t.Fatalf("a rejected token must be replaced, got minted=%v mints=%d cred=%#v", minted, *mints, cred)
	}
}

// Valid, but somebody else's: reusing it would give the login path that
// account's authority instead of the broker's role.
func TestProvisionLoginBroker_DoesNotReuseAnotherAccountsToken(t *testing.T) {
	client, mints := stubBrokerIssuer(t, func(string) (int, string) {
		return http.StatusOK, `{"user":{"id":"someone-else"}}`
	})

	_, minted, err := client.ProvisionLoginBroker(context.Background(), "app-login-broker", "other-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !minted || *mints != 1 {
		t.Fatalf("another account's token was kept (minted=%v, mints=%d)", minted, *mints)
	}
}

// An issuer that cannot answer is not evidence the token is dead. Treating
// it as one would mint a new credential every time the IdP is slow.
func TestProvisionLoginBroker_IssuerErrorIsNotAMint(t *testing.T) {
	client, mints := stubBrokerIssuer(t, func(string) (int, string) {
		return http.StatusInternalServerError, `upstream unavailable`
	})

	if _, _, err := client.ProvisionLoginBroker(context.Background(), "app-login-broker", "stored-token"); err == nil {
		t.Fatal("expected an error when the issuer cannot validate the stored token")
	}
	if *mints != 0 {
		t.Fatalf("an unreachable issuer caused %d mint(s)", *mints)
	}
}

// The scaffolded store documents every slot, and holds the developer's own
// values next to this one. Setting the broker token must keep both.
func TestSecretFile_SetKeepsCommentsAndOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "dev.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const scaffolded = `# SECRET VALUES for the ` + "`dev`" + ` environment — GITIGNORED, never commit.

# PostgreSQL connection string.
DATABASE_URL:
# Your own API key.
STRIPE_KEY: sk_test_123
# The login broker's service-account token.
IDP_BROKER_TOKEN:
`
	if err := os.WriteFile(path, []byte(scaffolded), 0o644); err != nil {
		t.Fatal(err)
	}

	store := SecretFile{Path: path}
	if err := store.Set(BrokerTokenKey, "tok-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"GITIGNORED, never commit",
		"# PostgreSQL connection string.",
		"# The login broker's service-account token.",
		"STRIPE_KEY: sk_test_123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Set dropped %q:\n%s", want, got)
		}
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("store no longer parses: %v\n%s", err, got)
	}
	if values[BrokerTokenKey] != "tok-1" || values["STRIPE_KEY"] != "sk_test_123" {
		t.Fatalf("unexpected values after Set: %#v", values)
	}
	if v, err := store.Get(BrokerTokenKey); err != nil || v != "tok-1" {
		t.Fatalf("Get = %q, %v; want tok-1", v, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("a credential store must be 0600, got %v", info.Mode().Perm())
	}
}

func TestSecretFile_SetCreatesAMissingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "dev.yaml")
	store := SecretFile{Path: path}

	if v, err := store.Get(BrokerTokenKey); err != nil || v != "" {
		t.Fatalf("a missing store must read as empty, got %q, %v", v, err)
	}
	if err := store.Set(BrokerTokenKey, "tok-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, err := store.Get(BrokerTokenKey); err != nil || v != "tok-1" {
		t.Fatalf("Get = %q, %v; want tok-1", v, err)
	}
}
