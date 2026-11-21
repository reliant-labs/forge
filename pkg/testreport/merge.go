package testreport

import (
	"crypto/sha1" //nolint:gosec // git's blob id is defined as SHA-1; it names content, it secures nothing.
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// SourceMismatchError is returned when two inputs (or two copies of a block
// in one input) give one block different statement counts, or one Go file
// different block sets: they were taken on different source. Coverage is
// never averaged across source versions; the merge is refused.
type SourceMismatchError struct {
	File  string
	Block string
	// Statements are the two disagreeing statement counts, when that is the
	// disagreement.
	Statements [2]int
	// Detail describes a block-set disagreement.
	Detail string
}

func (e *SourceMismatchError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("testreport: %s: inputs come from different source (%s)", e.File, e.Detail)
	}
	return fmt.Sprintf("testreport: %s:%s: inputs come from different source (block has %d statements in one, %d in another)", e.File, e.Block, e.Statements[0], e.Statements[1])
}

// BuildMismatchError is returned when two inputs carry different build
// fingerprints for one package ([Coverage.Builds]). It is opt-in: only a
// caller that records a true per-package source fingerprint sees it. A
// binary's covmeta hash is not one (two binaries from one tree differ), so
// it must not be passed as a fingerprint.
type BuildMismatchError struct {
	Package      string
	Fingerprints [2]string
}

func (e *BuildMismatchError) Error() string {
	return fmt.Sprintf("testreport: package %s comes from two builds (%s, %s): coverage is never merged across builds", e.Package, e.Fingerprints[0], e.Fingerprints[1])
}

// TreeMismatchError is returned when inputs were taken on different trees.
// A tree hash that is empty (unknown) does not merge with a known one either:
// the result could not say which tree it describes.
type TreeMismatchError struct {
	Trees [2]string
}

func (e *TreeMismatchError) Error() string {
	q := func(s string) string {
		if s == "" {
			return "(unknown)"
		}
		return s
	}
	return fmt.Sprintf("testreport: lanes come from different trees (%s, %s)", q(e.Trees[0]), q(e.Trees[1]))
}

// GranularityMismatchError is returned when one file is statement-granular in
// one input and line-granular in another.
type GranularityMismatchError struct {
	File string
}

func (e *GranularityMismatchError) Error() string {
	return fmt.Sprintf("testreport: %s: statement and line coverage cannot merge", e.File)
}

// OverlapError is returned when a document holds some inputs already merged
// and some not: its counts cannot be separated, so merging it would count
// the shared inputs twice.
type OverlapError struct {
	Digests []string
}

func (e *OverlapError) Error() string {
	return fmt.Sprintf("testreport: a document repeats inputs already merged (%s) beside new ones; merge the new inputs on their own", strings.Join(e.Digests, ", "))
}

func overlapping(ins []InputRef, seen map[string]bool) []string {
	var out []string
	for _, in := range ins {
		if seen[in.Digest] {
			out = append(out, in.Digest)
		}
	}
	return out
}

// MergeOptions configures [Merge].
type MergeOptions struct {
	// ModulePath and ModuleDir derive a Go file's repo-relative Path from its
	// import path: a file under ModulePath gets ModuleDir (the module's
	// directory in the repository, "" for the root) plus the rest. Files
	// that already have a Path keep it.
	ModulePath string
	ModuleDir  string
	// IncludeGenerated counts generated files toward the percentages.
	// Default false: gen/, *_gen.go (mock_gen.go included), *.pb.go,
	// *-hooks.ts and Generated are reported on their own line.
	IncludeGenerated bool
	// Generated are further generated files, as repo-relative paths (or
	// globs), e.g. the files forge's ownership record marks generated.
	Generated []string
	// Include, when set, counts only files matching one of these globs;
	// Exclude drops files matching any. Globs match the repo-relative path
	// (the import path when none is known); '**' matches any number of
	// directories. Dropped files are marked "filtered", not deleted.
	Include []string
	Exclude []string
	// Limits bounds the result (MaxBlocks, MaxCases).
	Limits Limits
}

