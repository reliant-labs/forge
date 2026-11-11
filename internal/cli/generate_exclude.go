package cli

// generate_exclude.go — scaffold.exclude at the level of whole commands.
//
// The write chokepoints (internal/checksums/exclude.go) decline an excluded
// path before writing it and record the skip. Two things are left for this
// file:
//
//  1. Outputs no chokepoint writes: buf's stubs and the frontend hooks. They
//     are directories, decided here per frontend, and a glob over a generated
//     tree other generated code imports is refused rather than skipped.
//  2. The guarantee. A forge run writes through hundreds of sites; most go
//     through a chokepoint, and some — a raw write, an external tool — do
//     not. So every run that holds the generate lock (generate, every
//     scaffold, rescaffold, project new) is bracketed: the files under an
//     excluded glob are captured when the lock is taken, and anything the run
//     changed there is put back before the lock is released and reported as
//     a skipped write. "forge never writes there" is then a property of the
//     run, not of each writer remembering to ask.

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/pathglob"
)

// skipExcludedDir reports whether forge must not write the generated
// directory relDir, judged by probe, a file name forge writes there. It
// records the skip.
func skipExcludedDir(root, relDir string) bool {
	probe := filepath.ToSlash(filepath.Join(relDir, "x-hooks.ts"))
	glob, ok := checksums.ExcludedBy(root, probe)
	if ok {
		checksums.NoteExcluded(filepath.ToSlash(relDir)+"/", glob)
	}
	return ok
}

