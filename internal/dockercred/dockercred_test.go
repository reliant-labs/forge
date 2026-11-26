package dockercred

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
)

// writeConfig writes a docker config.json whose credsStore names a helper
// that does not exist, so any read that reaches the native store fails loudly
// instead of consulting this machine's keychain.
func writeConfig(t *testing.T, credHelpers string) string {
	t.Helper()
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("forge:rlat_from_login"))
	cfg := `{
  "credsStore": "forge-test-no-such-helper",
  "credHelpers": ` + credHelpers + `,
  "auths": {"registry.example.com": {"auth": "` + auth + `"}}
}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An empty credHelpers entry pins the host to config.json, as docker reads
// it: the credential `docker login` wrote there is the one returned, and the
// credsStore is never consulted. This is the anonymous deploy's config, and
// reading the credsStore instead is what made every oras push 401.
func TestEmptyCredHelperReadsTheConfigFile(t *testing.T) {
	cred, err := mustCredential(t, writeConfig(t, `{"registry.example.com": ""}`))(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatalf("Get: %v (the credsStore was consulted for a host pinned to the file)", err)
	}
	if cred.Username != "forge" || cred.Password != "rlat_from_login" {
		t.Fatalf("got %+v, want the credential docker login wrote to config.json", cred)
	}
}

// A host with no credHelpers entry still goes to credsStore, exactly as
// docker would send it: the pin is per host, not a switch to the file.
func TestUnpinnedHostKeepsTheCredsStore(t *testing.T) {
	if _, err := mustCredential(t, writeConfig(t, `{"registry.example.com": ""}`))(context.Background(), "other.example.com"); err == nil {
		t.Fatal("Get of an unpinned host succeeded without its credsStore helper; it must be asked")
	}
}

// With no empty entry every host keeps oras's reading, credsStore included.
func TestNoPinKeepsTheCredsStore(t *testing.T) {
	if _, err := mustCredential(t, writeConfig(t, `{"gcr.io": "gcloud"}`))(context.Background(), "registry.example.com"); err == nil {
		t.Fatal("a host with no credHelpers entry was read from the file; its credsStore must be asked")
	}
}

// Credential follows $DOCKER_CONFIG, which is how an anonymous deploy
// points both docker and oras at its throwaway config.
func TestNewStoreFromDockerFollowsDockerConfig(t *testing.T) {
	path := writeConfig(t, `{"registry.example.com": ""}`)
	t.Setenv("DOCKER_CONFIG", filepath.Dir(path))
	credential, err := Credential()
	if err != nil {
		t.Fatal(err)
	}
	cred, err := credential(context.Background(), "registry.example.com")
	if err != nil || cred.Password != "rlat_from_login" {
		t.Fatalf("got %+v, %v; want the throwaway config's credential", cred, err)
	}
}

func TestMissingConfigIsEmpty(t *testing.T) {
	if _, err := mustCredential(t, filepath.Join(t.TempDir(), "config.json"))(context.Background(), "registry.example.com"); err != nil {
		t.Fatalf("Get against a missing config: %v", err)
	}
}

func mustCredential(t *testing.T, configPath string) auth.CredentialFunc {
	t.Helper()
	credential, err := CredentialFrom(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

// Login writes where docker login would, so Credential reads it back: a
// config with no credential store gets config.json's `auths`.
func TestLoginIsReadBackByCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := LoginTo(context.Background(), path, "registry.example.com", "forge", "rlat_in_process"); err != nil {
		t.Fatalf("LoginTo: %v", err)
	}
	cred, err := mustCredential(t, path)(context.Background(), "registry.example.com")
	if err != nil || cred.Username != "forge" || cred.Password != "rlat_in_process" {
		t.Fatalf("got %+v, %v; want the stored login", cred, err)
	}
}

// A missing configured helper may not turn a helper-backed login into a
// write to config.json, even though file storage is allowed for file-backed
// configurations. The existing file must be unchanged on failure.
func TestLoginDoesNotFallBackWhenConfiguredHelperIsMissing(t *testing.T) {
	const host = "registry.example.com"
	for _, tc := range []struct{ name, config string }{
		{"global store", `{"credsStore":"forge-test-missing"}`},
		{"registry helper", `{"credHelpers":{"registry.example.com":"forge-test-missing"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			// Use an empty PATH so a machine-local helper cannot affect the test.
			t.Setenv("PATH", t.TempDir())
			if err := LoginTo(context.Background(), path, host, "forge", "dummy-test-value"); err == nil {
				t.Fatal("login succeeded without the configured helper")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != tc.config {
				t.Fatalf("failed helper login changed config.json (read error: %v)", err)
			}
		})
	}
}

