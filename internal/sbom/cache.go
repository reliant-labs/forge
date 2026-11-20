package sbom

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/pkg/release"
)

// CacheDirEnv relocates the SBOM cache.
const CacheDirEnv = "FORGE_SBOM_CACHE"

// Cache holds the SBOMs this machine generated, keyed by what syft read and
// which syft read it:
//
//	<dir>/<generator>/<subject>[@<platform>].cdx.json   the SBOM bytes
//	<dir>/<generator>/<subject>[@<platform>].json       {source, sbom_digest}
//
// The subject is the digest of the described bytes (an image digest, or the
// hash of a binary or lockfile), so an entry is valid for as long as those
// bytes exist, across projects, worktrees and releases: the same image cut
// into two releases, or a re-cut, runs syft once. A newer syft is a new
// generator directory, so upgrading forge regenerates rather than reusing
// an older tool's view.
//
// It is a cache, never a record: the machine ledger holds every SBOM a
// release names, and deleting this directory costs syft runs, nothing else.
// Every read re-hashes the bytes against the digest beside them, so a
// damaged entry is a miss, never served. A nil *Cache caches nothing.
type Cache struct {
	Dir string
}

// DefaultCacheDir is $FORGE_SBOM_CACHE, else <UserCacheDir>/forge/sbom —
// beside forge's other machine caches (tools, git sources, KCL).
func DefaultCacheDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(CacheDirEnv)); d != "" {
		return d, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate the SBOM cache: %w (set %s to choose one)", err, CacheDirEnv)
	}
	return filepath.Join(base, "forge", "sbom"), nil
}

type cacheMeta struct {
	Source     string `json:"source"`
	SBOMDigest string `json:"sbom_digest"`
	Format     string `json:"format"`
}

// Get returns the cached SBOM for (generator, subject, platform) and the
// source it was read from.
func (c *Cache) Get(generator, subject, platform string) (data []byte, source string, ok bool) {
	stem, ok := c.stem(generator, subject, platform)
	if !ok {
		return nil, "", false
	}
	raw, err := os.ReadFile(stem + ".json") //nolint:gosec // stem is under the cache dir, each segment flattened
	if err != nil {
		return nil, "", false
	}
	var meta cacheMeta
	if json.Unmarshal(raw, &meta) != nil || meta.Source == "" || meta.Format != Format {
		return nil, "", false
	}
	data, err = os.ReadFile(stem + ".cdx.json") //nolint:gosec // as above
	if err != nil || Digest(data) != meta.SBOMDigest || checkDocument(data) != nil {
		return nil, "", false
	}
	return data, meta.Source, true
}

// Put stores an SBOM. The bytes land before the metadata that vouches for
// them, each by rename, so a concurrent Get sees a whole entry or a miss.
func (c *Cache) Put(generator, subject, platform, source string, data []byte) error {
	stem, ok := c.stem(generator, subject, platform)
	if !ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(stem), 0o700); err != nil {
		return err
	}
	meta, err := json.Marshal(cacheMeta{Source: source, SBOMDigest: Digest(data), Format: Format})
	if err != nil {
		return err
	}
	if err := writeAtomic(stem+".cdx.json", data); err != nil {
		return err
	}
	return writeAtomic(stem+".json", meta)
}

func (c *Cache) stem(generator, subject, platform string) (string, bool) {
	if c == nil || c.Dir == "" || generator == "" || !release.ValidDigest(subject) {
		return "", false
	}
	name := strings.Replace(subject, ":", "-", 1)
	if platform != "" {
		name += "@" + safeSegment(platform)
	}
	return filepath.Join(c.Dir, safeSegment(generator), name), true
}

// safeSegment flattens a value into one path segment.
func safeSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '@':
			return r
		}
		return '_'
	}, s)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
