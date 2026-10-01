package storage

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Temp sweep: reclaim abandoned scratch directories in the system temp dir.
//
// These are not forge's own artifacts in the sense the rest of this package
// means it — nobody declared them and no project owns them — but they are the
// single largest unreclaimed consumer on a developer machine that builds Go
// all day. Measured on one host: 128 `go-link-*` dirs holding 15 GB (linker
// scratch orphaned when a link is SIGKILLed, which `cmd/link` would otherwise
// have removed on exit), plus several hundred leaked test fixtures.
//
// The whole design is about never deleting something that is still wanted,
// because $TMPDIR is shared with every other tool on the machine:
//
//   - A FIXED ALLOWLIST of name prefixes, never a pattern or a blanket sweep.
//     An entry forge does not recognize is left alone forever, even if it is
//     ancient and enormous.
//   - TOP LEVEL ONLY, so a prefix match can never be inherited from a parent.
//   - 24h of total quiescence, measured as the NEWEST mtime anywhere inside
//     the entry — not the root's own mtime, which a directory keeps from its
//     creation while files churn underneath it.
//   - Not open by any process. One lsof snapshot covers every candidate, and
//     a missing or failing lsof skips the whole layer: without that evidence
//     the age test alone cannot distinguish abandoned from merely idle.
//   - Never a git worktree. A `.git` FILE is the gitdir pointer a real
//     worktree has, so it means stop. A `.git` DIR is allowed only under
//     `tierguard-`, whose fixtures `git init` a scaffolded project on purpose.
type tempSweep struct {
	root   string
	now    time.Time
	maxAge time.Duration
	// openPaths reports every path currently held open by any process.
	// Injectable so tests need neither a real lsof nor a real open file.
	openPaths func() (map[string]bool, error)
	print     func(string, ...any)
}

// tempSweepPrefixes is the allowlist. Each entry is scratch that its producer
// is KNOWN to abandon — Go toolchain scratch orphaned by a killed process, the
// upstream embedded-postgres log file (created per start, never removed), and
// forge's own sync.Once test fixtures, which are now cleaned up at the source
// but leave historical copies on disk.
var tempSweepPrefixes = []string{
	"go-link-",
	"go-build",
	"embedded_postgres_log",
	"forge-e2e-bin-",
	"forge-bin-",
	"forge-skill-validate-",
	"forge-kcl-module-test-",
	"tierguard-",
	"forge-k3d-",
}

// tempSweepGitDirPrefixes names the prefixes whose fixtures legitimately
// contain a `.git` DIRECTORY. Everything else with a `.git` of any kind is
// treated as someone's checkout and skipped.
var tempSweepGitDirPrefixes = []string{"tierguard-"}

const tempSweepAge = 24 * time.Hour

// tempSweepWalkLimit bounds the per-entry walk. A scaffolded fixture tree is a
// few thousand files; anything larger is not a shape we recognize, and the
// sweep declines rather than spending unbounded time measuring it.
const tempSweepWalkLimit = 200_000

// tempRoot resolves the directory this sweep reclaims from.
//
// Under `go test` an unset root is REFUSED rather than defaulted to
// os.TempDir(). This layer is one of only two in GC whose blast radius is a
// fixed ABSOLUTE path outside the project — Logs walks Policy.Projects (empty
// by default, so inert) and the registry layers go through an injected
// Command, but an unset TempRoot would apply-sweep the developer's REAL
// $TMPDIR. Four pre-existing tests in this package call GC(ctx, true) to
// assert things about Docker endpoints and builder pruning; none of them has
// any reason to think about a temp sweep, and the identical hole in the
// Sources layer deleted five real source clones before it was closed.
//
// So the safeguard cannot be "remember to set a field". Making the default
// unreachable from a test turns a silent deletion into a message that names
// what to set.
func (r Runner) tempRoot() (string, error) {
	if r.TempRoot != "" {
		return r.TempRoot, nil
	}
	if testing.Testing() {
		return "", fmt.Errorf("storage.Runner.TempRoot is unset under test: " +
			"set it to a t.TempDir() (an unset root would apply-sweep the developer's real $TMPDIR)")
	}
	return os.TempDir(), nil
}

// TempSweep reclaims abandoned scratch entries in the system temp directory.
// Dry-run by default: it prints each candidate and its size, and removes
// nothing unless apply is set.
func (r Runner) TempSweep(apply bool) error {
	root, err := r.tempRoot()
	if err != nil {
		r.print("skip temp sweep (%v)\n", err)
		return nil
	}
	s := tempSweep{
		root:      root,
		now:       time.Now(),
		maxAge:    tempSweepAge,
		openPaths: lsofOpenPaths,
		print:     r.print,
	}
	return s.run(apply)
}

type tempCandidate struct {
	path   string
	size   int64
	newest time.Time
}