// Merge unions coverage documents into one. Blocks are keyed by file and
// position; a block's hits are summed per lane. An input whose digest is
// already in an earlier document is skipped, so merging the same upload
// twice changes nothing.
//
// It refuses, with a typed error and never a partial result, inputs that
// cannot describe the same source. Two checks carry that rule: the tree
// check ([*TreeMismatchError]: every input names the same tree hash) and
// per-file block-set identity ([*SourceMismatchError]: a Go file has the
// same blocks in every input, and a block the same statement count, because
// instrumenting one source file always yields one block set, so a shifted
// line is caught even without a tree hash). Neither depends on which binary
// counted the blocks, so two binaries built from one tree (an api and a
// worker sharing a package) merge. It also refuses one file at two
// granularities ([*GranularityMismatchError]), a document that repeats some
// inputs already merged ([*OverlapError]), and, for callers that supply a
// true per-package fingerprint in Builds, two fingerprints for one package
// ([*BuildMismatchError]).
//
// The result's files are marked excluded per opts; the inputs are not
// modified.
func Merge(docs []*Coverage, opts MergeOptions) (*Coverage, error) {
	lim := opts.Limits.resolved()
	out := &Coverage{Head: Head{Schema: SchemaCoverage, OK: true}, Lanes: []LaneInfo{}, Inputs: []InputRef{}, Files: []CoverageFile{}}
	digests := map[string]bool{}
	var live []*Coverage
	first := true
	for _, d := range docs {
		if d == nil {
			continue
		}
		seen := 0
		for _, in := range d.Inputs {
			if digests[in.Digest] {
				seen++
			}
		}
		switch {
		case len(d.Inputs) > 0 && seen == len(d.Inputs):
			continue // already merged: idempotent
		case seen > 0:
			return nil, &OverlapError{Digests: overlapping(d.Inputs, digests)}
		}
		if first {
			out.Tree, out.Commit, out.Dirty = d.Tree, d.Commit, d.Dirty
			first = false
		} else if d.Tree != out.Tree {
			return nil, &TreeMismatchError{Trees: [2]string{out.Tree, d.Tree}}
		}
		if out.Commit == "" {
			out.Commit = d.Commit
		}
		out.Dirty = out.Dirty || d.Dirty
		for _, in := range d.Inputs {
			if !digests[in.Digest] {
				digests[in.Digest] = true
				out.Inputs = append(out.Inputs, in)
			}
		}
		for pkg, fp := range d.Builds {
			if have, ok := out.Builds[pkg]; ok && have != fp {
				return nil, &BuildMismatchError{Package: pkg, Fingerprints: [2]string{have, fp}}
			}
			if out.Builds == nil {
				out.Builds = map[string]string{}
			}
			out.Builds[pkg] = fp
		}
		live = append(live, d)
	}

	// The lanes, sorted; a lane any set input joined is a set lane.
	modes := map[Lane]Mode{}
	for _, d := range live {
		for _, l := range d.Lanes {
			if modes[l.Name] != ModeSet {
				modes[l.Name] = l.Mode
			}
		}
	}
	for name, m := range modes {
		if m == "" {
			m = ModeCount
		}
		out.Lanes = append(out.Lanes, LaneInfo{Name: name, Mode: m})
	}
	sort.Slice(out.Lanes, func(i, j int) bool { return out.Lanes[i].Name < out.Lanes[j].Name })
	laneIdx := map[Lane]int{}
	for i, l := range out.Lanes {
		laneIdx[l.Name] = i
	}

	type mfile struct {
		file  CoverageFile
		index map[blockKey]int
	}
	files := map[string]*mfile{}
	var order []string
	blocks := 0
	for _, d := range live {
		remap := make([]int, len(d.Lanes))
		for i, l := range d.Lanes {
			remap[i] = laneIdx[l.Name]
		}
		for _, f := range d.Files {
			f.Path = repoPath(f, opts)
			mf := files[f.Key()]
			if mf == nil {
				if len(files) >= lim.MaxCases {
					return nil, &LimitError{Limit: "MaxCases", Max: int64(lim.MaxCases)}
				}
				nf := f
				nf.Blocks = nil
				nf.Excluded = ""
				mf = &mfile{file: nf, index: map[blockKey]int{}}
				files[f.Key()] = mf
				order = append(order, f.Key())
			} else {
				if mf.file.Granularity != f.Granularity {
					return nil, &GranularityMismatchError{File: f.DisplayPath()}
				}
				if f.Lang == LangGo && f.Granularity == GranularityStatement && len(mf.file.Blocks) > 0 {
					if err := sameBlockSet(mf.file, f); err != nil {
						return nil, err
					}
				}
				if mf.file.ImportPath == "" {
					mf.file.ImportPath = f.ImportPath
					mf.file.Package = f.Package
				}
				if mf.file.BlobID == "" {
					mf.file.BlobID = f.BlobID
				}
			}
			for _, b := range f.Blocks {
				k := b.key()
				i, ok := mf.index[k]
				if !ok {
					if blocks >= lim.MaxBlocks {
						return nil, &LimitError{Limit: "MaxBlocks", Max: int64(lim.MaxBlocks)}
					}
					blocks++
					nb := b
					nb.Hits = make([]int64, len(out.Lanes))
					for j := range nb.Hits {
						nb.Hits[j] = -1
					}
					i = len(mf.file.Blocks)
					mf.index[k] = i
					mf.file.Blocks = append(mf.file.Blocks, nb)
				}
				have := &mf.file.Blocks[i]
				if have.Statements != b.Statements {
					return nil, &SourceMismatchError{File: f.DisplayPath(), Block: blockPos(b), Statements: [2]int{have.Statements, b.Statements}}
				}
				for li, h := range b.Hits {
					if li >= len(remap) || h < 0 {
						continue
					}
					j := remap[li]
					if have.Hits[j] < 0 {
						have.Hits[j] = 0
					}
					have.Hits[j] = addHits(have.Hits[j], h)
				}
			}
		}
	}
	for _, k := range order {
		f := files[k].file
		for i := range f.Blocks {
			for j, l := range out.Lanes {
				if l.Mode == ModeSet && f.Blocks[i].Hits[j] > 1 {
					f.Blocks[i].Hits[j] = 1
				}
			}
		}
		sortBlocks(f.Blocks)
		f.Excluded = exclusion(f, opts)
		out.Files = append(out.Files, f)
	}
	sortFiles(out.Files)
	for _, d := range live {
		out.Changed = mergeChanged(out.Changed, d.Changed)
	}
	return out, nil
}