// refuseExcludedGoStubs fails the run when scaffold.exclude covers gen/: the
// protobuf and Connect stubs every handler and the generated composition
// import, regenerated from proto on every run. There is no half of that
// pairing a project can own while forge keeps the other.
func refuseExcludedGoStubs(root string) error {
	probes := []string{"gen/forge_descriptor.json", "gen/services/x/v1/x.pb.go", "gen/services/x/v1/xv1connect/x.connect.go"}
	_ = filepath.WalkDir(filepath.Join(root, "gen"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && len(probes) < 64 {
			if rel, rerr := filepath.Rel(root, p); rerr == nil {
				probes = append(probes, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	for _, p := range probes {
		if glob, ok := checksums.ExcludedBy(root, p); ok {
			return &checksums.ExcludeConflictError{Path: "gen/", Glob: glob,
				Dependent: "every handler under internal/handlers/, internal/app's generated mounts and each frontend's hooks",
				Instead:   "To stop forge generating code at all, pin `features: {codegen: false}`"}
		}
	}
	return nil
}

// excludedTSStubs decides a frontend's TypeScript stubs (<feDir>/src/gen)
// under scaffold.exclude: skip when they are excluded together with the hooks
// that import them, refuse when only the stubs are.
func excludedTSStubs(root, feDir string) (skip bool, err error) {
	genProbe := filepath.ToSlash(filepath.Join(feDir, "src", "gen", "x_pb.ts"))
	glob, ok := checksums.ExcludedBy(root, genProbe)
	if !ok {
		return false, nil
	}
	hooksProbe := filepath.ToSlash(filepath.Join(feDir, "src", "hooks", "x-hooks.ts"))
	if _, hooksExcluded := checksums.ExcludedBy(root, hooksProbe); !hooksExcluded {
		return true, &checksums.ExcludeConflictError{Path: filepath.ToSlash(filepath.Join(feDir, "src", "gen")) + "/", Glob: glob,
			Dependent: "the generated hooks in " + filepath.ToSlash(filepath.Join(feDir, "src", "hooks")) + "/",
			Instead:   "Exclude the hooks with it (`" + filepath.ToSlash(feDir) + "/src/hooks/`), or the whole frontend (`" + filepath.ToSlash(feDir) + "/`)"}
	}
	checksums.NoteExcluded(filepath.ToSlash(filepath.Join(feDir, "src", "gen"))+"/", glob)
	return true, nil
}

// excludeCaptureLimit bounds the bytes one excluded file may hold in memory
// for the run. A larger file is fingerprinted only; a run that changes one is
// reported, since it cannot be put back.
const excludeCaptureLimit = 16 << 20

// excludeWalkSkip are directories the guard never descends: no forge writer
// targets them, and walking node_modules under a `frontends/web/**` glob
// would dominate the run.
var excludeWalkSkip = map[string]bool{".git": true, "node_modules": true, ".forge": true, ".next": true}

type excludedFile struct {
	data []byte // nil when over excludeCaptureLimit
	hash string
	mode fs.FileMode
	glob string
}

// excludeGuard is one locked run's capture of the files under
// scaffold.exclude (see the file comment).
type excludeGuard struct {
	root   string
	globs  []string
	exempt []string // generate.hooks writes: the project's own commands
	pre    map[string]excludedFile
	mu     sync.Mutex
}

var (
	activeExcludeGuardMu sync.Mutex
	activeExcludeGuard   *excludeGuard
)

// armExcludeGuard captures the excluded files of the project at root. It
// returns nil — a no-op guard — for a project with no scaffold.exclude.
func armExcludeGuard(root string) *excludeGuard {
	globs := checksums.ExcludeGlobs(root)
	if len(globs) == 0 {
		return nil
	}
	g := &excludeGuard{root: root, globs: globs, exempt: config.ReadGenerateHookWrites(root),
		pre: map[string]excludedFile{}}
	g.walk(func(rel, glob string, full string, fi fs.FileInfo) {
		f := excludedFile{mode: fi.Mode().Perm(), glob: glob}
		data, err := os.ReadFile(full)
		if err != nil {
			return
		}
		f.hash = checksums.Hash(data)
		if len(data) <= excludeCaptureLimit {
			f.data = data
		}
		g.pre[rel] = f
	})
	activeExcludeGuardMu.Lock()
	activeExcludeGuard = g
	activeExcludeGuardMu.Unlock()
	return g
}

// walk visits every regular file under root an exclude glob matches and no
// generate hook declares as its output.
func (g *excludeGuard) walk(visit func(rel, glob, full string, fi fs.FileInfo)) {
	seen := map[string]bool{}
	for _, glob := range g.globs {
		start := filepath.Join(g.root, filepath.FromSlash(pathglob.StaticPrefix(glob)))
		_ = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != start && excludeWalkSkip[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(g.root, p)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if seen[rel] || !d.Type().IsRegular() || g.isExempt(rel) {
				return nil
			}
			if matched, ok := g.matches(rel); ok {
				seen[rel] = true
				if fi, ierr := d.Info(); ierr == nil {
					visit(rel, matched, p, fi)
				}
			}
			return nil
		})
	}
}

func (g *excludeGuard) matches(rel string) (string, bool) {
	for _, glob := range g.globs {
		if pathglob.Match(glob, rel) {
			return glob, true
		}
	}
	return "", false
}

func (g *excludeGuard) isExempt(rel string) bool {
	for _, w := range g.exempt {
		w = strings.TrimSuffix(filepath.ToSlash(w), "/")
		if rel == w || strings.HasPrefix(rel, w+"/") {
			return true
		}
	}
	return false
}

// settle puts back every excluded file the run changed, created or deleted,
// records each as a skipped write, and returns the paths it could not restore.
// Safe to call more than once: a settled tree matches the capture.
func (g *excludeGuard) settle() (unrestored []string) {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := map[string]bool{}
	g.walk(func(rel, glob, full string, _ fs.FileInfo) {
		now[rel] = true
		before, had := g.pre[rel]
		if !had {
			// Created by the run under an excluded glob: forge's write.
			if err := os.Remove(full); err != nil {
				unrestored = append(unrestored, rel)
				return
			}
			pruneEmptyDirsUpTo(filepath.Dir(full), g.root)
			checksums.NoteExcluded(rel, glob)
			return
		}
		data, err := os.ReadFile(full)
		if err != nil || checksums.Hash(data) == before.hash {
			return
		}
		if !g.restore(rel, full, before) {
			unrestored = append(unrestored, rel)
			return
		}
		checksums.NoteExcluded(rel, before.glob)
	})
	for rel, before := range g.pre {
		if now[rel] {
			continue
		}
		// Deleted by the run (a stale-output sweep): put it back.
		full := filepath.Join(g.root, filepath.FromSlash(rel))
		if !g.restore(rel, full, before) {
			unrestored = append(unrestored, rel)
			continue
		}
		checksums.NoteExcluded(rel, before.glob)
	}
	sort.Strings(unrestored)
	return unrestored
}

func (g *excludeGuard) restore(rel, full string, f excludedFile) bool {
	if f.data == nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false
	}
	return os.WriteFile(full, f.data, f.mode) == nil
}

// pruneEmptyDirsUpTo removes dir and its parents while they are empty,
// stopping at root.
func pruneEmptyDirsUpTo(dir, root string) {
	for dir != root && strings.HasPrefix(dir, root) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// reportExcludedWrites settles the active guard and prints the run's skipped
// writes under scaffold.exclude: one line per path, at most
// excludeReportPerGlob per glob unless the run is verbose.
func reportExcludedWrites(w io.Writer) {
	unrestored := settleActiveExcludeGuard()
	printExcludedSkips(w, checksums.TakeExcludedSkips(), generateVerbose)
	printUnrestoredExcluded(w, unrestored)
}

// settleActiveExcludeGuard settles the guard of the locked run in progress,
// if any (see excludeGuard.settle).
func settleActiveExcludeGuard() []string {
	activeExcludeGuardMu.Lock()
	g := activeExcludeGuard
	activeExcludeGuardMu.Unlock()
	return g.settle()
}

func printUnrestoredExcluded(w io.Writer, unrestored []string) {
	for _, rel := range unrestored {
		fmt.Fprintf(w, "⚠️  %s is excluded by scaffold.exclude, and this run changed it; forge could not put it back (git restore it)\n", rel)
	}
}

// releaseExcludeGuard is reportExcludedWrites for the end of a locked run.
func releaseExcludeGuard(g *excludeGuard, w io.Writer) {
	if g == nil {
		return
	}
	reportExcludedWrites(w)
	activeExcludeGuardMu.Lock()
	if activeExcludeGuard == g {
		activeExcludeGuard = nil
	}
	activeExcludeGuardMu.Unlock()
}

// excludeReportPerGlob caps the default report per glob: an excluded tree can
// hold every page of a frontend, and the point is that forge said so, once.
const excludeReportPerGlob = 5

func printExcludedSkips(w io.Writer, skips []checksums.ExcludedSkip, verbose bool) {
	if len(skips) == 0 {
		return
	}
	shown := map[string]int{}
	hidden := map[string]int{}
	for _, s := range skips {
		if !verbose && shown[s.Glob] >= excludeReportPerGlob {
			hidden[s.Glob]++
			continue
		}
		shown[s.Glob]++
		fmt.Fprintf(w, "  ↷ %s skipped (scaffold.exclude: %s)\n", s.Path, s.Glob)
	}
	globs := make([]string, 0, len(hidden))
	for g := range hidden {
		globs = append(globs, g)
	}
	sort.Strings(globs)
	for _, g := range globs {
		fmt.Fprintf(w, "  ↷ … and %d more skipped (scaffold.exclude: %s); -v lists them\n", hidden[g], g)
	}
}
