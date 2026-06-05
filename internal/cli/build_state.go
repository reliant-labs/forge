package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// buildStateDir is the per-project on-disk location for forge runtime
// state that needs to survive across `forge build` / `forge deploy`
// invocations. Sits under .forge/ alongside checksums.json so a single
// .gitignore rule (`.forge/`) covers both.
const buildStateDir = ".forge/state"

// BuildState records what `forge build --push` actually pushed to a
// registry, so a subsequent `forge deploy <env>` can reference the
// same tag even when the working tree has changed between phases.
//
// The original bug this struct closes: `forge build` tags an image
// `<reg>/<svc>:<git-describe>` (which includes `-dirty` when the
// working tree has untracked or modified files), then `forge deploy`
// independently computes a tag — and the two diverge whenever a
// working-tree mutation between the two phases flips the dirty bit,
// or when the two phases use different git commands altogether
// (build uses `git describe --tags --always --dirty`, deploy used
// `git rev-parse --short HEAD`). The state file fixes both by making
// build authoritative.
//
// Wire format is JSON; fields use snake_case for readability when a
// user peeks at the file by hand. PushedAt is RFC3339 so a human can
// eyeball "how stale is this?" without a parser.
type BuildState struct {
	Image    string `json:"image"`
	Tag      string `json:"tag"`
	Registry string `json:"registry"`
	// PushedAt is the wall-clock time the push completed, formatted as
	// time.RFC3339. The state file is informational across forge
	// invocations, so we use real time here — reproducibility
	// constraints don't apply.
	PushedAt string `json:"pushed_at"`
}

// buildStatePath returns the absolute path to the per-env build-state
// file. One file per environment so `forge build --push --env=dev`
// and `forge build --push --env=staging` don't clobber each other,
// and so `forge deploy <env>` can read the right one without a
// separate lookup. When env is empty we use the literal "default"
// segment to keep the path stable.
func buildStatePath(projectDir, env string) string {
	if env == "" {
		env = "default"
	}
	return filepath.Join(projectDir, buildStateDir, "build-"+env+".json")
}

// WriteBuildState persists a successful `forge build --push` to disk.
// Called by runBuild after every per-image push succeeds, so the most
// recent push is always the source of truth a subsequent
// `forge deploy <env>` consumes.
//
// The directory is created lazily — projects that never use --push
// never grow a .forge/state/ tree. File is 0o644 (world-readable) to
// match checksums.json's mode; nothing in here is secret.
func WriteBuildState(projectDir, env string, state BuildState) error {
	path := buildStatePath(projectDir, env)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create build-state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal build state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write build state: %w", err)
	}
	return nil
}

// ReadBuildState loads the per-env build-state file. Returns
// (nil, nil) when the file is missing — that's the
// "deploy-without-build" path (CI with a separate build job, or the
// user running `forge deploy` on a fresh checkout) and the caller
// falls through to resolveImageTag. Returns (nil, err) for malformed
// JSON or unreadable files; callers should not silently swallow these
// because they mean the state file exists but can't be trusted.
func ReadBuildState(projectDir, env string) (*BuildState, error) {
	path := buildStatePath(projectDir, env)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read build state %s: %w", path, err)
	}
	var st BuildState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse build state %s: %w", path, err)
	}
	return &st, nil
}

// nowRFC3339 returns the current wall-clock time formatted as
// time.RFC3339. Wrapped so tests can verify the timestamp shape
// without re-deriving the format string.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
