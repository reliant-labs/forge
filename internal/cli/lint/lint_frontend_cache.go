// File: internal/cli/lint/lint_frontend_cache.go
//
// The frontend lint lane's pass cache. `npm run lint` (eslint over the whole
// frontend) was ~57s of a 61s warm `forge lint` on a fresh scaffold, and an
// agent working on the backend re-ran it unchanged on every lint. A frontend
// whose lint inputs are byte-identical to the ones its last PASS judged is
// not linted again: the lane reports "unchanged since its last pass, cached
// pass" instead, in every output mode, and the verdict names it.
//
// WHAT THE KEY COVERS. Everything the lane's verdict reads that a project
// edit or a machine change can move:
//
//   - every file under the frontend's directory, by path and content, minus
//     dependency trees and build output (frontendLintSkipDirs) and the files
//     a tool rewrites on its own (frontendLintSkipFiles). .gitignore is NOT
//     honoured: eslint does not read it, so an ignored file it still lints
//     (generated code a project chose not to commit) must still invalidate.
//   - the installed dependencies, through the stamp each package manager
//     rewrites on every install (node_modules/.package-lock.json,
//     .modules.yaml, …) in the node_modules the frontend resolves from, and
//     the complete runtime dependency closure of what eslint loads (eslint,
//     typescript, its config's imports, the eslint-shaped packages
//     package.json lists, and everything they depend on) by the content of
//     every file, so a dependency patched in place without a reinstall is a
//     change too (lint_frontend_cache_deps.go). Executable and legacy
//     configs are left uncached: their dependencies cannot be proven complete
//     by static import discovery.
//   - for a workspace member whose dependencies are hoisted to an ancestor:
//     that workspace root's own files (manifests, lockfile, shared configs),
//     its membership — the normalized pnpm-workspace.yaml / package.json
//     `workspaces` patterns, the member set they resolve to, and the entries
//     of each directory a pattern globs in, so a member appearing or
//     disappearing (even an empty directory) is a change — and every
//     member's files. Directories are keyed as well as files throughout.
//     Recursive globs, negations and other unsupported workspace syntax
//     disable caching rather than approximate package-manager membership.
//   - the scripts the lane runs (and whether css_health adds lint:styles),
//     the forge binary (version, path, size, mtime), node and npm (path,
//     size, mtime, content) and the complete inherited environment: arbitrary
//     project scripts and their child tools may read any variable. Environment
//     values are hashed into the key, never stored in the cache record.
//
// Only passes are recorded, and only through recordPass: the key is taken
// before the run, re-checked after it, written, and re-checked after the
// write, which deletes the record on a mismatch. An edit during the run, an
// `eslint --fix` that rewrote something, or an edit racing the write leaves
// no record. A failure is never cached: the next
// run lints again. A --fix run never consults the cache (it exists to run
// eslint --fix) but may record its pass.
//
// TURNING IT OFF. `forge lint --no-cache`, FORGE_LINT_NO_CACHE=1, or a CI
// environment (CI set, as every hosted runner sets it) runs the lane every
// time; each still records its pass. CI's contract is that a gate run judges
// the tree in front of it, and a CI checkout has no .forge/ state anyway.
// `forge ci gate`'s lint-<frontend> rows run `npm run lint` directly and have
// their own verdict cache (internal/cigate/cache.go); this one is not on
// their path.

package lint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/statefile"
)

const (
	// frontendLintCacheRel is the record, in the project's own (gitignored)
	// .forge/, beside generate's .forge/generate-cache.json.
	frontendLintCacheRel = ".forge/lint-cache.json"

	// frontendLintCacheVersion is part of every key. Bump it whenever the
	// fingerprint's meaning changes, so a record written under the old rules
	// never answers under the new ones.
	frontendLintCacheVersion = 5

	// lintNoCacheEnv is --no-cache for runs nobody types (a gate script).
	lintNoCacheEnv = "FORGE_LINT_NO_CACHE"
)

