// Package envutil holds the small, shared environment-handling helpers
// the build, deploy, and host-launch paths all need: a minimal .env
// parser and two env-overlay merges whose precedence is encoded in the
// name. Factoring them here ends a duplication where the same operation
// name (mergeEnv) meant opposite precedence in different packages.
package envutil

import (
	"os"
	"strings"
)

// ParseDotEnv parses a .env file (KEY=VALUE per line, # comments,
// trailing whitespace trimmed) into a map. Quoted values
// ("VALUE", 'VALUE') have their outer quotes stripped. An optional
// leading "export " is stripped. Missing file returns os.ErrNotExist
// (wrapped) so callers can treat absence as non-fatal.
//
// Intentionally minimal — we don't expand $VARS or `${VAR:-default}`
// shell features. Projects needing those should use direnv or a wrapper
// script; this helper is just enough for the common
// "DATABASE_URL=postgres://..." case the host-mode loop and the
// External deploy env_file overlay need.
func ParseDotEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// strip an optional leading "export ".
		line = strings.TrimPrefix(line, "export ")
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		// Strip a single layer of matching quotes, if present.
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		out[k] = v
	}
	return out, nil
}

// MergeExtraWins layers extra KEY=VALUE pairs onto a base os.Environ()
// slice with extra winning on key conflict. Used where the overlay
// (env_file / BuildEnv) is meant to be authoritative for the variables
// it declares and the parent process's env is background context.
// Returns a fresh slice safe to assign to cmd.Env.
func MergeExtraWins(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return append([]string(nil), base...)
	}
	out := make([]string, 0, len(base)+len(extra))
	seen := map[string]struct{}{}
	for k, v := range extra {
		seen[k] = struct{}{}
		out = append(out, k+"="+v)
	}
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		if _, dup := seen[kv[:eq]]; dup {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// MergeBaseWins layers extra KEY=VALUE pairs onto a base os.Environ()
// slice with base winning on key conflict (so a developer's shell
// override always wins). Non-conflicting extras are appended. Returns a
// fresh slice safe to assign to cmd.Env.
func MergeBaseWins(base []string, extra map[string]string) []string {
	have := map[string]struct{}{}
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			have[kv[:i]] = struct{}{}
		}
	}
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	for k, v := range extra {
		if _, exists := have[k]; exists {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}