// A host pinned to config.json by an empty credHelpers entry (the anonymous
// deploy's throwaway config) is written to the file, never to the
// credsStore, which is what Credential then reads.
func TestLoginHonoursAFilePin(t *testing.T) {
	path := writeConfig(t, `{"registry.example.com": ""}`)
	if err := LoginTo(context.Background(), path, "registry.example.com", "forge", "rlat_pinned"); err != nil {
		t.Fatalf("LoginTo a pinned host reached the credsStore: %v", err)
	}
	cred, err := mustCredential(t, path)(context.Background(), "registry.example.com")
	if err != nil || cred.Password != "rlat_pinned" {
		t.Fatalf("got %+v, %v", cred, err)
	}
}

// Login, unlike LoginTo, writes to the ambient docker config, which an
// anonymous deploy points at its throwaway directory with $DOCKER_CONFIG.
func TestLoginWritesTheAmbientConfigAndCredentialReadsItBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	if err := Login(context.Background(), "registry.example.com", "forge", "rlat_ambient"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("Login did not write $DOCKER_CONFIG/config.json: %v", err)
	}
	credential, err := Credential()
	if err != nil {
		t.Fatal(err)
	}
	cred, err := credential(context.Background(), "registry.example.com")
	if err != nil || cred.Username != "forge" || cred.Password != "rlat_ambient" {
		t.Fatalf("got %+v, %v; want the credential Login stored", cred, err)
	}
}

// docker files Docker Hub's credential under its legacy index URL, so a login
// for "docker.io" must land there for docker itself to find it.
func TestLoginForDockerHubUsesTheIndexKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := LoginTo(context.Background(), path, "docker.io", "me", "hub-secret"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Auths["https://index.docker.io/v1/"]; !ok || len(cfg.Auths) != 1 {
		t.Fatalf("auths keys = %v, want only https://index.docker.io/v1/", cfg.Auths)
	}
	// oras asks the hook for registry-1.docker.io, which it files under the same key.
	cred, err := mustCredential(t, path)(context.Background(), "registry-1.docker.io")
	if err != nil || cred.Password != "hub-secret" {
		t.Fatalf("got %+v, %v; want the stored Hub login read back for registry-1.docker.io", cred, err)
	}
}

func TestServerAddress(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io":            "https://index.docker.io/v1/",
		"ghcr.io":              "ghcr.io",
		"localhost:5000":       "localhost:5000",
		"index.docker.io":      "index.docker.io",
		"registry.example.com": "registry.example.com",
	} {
		if got := serverAddress(in); got != want {
			t.Errorf("serverAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// A config.json that is not JSON cannot say which hosts are pinned, so every
// entry point reports it with the file's path rather than guessing.
func TestUnreadableConfigIsReportedByEveryEntryPoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"credHelpers": not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)

	for name, call := range map[string]func() error{
		"CredentialFrom": func() error { _, err := CredentialFrom(path); return err },
		"Credential":     func() error { _, err := Credential(); return err },
		"LoginTo": func() error {
			return LoginTo(context.Background(), path, "registry.example.com", "u", "p")
		},
		"Login": func() error { return Login(context.Background(), "registry.example.com", "u", "p") },
	} {
		err := call()
		if err == nil {
			t.Errorf("%s accepted a config that is not JSON", name)
			continue
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%s error %q does not name %s", name, err, path)
		}
	}
	if raw, _ := os.ReadFile(path); string(raw) != `{"credHelpers": not-json` {
		t.Errorf("a failed login rewrote the broken config: %q", raw)
	}
}

// A config directory that cannot be read as a file (config.json is a
// directory) is an error, not an empty config.
func TestConfigPathThatIsADirectoryIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CredentialFrom(path); err == nil {
		t.Fatal("CredentialFrom accepted a config.json that is a directory")
	}
}
