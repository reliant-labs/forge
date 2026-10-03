package hostedimage

// The push-base cache: one record per env under .forge/state.
//
// `<registry_base>/<org>` is a fact about the PLATFORM, not about the
// project, so it is not declared anywhere in the checkout — the control plane
// returns it on every environment read. Two consumers need it where no call
// is possible:
//
//   - `forge env render <env>` and `forge lint`, which must judge a hosted
//     image offline. A render that needed a credential would stop being the
//     reproducible projection every other surface diffs against.
//   - the deploy, which must resolve the same bare image the build pushed and
//     has to reach the same answer without deriving it somewhere else.
//
// So it is written down every time the control plane states it — on the
// EnsureEnvironment forge was already making — and read back by name. The
// server is always authoritative: a cached value is used only where asking is
// impossible, and every ensure overwrites it.
//
// The FORMAT lives here, beside the rule that consumes it, because
// internal/cli writes it and internal/cli/lint reads it and the two cannot
// import each other. A second definition of the filename would be a cache
// one side silently never finds.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CacheDirRel is where the records live, relative to the project root. It
// matches internal/statefile.DirRel; stated here so this package has no
// dependency on it for a path.
const CacheDirRel = ".forge/state"

// cacheFilePrefix is the per-env record's name prefix.
const cacheFilePrefix = "push-base-"

// CacheRecord is one env's last-known push base. RecordedAt is for the human
// reading the file, so a stale value is legible rather than mysterious.
type CacheRecord struct {
	Env        string `json:"env"`
	PushBase   string `json:"image_push_base"`
	RecordedAt string `json:"recorded_at"`
}

// CachePath is the record for one env.
func CachePath(projectDir, env string) string {
	return filepath.Join(projectDir, CacheDirRel, cacheFilePrefix+safeSegment(env)+".json")
}

// CachedBase is the base last recorded for this env, or "" when none was.
//
// An unreadable or malformed record is "" as well: a corrupt cache must
// degrade to "unknown", which has its own weaker message, rather than fail a
// render over a file that exists only to sharpen one.
func CachedBase(projectDir, env string) string {
	return readCache(CachePath(projectDir, env))
}

// AnyCachedBase is the base from any env's record, for a consumer that cannot
// attribute a declaration to an env (the lint's text scan).
//
// Using SOME base only ever makes a finding stronger — it can then name the
// subtree and the exact replacement — and the comparison still reports only
// an image whose host is not the platform's. Envs are read in name order so
// the choice is deterministic rather than filesystem-dependent.
func AnyCachedBase(projectDir string) string {
	entries, err := os.ReadDir(filepath.Join(projectDir, CacheDirRel))
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), cacheFilePrefix) && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if base := readCache(filepath.Join(projectDir, CacheDirRel, name)); base != "" {
			return base
		}
	}
	return ""
}

func readCache(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var rec CacheRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return ""
	}
	return NormalizeBase(rec.PushBase)
}

// safeSegment strips path separators so an env name can never escape the
// state directory. The same rule internal/statefile applies.
func safeSegment(s string) string {
	s = strings.ReplaceAll(s, string(filepath.Separator), "-")
	s = strings.ReplaceAll(s, "/", "-")
	return strings.ReplaceAll(s, "..", "-")
}
