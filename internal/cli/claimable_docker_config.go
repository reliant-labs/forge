package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// credentialConfigKeys are the docker config.json keys that carry or point at
// credentials; the throwaway config drops them.
var credentialConfigKeys = []string{"auths", "credsStore", "credHelpers"}

// isolateDockerConfig points DOCKER_CONFIG at a throwaway directory for an
// anonymous deploy, so the registry login it makes does not write a credential
// into the user's ~/.docker. Contexts, CLI plugins and buildx state stay
// reachable (linked in), so docker still finds its daemon and its plugins.
//
// keepCredentials says whether the user's own registry credentials are copied
// along. A site-only deploy builds no image, so it takes none: the throwaway
// holds only what the login writes. A deploy that builds a Dockerfile may need
// them to pull a private base image, so there they are kept, and a configured
// credential helper still receives the login as it always has.
//
// Best effort: on any failure the real config is left in use and the returned
// restore is a no-op.
func isolateDockerConfig(keepCredentials bool) (restore func()) {
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
	if err := populateIsolatedDockerConfig(real, tmp, keepCredentials); err != nil {
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
// to every other entry. A link that cannot be made is skipped: the entry is an
// optional convenience, and an isolated config is still the goal.
func populateIsolatedDockerConfig(src, dst string, keepCredentials bool) error {
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
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dst, "config.json"), raw, 0o600); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.Name() == "config.json" {
			continue
		}
		_ = os.Symlink(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
	}
	return nil
}