// frontendLintSkipDirs are directories never part of a frontend's lint
// inputs, at any depth: dependency trees (covered by the install stamp),
// build and test output, and forge/git state.
var frontendLintSkipDirs = map[string]bool{
	"node_modules": true,
	".next":        true,
	".next-prod":   true,
	"out":          true,
	"dist":         true,
	"coverage":     true,
	".turbo":       true,
	".git":         true,
	".forge":       true,
	".forge-link":  true,
}

// frontendLintSkipFiles reports files a tool rewrites by itself, which would
// otherwise invalidate the record on every run: tsc's incremental build info
// (the typecheck lane writes it right after this lane) and eslint's own
// cache file.
func frontendLintSkipFile(name string) bool {
	return strings.HasSuffix(name, ".tsbuildinfo") || name == ".eslintcache"
}

// installStamps are the files a package manager rewrites inside node_modules
// on every install, so they move whenever the installed tree does.
var installStamps = []string{
	".package-lock.json", // npm
	".modules.yaml",      // pnpm
	".pnpm/lock.yaml",    // pnpm
	".yarn-state.yml",    // yarn berry (node-modules linker)
	".yarn-integrity",    // yarn classic
}

// frontendLintCache is one run's view of the record.
type frontendLintCache struct {
	root    string
	consult bool
	// why is why the run does not consult the record ("--no-cache", …), for
	// the lane's note; empty when it does.
	why string

	mu sync.Mutex
}

// frontendLintCacheRecord is the file's shape.
type frontendLintCacheRecord struct {
	Version int `json:"version"`
	// Frontends maps a frontend's directory (project-relative, slash form)
	// to its last pass.
	Frontends map[string]frontendLintPass `json:"frontends"`
}

// frontendLintPass is one frontend's last passing run.
type frontendLintPass struct {
	Key      string    `json:"key"`
	Files    int       `json:"files"`
	PassedAt time.Time `json:"passed_at"`
}

// newFrontendLintCache is the cache for a run rooted at root. noCache is
// --no-cache; getenv reads the environment (a seam for tests).
func newFrontendLintCache(root string, noCache bool, getenv func(string) string) *frontendLintCache {
	c := &frontendLintCache{root: root, consult: true}
	switch {
	case noCache:
		c.consult, c.why = false, "--no-cache"
	case envTruthy(getenv(lintNoCacheEnv)):
		c.consult, c.why = false, lintNoCacheEnv+"=1"
	case envTruthy(getenv("CI")):
		c.consult, c.why = false, "CI is set"
	}
	return c
}

