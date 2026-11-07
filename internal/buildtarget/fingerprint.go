package buildtarget

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// fingerprintScheme names the layout of the bytes FingerprintInputs hashes.
// Bump it whenever that layout changes, so a fingerprint recorded by an older
// forge can never equal one computed by a newer forge over the same inputs
// and authorize a skip under different rules.
const fingerprintScheme = "forge-shellbuild-inputs/v1"

// prunedDirs are directory names a walk never descends into unless a
// pattern names them literally. `.git` is git's object store and index,
// never a build input; node_modules is an install tree whose lockfile is
// the input. Both are large enough that hashing them would dominate.
var prunedDirs = map[string]bool{".git": true, "node_modules": true}

// Fingerprint is the content address of a ShellBuild's declared inputs.
type Fingerprint struct {
	// Digest is "sha256:<hex>" over the scheme, the caller's recipe bytes
	// and, for every selected file in path order, its path, size,
	// executable bit and content hash.
	Digest string
	// Files and Bytes describe what was hashed, for the build log.
	Files int
	Bytes int64
	// files maps each selected path (slash-separated, relative to cwd) to
	// its content entry, so Changed can name what moved.
	files map[string]string
}

// Changed lists the paths whose entry differs between f and other (added,
// removed or modified), sorted. It names what a build touched in its own
// declared inputs.
func (f Fingerprint) Changed(other Fingerprint) []string {
	var out []string
	for p, e := range f.files {
		if other.files[p] != e {
			out = append(out, p)
		}
	}
	for p := range other.files {
		if _, ok := f.files[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// inputPattern is one parsed `inputs` entry.
type inputPattern struct {
	raw    string
	negate bool
	// clean is the slash-separated pattern relative to cwd, cleaned, so a
	// leading run of ".." segments is the only place ".." can appear.
	clean string
	segs  []string
	// root is the longest leading run of segments without glob metacharacters:
	// the directory (or file) a walk for this pattern starts from.
	root string
	// literal is true when the whole pattern has no metacharacter. A literal
	// naming a directory selects every file beneath it.
	literal bool
}

func hasMeta(s string) bool { return strings.ContainsAny(s, `*?[\`) }

func parseInputPattern(raw string) (inputPattern, error) {
	p := inputPattern{raw: raw}
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "!") {
		p.negate = true
		s = s[1:]
	}
	if s == "" {
		return p, fmt.Errorf("inputs entry %q is empty", raw)
	}
	s = filepath.ToSlash(s)
	if path.IsAbs(s) || filepath.IsAbs(s) {
		return p, fmt.Errorf("inputs entry %q is absolute; inputs are relative to the ShellBuild's cwd", raw)
	}
	p.clean = path.Clean(s)
	if p.clean == "." {
		p.clean = "**"
	}
	p.segs = strings.Split(p.clean, "/")
	for _, seg := range p.segs {
		if strings.Contains(seg, "**") && seg != "**" {
			return p, fmt.Errorf("inputs entry %q: `**` must be a whole path segment (\"src/**\", not \"src**\")", raw)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return p, fmt.Errorf("inputs entry %q: %w", raw, err)
		}
	}
	var root []string
	for _, seg := range p.segs {
		if hasMeta(seg) {
			break
		}
		root = append(root, seg)
	}
	p.literal = len(root) == len(p.segs)
	p.root = strings.Join(root, "/")
	if p.root == "" {
		p.root = "."
	}
	return p, nil
}

// matches reports whether rel (slash-separated, relative to cwd, cleaned)
// is selected by the pattern.
func (p inputPattern) matches(rel string) bool {
	if p.literal {
		return rel == p.clean || strings.HasPrefix(rel, p.clean+"/")
	}
	return matchSegments(p.segs, strings.Split(rel, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse consecutive `**`, then try every split point.
			for len(pat) > 0 && pat[0] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// namesSegment reports whether the pattern names seg literally, which is
// what lets a pattern reach into a pruned directory on purpose.
func (p inputPattern) namesSegment(seg string) bool {
	for _, s := range p.segs {
		if s == seg {
			return true
		}
	}
	return false
}

// ExpandInputs resolves patterns against cwd and returns the selected files,
// each as a slash-separated path relative to cwd, sorted.
//
// Patterns are slash-separated and relative to cwd; a leading `../` reaches a
// sibling checkout. `*`, `?` and `[...]` match within one path segment and a
// whole `**` segment matches any number of segments. A pattern without
// metacharacters naming a directory selects every file beneath it. A pattern
// starting with `!` excludes what it matches from every other pattern's
// selection, whatever their order. Walks never descend into `.git` or
// `node_modules` unless a pattern names that directory literally.
//
// An including pattern that selects no file is an error: a typo would
// otherwise silently drop an input, and the build would be skipped when the
// file it meant changed.
func ExpandInputs(cwd string, patterns []string) ([]string, error) {
	var includes, excludes []inputPattern
	for _, raw := range patterns {
		p, err := parseInputPattern(raw)
		if err != nil {
			return nil, err
		}
		if p.negate {
			excludes = append(excludes, p)
		} else {
			includes = append(includes, p)
		}
	}
	if len(includes) == 0 {
		return nil, errors.New("inputs declares only exclusions (`!…`); name at least one pattern that selects files")
	}
	selected := map[string]bool{}
	for _, p := range includes {
		n, err := walkPattern(cwd, p, selected)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("inputs entry %q matches no file under %s", p.raw, cwd)
		}
	}
	out := make([]string, 0, len(selected))
	for rel := range selected {
		excluded := false
		for _, x := range excludes {
			if x.matches(rel) {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}

// walkPattern adds every file p selects to selected and returns how many it
// matched (including ones another pattern already selected).
func walkPattern(cwd string, p inputPattern, selected map[string]bool) (int, error) {
	start := filepath.Join(cwd, filepath.FromSlash(p.root))
	info, err := os.Stat(start)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		if p.matches(p.root) {
			selected[p.root] = true
			return 1, nil
		}
		return 0, nil
	}
	// Walk the resolved directory so a root that is itself a symlink (a
	// sibling checkout linked into place) is descended, while paths are still
	// reported under the name the pattern used.
	real, err := filepath.EvalSymlinks(start)
	if err != nil {
		return 0, err
	}
	n := 0
	err = filepath.WalkDir(real, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sub, err := filepath.Rel(real, abs)
		if err != nil {
			return err
		}
		rel := path.Clean(path.Join(p.root, filepath.ToSlash(sub)))
		if d.IsDir() {
			if abs != real && prunedDirs[d.Name()] && !p.namesSegment(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if p.matches(rel) {
			selected[rel] = true
			n++
		}
		return nil
	})
	return n, err
}

// FingerprintInputs hashes recipe together with every file patterns select
// under cwd (see ExpandInputs). recipe carries everything else the command's
// output depends on — the command itself, its environment, platform and
// destination — so two builds with equal fingerprints ran the same command
// over the same bytes.
//
// Contents are hashed, not mtimes: a fresh checkout or a `git switch` there
// and back rewrites mtimes without changing a byte, and a same-second edit
// can leave an mtime unchanged.
func FingerprintInputs(cwd string, patterns []string, recipe []byte) (Fingerprint, error) {
	files, err := ExpandInputs(cwd, patterns)
	if err != nil {
		return Fingerprint{}, err
	}
	entries, total, err := hashFiles(cwd, files)
	if err != nil {
		return Fingerprint{}, err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n%d\n", fingerprintScheme, len(recipe))
	_, _ = h.Write(recipe)
	sorted := append([]string(nil), patterns...)
	sort.Strings(sorted)
	for _, p := range sorted {
		_, _ = fmt.Fprintf(h, "\npattern\x00%s", p)
	}
	fp := Fingerprint{Files: len(files), Bytes: total, files: make(map[string]string, len(files))}
	for i, rel := range files {
		_, _ = fmt.Fprintf(h, "\nfile\x00%s\x00%s", rel, entries[i])
		fp.files[rel] = entries[i]
	}
	fp.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return fp, nil
}

// hashFiles returns one entry per file — "<size> <x|-> <sha256>" — in the
// order given, hashing in parallel.
func hashFiles(cwd string, files []string) ([]string, int64, error) {
	entries := make([]string, len(files))
	sizes := make([]int64, len(files))
	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	if workers > len(files) {
		workers = len(files)
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				entry, size, err := hashFile(filepath.Join(cwd, filepath.FromSlash(files[i])))
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("hash input %s: %w", files[i], err)
					}
					mu.Unlock()
					continue
				}
				entries[i], sizes[i] = entry, size
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	if firstErr != nil {
		return nil, 0, firstErr
	}
	var total int64
	for _, s := range sizes {
		total += s
	}
	return entries, total, nil
}

func hashFile(abs string) (string, int64, error) {
	li, err := os.Lstat(abs)
	if err != nil {
		return "", 0, err
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		// A link to a regular file is its content; anything else (a link to a
		// directory, a dangling link) is the link text, so retargeting it is
		// still a change.
		if st, err := os.Stat(abs); err != nil || !st.Mode().IsRegular() {
			target, rerr := os.Readlink(abs)
			if rerr != nil {
				return "", 0, rerr
			}
			sum := sha256.Sum256([]byte(target))
			return "link " + hex.EncodeToString(sum[:]), 0, nil
		}
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	exec := "-"
	if st.Mode()&0o111 != 0 {
		exec = "x"
	}
	return fmt.Sprintf("%d %s %s", n, exec, hex.EncodeToString(h.Sum(nil))), n, nil
}
