// Package kclmodule makes forge's embedded KCL module available to a kcl
// invocation WITHOUT putting a copy inside the project.
//
// WHY THIS EXISTS, and what it replaced. forge used to materialize the
// embedded module into `<project>/.forge-kcl/` and point the project's
// kcl.mod at that path (internal/kclvendor, ADR 0001). That made the schemas
// a GENERATED ARTIFACT living in the consumer's repo, and a generated
// artifact is rewritten by whatever binary runs next. The only record of
// which forge produced the copy on disk was a version string, and a version
// cannot express lineage. Three incidents came out of that — see ADR 0002 —
// the sharpest being ~880 lines of a project's deploy-tier schemas deleted by
// a routine `forge generate`, surfacing much later as an unknown-schema error
// in a different command.
//
// A declared dependency cannot be rewritten by a binary that merely happens
// to run. So the module now lives in a machine-local cache and is handed to
// kcl as an external package at invocation time (see internal/kcloptions);
// nothing forge-owns sits in the project, so there is nothing there to stomp.
//
// KEYED BY CONTENT, NOT BY VERSION. The cache directory name is a digest of
// the embedded module itself. That is what retires the whole downgrade-guard
// problem rather than re-solving it: two different module contents can never
// collide on one path, so there is no "is the thing on disk newer than me?"
// question to get wrong. It also handles a dirty local build for free, where
// the version string stays fixed while the bytes change on every rebuild.
package kclmodule

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/reliant-labs/forge/internal/buildinfo"
	forgekcl "github.com/reliant-labs/forge/kcl"
)

// PkgName is the external-package name kcl resolves `import forge` against.
// It must match the module name in kcl/kcl.mod.
const PkgName = "forge"

// sourceFileName records, inside the cache directory, which forge build
// produced it. Purely for a human staring at the cache; nothing reads it.
const sourceFileName = ".forge-source"

var (
	once      sync.Once
	cachedIn  string
	cachedErr error
)

// Path materializes the embedded KCL module into the machine-local cache and
// returns the directory kcl should resolve `import forge` from.
//
// Idempotent and safe to call repeatedly: the work happens once per process,
// and across processes the content digest makes a second write a no-op.
func Path() (string, error) {
	once.Do(func() { cachedIn, cachedErr = materialize(cacheRoot()) })
	return cachedIn, cachedErr
}

// cacheRoot is the parent for every version of the module. It deliberately
// sits under the user cache dir and NOT in the project: the entire point is
// that no forge-owned bytes live in a consumer repo.
func cacheRoot() string {
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "forge", "kcl")
	}
	return filepath.Join(os.TempDir(), "forge-kcl")
}

// materialize writes the embedded module into <root>/<digest>/ and returns
// that directory. Split from Path so tests can point it at a temp root.
func materialize(root string) (string, error) {
	files, digest, err := embeddedModule()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, digest)

	// A complete directory at this digest already holds exactly these bytes —
	// that is what the digest means — so there is nothing to do. The marker is
	// written LAST below, so its presence is the completeness signal and a
	// half-written cache from a killed process is never mistaken for a good
	// one.
	if _, err := os.Stat(filepath.Join(dir, sourceFileName)); err == nil {
		return dir, nil
	}

	// Write to a sibling temp dir and rename, so a concurrent forge never
	// observes a partial module. Two processes racing here both produce
	// identical bytes, so whichever rename lands second is harmless.
	tmp, err := os.MkdirTemp(root, ".tmp-")
	if err != nil {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", fmt.Errorf("create kcl module cache %s: %w", root, err)
		}
		if tmp, err = os.MkdirTemp(root, ".tmp-"); err != nil {
			return "", fmt.Errorf("create kcl module staging dir: %w", err)
		}
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	for _, name := range sortedKeys(files) {
		dst := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, files[name], 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", dst, err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, sourceFileName),
		[]byte(buildinfo.Version()+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write cache marker: %w", err)
	}

	if err := os.Rename(tmp, dir); err != nil {
		// Lost a race: another process already put identical bytes there.
		if _, statErr := os.Stat(filepath.Join(dir, sourceFileName)); statErr == nil {
			return dir, nil
		}
		return "", fmt.Errorf("publish kcl module cache %s: %w", dir, err)
	}
	return dir, nil
}

// embeddedModule reads the embedded module into memory and digests it.
//
// The digest covers every path AND its content, in sorted order, with the
// path length framed in. Framing matters: without it, concatenating
// "a.k"+"bb" and "a.kb"+"b" hashes identically, and two different modules
// sharing a cache directory is the one failure this design must not have.
func embeddedModule() (map[string][]byte, string, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(forgekcl.Module, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." || d.IsDir() {
			return nil
		}
		data, rerr := fs.ReadFile(forgekcl.Module, path)
		if rerr != nil {
			return rerr
		}
		files[path] = data
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("read embedded forge KCL module: %w", err)
	}
	if len(files) == 0 {
		// An empty embed means the go:embed pattern stopped matching, which
		// would otherwise present as a KCL "module not found" far from here.
		return nil, "", fmt.Errorf("embedded forge KCL module is empty — check the go:embed patterns in kcl/embed.go")
	}

	h := sha256.New()
	for _, name := range sortedKeys(files) {
		fmt.Fprintf(h, "%d:%s", len(name), name)
		fmt.Fprintf(h, "%d:", len(files[name]))
		h.Write(files[name])
	}
	return files, hex.EncodeToString(h.Sum(nil))[:16], nil
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