func envTruthy(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

func (c *frontendLintCache) path() string {
	return filepath.Join(c.root, filepath.FromSlash(frontendLintCacheRel))
}

// entryKey is the record's key for a frontend directory.
func (c *frontendLintCache) entryKey(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.ToSlash(dir)
	}
	root, err := filepath.Abs(c.root)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	if rel, err := filepath.Rel(root, abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

func (c *frontendLintCache) load() frontendLintCacheRecord {
	rec, err := statefile.Read[frontendLintCacheRecord](c.path(), "lint cache")
	if err != nil || rec == nil || rec.Version != frontendLintCacheVersion {
		return frontendLintCacheRecord{Version: frontendLintCacheVersion}
	}
	return *rec
}

// hit reports whether dir's last pass was judged on exactly key.
func (c *frontendLintCache) hit(dir, key string) (frontendLintPass, bool) {
	if c == nil || !c.consult || key == "" {
		return frontendLintPass{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.load().Frontends[c.entryKey(dir)]
	return p, ok && p.Key == key
}

// record stores a pass of dir on key. A failure to write is not the lane's
// failure: the next run simply lints again.
func (c *frontendLintCache) record(dir, key string, files int) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.load()
	if rec.Frontends == nil {
		rec.Frontends = map[string]frontendLintPass{}
	}
	rec.Frontends[c.entryKey(dir)] = frontendLintPass{Key: key, Files: files, PassedAt: time.Now().UTC()}
	_ = statefile.Write(c.path(), "lint cache", rec)
}

// recordPass records that the run which keyed dir's inputs as key before
// it started passed. The protocol leaves no record for inputs that changed
// after that key was taken:
//
//  1. re-hash after the run: a mismatch (an edit during the run, or an
//     eslint --fix rewrite) records nothing and drops any older record;
//  2. write the record for key;
//  3. re-hash after the write: a mismatch (an edit between step 1 and the
//     write) deletes the record just written.
//
// An edit after step 3 is harmless: the record names key, and the next run
// hashes the edited tree to a different key. It reports whether a record
// survived.
func (c *frontendLintCache) recordPass(dir string, run frontendLintRun, key string) bool {
	if c == nil || key == "" {
		return false
	}
	after, n, err := c.fingerprint(dir, run)
	if err != nil || after != key {
		c.forget(dir)
		return false
	}
	lintCacheBeforeRecord()
	c.record(dir, key, n)
	if again, _, err := c.fingerprint(dir, run); err != nil || again != key {
		c.forget(dir)
		return false
	}
	return true
}

// lintCacheBeforeRecord runs between recordPass's post-run hash and its
// write: a seam for the test that edits the tree at exactly that boundary.
var lintCacheBeforeRecord = func() {}

// forget drops dir's record, so a failing run can never be followed by a
// cached pass of an older tree that the record still describes.
func (c *frontendLintCache) forget(dir string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.load()
	k := c.entryKey(dir)
	if _, ok := rec.Frontends[k]; !ok {
		return
	}
	delete(rec.Frontends, k)
	_ = statefile.Write(c.path(), "lint cache", rec)
}

// frontendLintRun is what one frontend's lane runs: the inputs to the key
// that are not files.
type frontendLintRun struct {
	// scripts are the npm scripts the lane runs, in order.
	scripts []string
	// nmDir is the directory whose node_modules the frontend resolves from
	// (nearestNodeModulesDir); "" when unknown.
	nmDir string
}

// fingerprint is the key of dir's lint inputs for run, and the number of
// files it read. An error means the inputs could not be read in full: the
// lane runs and records nothing.
func (c *frontendLintCache) fingerprint(dir string, run frontendLintRun) (string, int, error) {
	h := sha256.New()
	fmt.Fprintf(h, "forge-lint-frontend-cache v%d\n", frontendLintCacheVersion)
	fmt.Fprintf(h, "scripts=%s\n", strings.Join(run.scripts, ","))
	fmt.Fprintf(h, "forge=%s %s\n", buildinfo.Version(), executableStamp(lintCacheExecutable))
	for _, tool := range []string{"node", "npm"} {
		p, err := lintCacheLookPath(tool)
		if err != nil {
			fmt.Fprintf(h, "tool:%s=absent\n", tool)
			continue
		}
		fmt.Fprintf(h, "tool:%s=%s\n", tool, fileStamp(p))
		if err := hashFile(h, "tool:"+tool, p); err != nil {
			return "", 0, err
		}
	}
	for _, kv := range lintCacheEnviron() {
		fmt.Fprintf(h, "env:%q\n", kv)
	}

	files := 0
	n, err := hashTree(h, "fe", dir)
	if err != nil {
		return "", 0, err
	}
	files += n

	// The packages eslint loads, by content: an in-place edit of a resolved
	// plugin moves no install stamp (lint_frontend_cache_deps.go).
	n, err = hashLintDeps(h, dir)
	if err != nil {
		return "", 0, err
	}
	files += n

	if run.nmDir != "" {
		for _, s := range installStamps {
			p := filepath.Join(run.nmDir, "node_modules", filepath.FromSlash(s))
			if err := hashFile(h, "stamp:"+s, p); err != nil && !os.IsNotExist(err) {
				return "", 0, err
			}
		}
		if hoisted, err := sameDir(run.nmDir, dir); err == nil && !hoisted {
			n, err := hashWorkspace(h, run.nmDir, dir)
			if err != nil {
				return "", 0, err
			}
			files += n
		}
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

// Seams for tests: the forge executable, PATH lookups and the environment.
var (
	lintCacheExecutable = os.Executable
	lintCacheLookPath   = exec.LookPath
	lintCacheEnviron    = func() []string {
		env := os.Environ()
		sort.Strings(env)
		return env
	}
)

// executableStamp identifies the running forge by path, size and mtime: a
// dev build keeps its version string across every rebuild, but not its
// mtime.
func executableStamp(executable func() (string, error)) string {
	p, err := executable()
	if err != nil {
		return "unknown"
	}
	return fileStamp(p)
}

func fileStamp(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return p + " absent"
	}
	return fmt.Sprintf("%s %d %d", p, st.Size(), st.ModTime().UnixNano())
}

// hashTree adds every lint input under dir to h, by slash path relative to
// dir (prefixed with label) and content, in lexical order.
func hashTree(h io.Writer, label, dir string) (int, error) {
	files := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != dir && frontendLintSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			// Directories join the key too, so creating or removing one —
			// even empty, such as a new workspace member — is a change.
			fmt.Fprintf(h, "dir %s:%s\n", label, rel)
			return nil
		}
		if frontendLintSkipFile(d.Name()) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "link %s:%s -> %s\n", label, rel, target)
			// A link to a file is read through, so an edit to its target
			// invalidates; a link to a directory is identified by target.
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				files++
				return hashFile(h, label+":"+rel, p)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files++
		return hashFile(h, label+":"+rel, p)
	})
	return files, err
}