// repoPath is a file's repo-relative path: its own, or for a Go file named
// by import path under opts.ModulePath, ModuleDir plus the rest.
func repoPath(f CoverageFile, opts MergeOptions) string {
	if f.Path != "" || f.ImportPath == "" || opts.ModulePath == "" {
		return f.Path
	}
	if rest, ok := strings.CutPrefix(f.ImportPath, opts.ModulePath+"/"); ok {
		return path.Join(opts.ModuleDir, rest)
	}
	return ""
}

// sameBlockSet checks that a Go file has the same blocks in an input as in
// the merge so far. Instrumenting one source file always yields one block
// set, whichever binary or lane built it, so a difference is proof of
// different source (a shifted line moves every block after it).
func sameBlockSet(have, f CoverageFile) error {
	if len(have.Blocks) != len(f.Blocks) {
		return &SourceMismatchError{File: f.DisplayPath(), Detail: fmt.Sprintf("%d blocks in one, %d in another", len(have.Blocks), len(f.Blocks))}
	}
	seen := make(map[blockKey]bool, len(have.Blocks))
	for _, b := range have.Blocks {
		seen[b.key()] = true
	}
	for _, b := range f.Blocks {
		if !seen[b.key()] {
			return &SourceMismatchError{File: f.DisplayPath(), Block: blockPos(b), Detail: "block " + blockPos(b) + " is in one input only"}
		}
	}
	return nil
}

