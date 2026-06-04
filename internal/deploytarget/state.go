package deploytarget

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DeployState records the last image+tag a non-cluster provider
// (External, Compose) successfully shipped for one service-env pair.
// It exists because external and compose, unlike `kubectl rollout
// undo`, have no native "go back to the previous revision" affordance —
// the provider has to remember the previous good tag itself if the
// rollback path is going to mean anything.
//
// The shape mirrors internal/cli.BuildState (image / tag /
// deployed_at) but it's a deliberate copy rather than a shared type:
// internal/cli depends on internal/deploytarget, so reusing the cli
// type would require either an import cycle or hoisting the type into
// a third package for one struct. The cost of a 4-field duplicate is
// lower than the cost of either alternative.
//
// File layout: one file per (provider, env, service) under
// .forge/state/. The directory is already in .gitignore via the
// existing `.forge/` rule; the file is 0o644 (same as build-state) so
// users can peek at it without sudo.
type DeployState struct {
	Image      string `json:"image"`
	Tag        string `json:"tag"`
	DeployedAt string `json:"deployed_at"` // RFC3339, wall-clock
}

// stateDirRel is the per-project on-disk location for deploy-target
// state files. Stays under .forge/state so a single .gitignore rule
// covers it alongside build-state.json and checksums.json.
const stateDirRel = ".forge/state"

// deployStatePath returns the absolute path for a per-(provider, env,
// service) state file. The env and service segments are sanitized to
// flat filenames — they come from KCL-validated identifiers in
// practice, but we strip path separators defensively.
func deployStatePath(projectDir, provider, env, service string) string {
	if env == "" {
		env = "default"
	}
	name := provider + "-" + safeStateSegment(env) + "-" + safeStateSegment(service) + ".json"
	return filepath.Join(projectDir, stateDirRel, name)
}

func safeStateSegment(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-' || c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}

// WriteDeployState persists a successful provider deploy. Returns the
// path written so callers can include it in log lines / errors. The
// state directory is created lazily so projects that never use
// non-cluster targets never grow the tree.
func WriteDeployState(projectDir, provider, env, service string, st DeployState) (string, error) {
	path := deployStatePath(projectDir, provider, env, service)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, fmt.Errorf("create deploy-state dir: %w", err)
	}
	if st.DeployedAt == "" {
		st.DeployedAt = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return path, fmt.Errorf("marshal deploy state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return path, fmt.Errorf("write deploy state: %w", err)
	}
	return path, nil
}

// ReadDeployState loads the per-(provider, env, service) state file.
// Returns (nil, nil) when the file is missing — that's "no previous
// deploy", which the caller handles separately from "file exists but
// is malformed" (returns (nil, err)).
func ReadDeployState(projectDir, provider, env, service string) (*DeployState, error) {
	path := deployStatePath(projectDir, provider, env, service)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read deploy state %s: %w", path, err)
	}
	var st DeployState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse deploy state %s: %w", path, err)
	}
	return &st, nil
}