// hashFile adds one file's name and content hash to h.
func hashFile(h io.Writer, name, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fh := sha256.New()
	if _, err := io.Copy(fh, f); err != nil {
		return err
	}
	fmt.Fprintf(h, "file %s %x\n", name, fh.Sum(nil))
	return nil
}

// hashWorkspace adds a hoisting workspace root's own lint inputs: its files
// (manifests, lockfile, shared configs; not its subdirectories, which hold
// unrelated code such as a Go module), and every member directory it
// declares other than the frontend itself, which hashTree already covered.
func hashWorkspace(h io.Writer, root, frontendDir string) (int, error) {
	files := 0
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if e.IsDir() || frontendLintSkipFile(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		files++
		if err := hashFile(h, "ws:"+e.Name(), filepath.Join(root, e.Name())); err != nil {
			return 0, err
		}
	}
	ws := resolveWorkspace(root)
	for _, pat := range ws.patterns {
		// filepath.Glob cannot reproduce package managers' recursive globs,
		// exclusions, brace expansion or extglobs. Never reuse a verdict
		// based on an approximation of their workspace membership.
		if strings.Contains(pat, "**") || strings.ContainsAny(pat, "!{}()\\") {
			return 0, fmt.Errorf("workspace pattern %q cannot be cached safely", pat)
		}
	}
	// Membership itself is an input: the declarations as normalized, the
	// member set they resolve to, and the entries of every directory a
	// pattern globs in, so a member that appears or disappears under an
	// existing pattern (even an empty directory) changes the key, as does a
	// declaration change that the root's file hashes would also show.
	for _, pat := range ws.patterns {
		fmt.Fprintf(h, "ws-pattern %s\n", pat)
	}
	for _, m := range ws.members {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(h, "ws-member %s\n", filepath.ToSlash(rel))
	}
	for _, dir := range ws.parents {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return 0, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		fmt.Fprintf(h, "ws-glob-dir %s exists=%v\n", filepath.ToSlash(rel), err == nil)
		for _, e := range entries {
			if frontendLintSkipDirs[e.Name()] {
				continue
			}
			fmt.Fprintf(h, "ws-glob-entry %s/%s dir=%v\n", filepath.ToSlash(rel), e.Name(), e.IsDir())
		}
	}
	for _, m := range ws.members {
		if same, err := sameDir(m, frontendDir); err == nil && same {
			continue
		}
		rel, err := filepath.Rel(root, m)
		if err != nil {
			return 0, err
		}
		n, err := hashTree(h, "ws:"+filepath.ToSlash(rel), m)
		if err != nil {
			return 0, err
		}
		files += n
	}
	return files, nil
}

// workspaceMembers resolves the member directories a workspace root
// declares, from pnpm-workspace.yaml's `packages` and package.json's
// `workspaces` (array or {packages: [...]}), sorted. A `**` pattern is
// resolved to the directory before it, which covers every member under it.
// Negations are ignored here. hashWorkspace refuses to cache declarations
// with recursive patterns or negations, since this approximation does not
// represent package-manager membership semantics.
func workspaceMembers(root string) []string { return resolveWorkspace(root).members }