func mergeChanged(a, b []ChangedFile) []ChangedFile {
	if len(b) == 0 {
		return a
	}
	idx := map[string]int{}
	for i, c := range a {
		idx[c.Path] = i
	}
	for _, c := range b {
		if i, ok := idx[c.Path]; ok {
			a[i].Ranges = append(a[i].Ranges, c.Ranges...)
			continue
		}
		idx[c.Path] = len(a)
		a = append(a, ChangedFile{Path: c.Path, Ranges: append([]LineRange(nil), c.Ranges...)})
	}
	return a
}

// exclusion says why a file does not count, or "".
func exclusion(f CoverageFile, opts MergeOptions) string {
	p := f.DisplayPath()
	if !opts.IncludeGenerated && (IsGenerated(p) || matchAny(opts.Generated, p)) {
		return ExcludedGenerated
	}
	if len(opts.Include) > 0 && !matchAny(opts.Include, p) {
		return ExcludedFiltered
	}
	if matchAny(opts.Exclude, p) {
		return ExcludedFiltered
	}
	return ""
}

// IsGenerated reports whether a path is generated by forge's conventions: a
// directory named gen, *_gen.go (mock_gen.go included), *.pb.go or
// *-hooks.ts.
func IsGenerated(p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	base := path.Base(p)
	if strings.HasSuffix(base, "_gen.go") || strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, "-hooks.ts") {
		return true
	}
	dir := path.Dir(p)
	for _, seg := range strings.Split(dir, "/") {
		if seg == "gen" {
			return true
		}
	}
	return false
}

func matchAny(globs []string, p string) bool {
	for _, g := range globs {
		if MatchGlob(g, p) {
			return true
		}
	}
	return false
}

// MatchGlob matches a slash-separated path against a glob: each segment by
// [path.Match], and '**' as any number of segments (zero included). A glob
// without a '/' matches the base name anywhere ("*_test.go").
func MatchGlob(glob, p string) bool {
	glob = strings.TrimPrefix(glob, "./")
	p = strings.TrimPrefix(strings.ReplaceAll(p, `\`, "/"), "./")
	if !strings.Contains(glob, "/") {
		ok, _ := path.Match(glob, path.Base(p))
		if ok {
			return true
		}
	}
	return matchSegs(strings.Split(glob, "/"), strings.Split(p, "/"))
}

func matchSegs(g, p []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			g = g[1:]
			if len(g) == 0 {
				return true
			}
			for i := 0; i <= len(p); i++ {
				if matchSegs(g, p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, _ := path.Match(g[0], p[0]); !ok {
			return false
		}
		g, p = g[1:], p[1:]
	}
	return len(p) == 0
}

func sortBlocks(bs []Block) {
	sort.Slice(bs, func(i, j int) bool {
		a, b := bs[i], bs[j]
		switch {
		case a.StartLine != b.StartLine:
			return a.StartLine < b.StartLine
		case a.StartCol != b.StartCol:
			return a.StartCol < b.StartCol
		case a.EndLine != b.EndLine:
			return a.EndLine < b.EndLine
		}
		return a.EndCol < b.EndCol
	})
}

func sortFiles(fs []CoverageFile) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].Lang != fs[j].Lang {
			return fs[i].Lang < fs[j].Lang
		}
		return fs[i].Key() < fs[j].Key()
	})
}

func gitBlobID(content []byte) string {
	h := sha1.New() //nolint:gosec // see the import.
	h.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}