func (s tempSweep) run(apply bool) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		// No temp dir to sweep is not a maintenance failure.
		return nil
	}

	// Collect the name-matched set first, so lsof is only paid for when there
	// is something it could protect.
	var named []string
	for _, entry := range entries {
		if !tempSweepAllowed(entry.Name()) {
			continue
		}
		// A symlink at the top level is never followed and never removed:
		// its target is outside the root this sweep is scoped to.
		if entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		named = append(named, entry.Name())
	}
	if len(named) == 0 {
		return nil
	}

	open, err := s.openPaths()
	if err != nil {
		// Fail closed. Age alone cannot tell an abandoned entry from one a
		// live process is still writing to, and deleting the latter breaks a
		// running build for bytes that would have been reclaimed next pass.
		s.print("temp sweep skipped: cannot determine which temp entries are in use (%v)\n", err)
		return nil
	}

	var candidates []tempCandidate
	var skippedOpen, skippedYoung, skippedGit int
	for _, name := range named {
		path := filepath.Join(s.root, name)
		if pathHeldOpen(open, path) {
			skippedOpen++
			continue
		}
		size, newest, hasGit, walkErr := s.measure(path, name)
		if walkErr != nil {
			continue
		}
		if hasGit {
			skippedGit++
			continue
		}
		if s.now.Sub(newest) < s.maxAge {
			skippedYoung++
			continue
		}
		candidates = append(candidates, tempCandidate{path: path, size: size, newest: newest})
	}

	var total int64
	var removed int
	for _, c := range candidates {
		s.print("temp sweep: %s (%d bytes, idle %s)\n", c.path, c.size, s.now.Sub(c.newest).Round(time.Hour))
		if !apply {
			total += c.size
			continue
		}
		if err := removeWritable(c.path); err != nil {
			s.print("temp sweep: could not remove %s: %v\n", c.path, err)
			continue
		}
		total += c.size
		removed++
	}
	if len(candidates) > 0 || skippedOpen > 0 || skippedYoung > 0 || skippedGit > 0 {
		verb := "reclaimable"
		if apply {
			verb = "reclaimed"
		}
		s.print("temp sweep: %d entries %s (%.2f GiB); retained %d in use, %d recent, %d with git metadata\n",
			map[bool]int{true: removed, false: len(candidates)}[apply], verb,
			float64(total)/float64(GiB), skippedOpen, skippedYoung, skippedGit)
	}
	return nil
}

// measure walks one candidate, returning its total size, the newest mtime
// anywhere inside it, and whether it carries git metadata that disqualifies
// it. WalkDir does not follow symlinks, which is the behaviour this needs:
// a link inside the entry must not let the walk escape the entry.
func (s tempSweep) measure(path, name string) (size int64, newest time.Time, hasGit bool, err error) {
	gitDirOK := false
	for _, prefix := range tempSweepGitDirPrefixes {
		if strings.HasPrefix(name, prefix) {
			gitDirOK = true
			break
		}
	}
	visited := 0
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable child makes the measurement incomplete, which
			// means the entry cannot be shown to be idle. Abort it.
			return walkErr
		}
		if visited++; visited > tempSweepWalkLimit {
			return fmt.Errorf("temp entry %s exceeds the %d-file walk limit", path, tempSweepWalkLimit)
		}
		if d.Name() == ".git" {
			// A `.git` FILE is a worktree's gitdir pointer: never ours.
			// A `.git` DIR is only ours under an allowlisted fixture prefix.
			if !d.IsDir() || !gitDirOK {
				hasGit = true
				return filepath.SkipAll
			}
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		if d.Type().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return size, newest, hasGit, err
}

// tempSweepAllowed reports whether name is on the fixed prefix allowlist.
func tempSweepAllowed(name string) bool {
	for _, prefix := range tempSweepPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// pathHeldOpen reports whether any open path IS the entry or lives under it.
func pathHeldOpen(open map[string]bool, path string) bool {
	if open[path] {
		return true
	}
	prefix := path + string(filepath.Separator)
	for p := range open {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// lsofOpenPaths takes ONE system-wide snapshot of open file paths.
//
// One snapshot rather than a probe per candidate: lsof costs seconds, and a
// per-entry `lsof +D` would also race — an entry could be opened between its
// own probe and its removal. A single snapshot is both cheaper and a
// consistent point in time to decide against.
//
// lsof exits non-zero whenever ANY process's file list was inaccessible,
// which on a developer machine is routine, so the exit code is not the
// signal: output that parsed into at least one path is treated as a usable
// snapshot, and anything else is an error the caller fails closed on.
func lsofOpenPaths() (map[string]bool, error) {
	if _, err := exec.LookPath("lsof"); err != nil {
		return nil, fmt.Errorf("lsof not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// -Fn: one field per line, names prefixed 'n'. -w: no warning lines.
	cmd := exec.CommandContext(ctx, "lsof", "-Fn", "-w")
	out, err := cmd.Output()
	open := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 2 || line[0] != 'n' {
			continue
		}
		if name := line[1:]; strings.HasPrefix(name, "/") {
			open[name] = true
		}
	}
	if len(open) == 0 {
		if err != nil {
			return nil, fmt.Errorf("lsof: %w", err)
		}
		return nil, fmt.Errorf("lsof returned no open paths")
	}
	return open, nil
}

// removeWritable makes a tree writable, then removes it.
//
// The chmod pass is required, not defensive: a leaked forge fixture contains a
// Go module cache, whose files and directories are mode 0444/0555 by design,
// and RemoveAll cannot unlink a child of a directory it cannot write.
func removeWritable(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best effort; RemoveAll reports what actually fails
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o700)
		} else if d.Type().IsRegular() {
			_ = os.Chmod(p, 0o600)
		}
		return nil
	})
	return os.RemoveAll(path)
}