// workspaceDecl is what a workspace root declares and what that resolves to.
type workspaceDecl struct {
	// patterns are the declared patterns, trimmed, deduplicated and sorted
	// (negations included: they are part of the declaration).
	patterns []string
	// members are the existing member directories, sorted.
	members []string
	// parents are the directories the patterns glob in: each pattern's
	// longest literal directory prefix, sorted. Their entry sets are keyed,
	// so a member created later under an existing pattern is a change.
	parents []string
}

// resolveWorkspace reads root's workspace declarations; see workspaceMembers.
func resolveWorkspace(root string) workspaceDecl {
	var patterns []string
	if data, err := os.ReadFile(filepath.Join(root, "pnpm-workspace.yaml")); err == nil {
		var doc struct {
			Packages []string `yaml:"packages"`
		}
		if yaml.Unmarshal(data, &doc) == nil {
			patterns = append(patterns, doc.Packages...)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		var doc struct {
			Workspaces json.RawMessage `json:"workspaces"`
		}
		if json.Unmarshal(data, &doc) == nil && len(doc.Workspaces) > 0 {
			var list []string
			var obj struct {
				Packages []string `json:"packages"`
			}
			switch {
			case json.Unmarshal(doc.Workspaces, &list) == nil:
				patterns = append(patterns, list...)
			case json.Unmarshal(doc.Workspaces, &obj) == nil:
				patterns = append(patterns, obj.Packages...)
			}
		}
	}
	var ws workspaceDecl
	seen := map[string]bool{}
	seenPat := map[string]bool{}
	seenParent := map[string]bool{}
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" || seenPat[pat] {
			continue
		}
		seenPat[pat] = true
		ws.patterns = append(ws.patterns, pat)
		if strings.HasPrefix(pat, "!") {
			continue
		}
		if parent := globParent(root, pat); !seenParent[parent] {
			seenParent[parent] = true
			ws.parents = append(ws.parents, parent)
		}
		if i := strings.Index(pat, "**"); i >= 0 {
			pat = strings.TrimSuffix(pat[:i], "/")
			if pat == "" {
				pat = "."
			}
		}
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pat)))
		if err != nil {
			continue
		}
		for _, m := range matches {
			if !dirExists(m) || seen[m] {
				continue
			}
			if same, err := sameDir(m, root); err == nil && same {
				// "." or "**": the root's own files are already in; its
				// subdirectories are not a member.
				continue
			}
			seen[m] = true
			ws.members = append(ws.members, m)
		}
	}
	sort.Strings(ws.patterns)
	sort.Strings(ws.members)
	sort.Strings(ws.parents)
	return ws
}

// globParent is the directory a workspace pattern globs in: the path of its
// leading segments that hold no glob metacharacter, under root. "packages/*"
// globs in packages/, "apps/*/web" in apps/, "packages/**" in packages/
// (whose whole tree is then a member), "*" in root itself.
func globParent(root, pat string) string {
	segs := strings.Split(filepath.ToSlash(pat), "/")
	var lit []string
	for i, seg := range segs {
		if strings.ContainsAny(seg, "*?[{") {
			break
		}
		if i == len(segs)-1 {
			// A literal pattern names its member; its parent is where it
			// would be created or removed.
			break
		}
		lit = append(lit, seg)
	}
	return filepath.Join(append([]string{root}, lit...)...)
}

// sameDir reports whether a and b name the same directory.
func sameDir(a, b string) (bool, error) {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	return filepath.Clean(aa) == filepath.Clean(bb), nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// cachedPassNote is the lane's line for a frontend it did not lint because
// its inputs are unchanged since its last pass. It is a ran note (the lane's
// ✓ block prints it) marked Cached, and the verdict names it.
func cachedPassNote(name string, p frontendLintPass, scripts []string) lintJSONFinding {
	what := "npm run " + strings.Join(scripts, ", npm run ")
	f := ranNote(fmt.Sprintf("%s: unchanged since its last pass (%s, %d files, passed %s), cached pass — --no-cache re-runs %s",
		name, what, p.Files, p.PassedAt.Local().Format(time.RFC3339), what))
	f.Cached = true
	f.cachedFor = name
	return f
}
