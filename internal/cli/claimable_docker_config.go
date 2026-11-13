package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// credentialConfigKeys are the docker config.json keys that carry or point at
// credentials; the throwaway config drops them.
var credentialConfigKeys = []string{"auths", "credsStore", "credHelpers"}

// linkedDockerConfigEntries are the only entries of the user's docker config
// directory the throwaway one links: what docker needs to reach its daemon
// (contexts, with their TLS material) and to find its plugins and builders
// (cli-plugins, buildx). Everything else is left out, because an entry forge
// does not know may be a credential helper's or Docker Desktop's own state,
// and linking it would let the anonymous login read or rewrite it.
var linkedDockerConfigEntries = []string{"contexts", "cli-plugins", "buildx"}

// isolateDockerConfig points DOCKER_CONFIG at a throwaway directory for an
// anonymous deploy, so the registry login it makes does not write a credential
// into the user's ~/.docker. Contexts, CLI plugins and buildx state stay
// reachable (linked in), so docker still finds its daemon and its plugins.
//
// keepCredentials says whether the user's own registry credentials are copied
// along. A site-only deploy builds no image, so it takes none: the throwaway
// holds only what the login writes. A deploy that builds a Dockerfile may need
// them to pull a private base image, so there they are kept. Even then the
// login to loginHost (the platform registry) is pinned to the throwaway file:
// an empty credHelpers entry for a host makes docker use the config file for it
// rather than the credsStore, so the anonymous credential never reaches the
// user's keychain or credential helper.
//
// Best effort: on any failure the real config is left in use and the returned
// restore is a no-op.
func isolateDockerConfig(keepCredentials bool, loginHost string) (restore func()) {
	noop := func() {}
	real := os.Getenv("DOCKER_CONFIG")
	if real == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return noop
		}
		real = filepath.Join(home, ".docker")
	}
	tmp, err := os.MkdirTemp("", "forge-docker-config-")
	if err != nil {
		return noop
	}
	if err := populateIsolatedDockerConfig(real, tmp, keepCredentials, loginHost); err != nil {
		_ = os.RemoveAll(tmp)
		return noop
	}
	prev, had := os.LookupEnv("DOCKER_CONFIG")
	if err := os.Setenv("DOCKER_CONFIG", tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return noop
	}
	return func() {
		if had {
			_ = os.Setenv("DOCKER_CONFIG", prev)
		} else {
			_ = os.Unsetenv("DOCKER_CONFIG")
		}
		_ = os.RemoveAll(tmp)
	}
}

// populateIsolatedDockerConfig fills dst from the docker config directory src:
// config.json (without its credential keys unless keepCredentials), and a link
// to each of linkedDockerConfigEntries that exists. No other entry is linked. A
// link that cannot be made is skipped: the entry is an optional convenience,
// and an isolated config is still the goal.
//
// When credentials are kept, loginHost (if set) gets an empty credHelpers
// entry, so its login is stored in dst's config.json and nowhere else.
func populateIsolatedDockerConfig(src, dst string, keepCredentials bool, loginHost string) error {
	cfg := map[string]any{}
	if raw, err := os.ReadFile(filepath.Join(src, "config.json")); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if !keepCredentials {
		for _, k := range credentialConfigKeys {
			delete(cfg, k)
		}
	} else if host, _, _ := strings.Cut(loginHost, "/"); host != "" {
		helpers, _ := cfg["credHelpers"].(map[string]any)
		if helpers == nil {
			helpers = map[string]any{}
		}
		helpers[host] = ""
		cfg["credHelpers"] = helpers
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dst, "config.json"), raw, 0o600); err != nil {
		return err
	}
	for _, name := range linkedDockerConfigEntries {
		if _, err := os.Lstat(filepath.Join(src, name)); err != nil {
			continue
		}
		_ = os.Symlink(filepath.Join(src, name), filepath.Join(dst, name))
	}
	return nil
}
