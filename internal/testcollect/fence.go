package testcollect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/atomicfile"
)

// The reset fence: what `forge test collect --reset` excludes from every
// later collect of one workload.
//
// A reset starts a run's coverage from zero, and the rule is:
//
//   - Live counters are baselined (baseline.go): each reachable process's
//     counts at the reset are recorded and subtracted from what it later
//     reports, pulled live or flushed when it exits.
//   - Flushed counters are fenced off by IDENTITY: every counter file present
//     at the moment of the reset is recorded in
//     .forge/tests/covdata/<env>/.reset/<workload> as (source, name, sha256 of
//     its bytes), and a later collect leaves a file out only when all three
//     match an entry. The source is where the file was read: the workload's
//     host GOCOVERDIR, or one compose container. This includes the file of a
//     process that exited before the reset and was never collected: it counts
//     the time before the run began.
//   - Anything else is collected. Go names a counter file
//     covcounters.<build>.<pid>.<nanos>, so two processes or containers can
//     write files of one name (pid 1 in every container); a file that shares a
//     fenced file's name but comes from another source, or holds other bytes,
//     is a different file and is kept. A name's encoded timestamp is never
//     compared with anything, so no clock can drop a file the reset did not
//     see.
//   - The fence only grows. A later reset adds what it sees to what is already
//     there, so a file excluded once is never re-included.
//   - A reset that zeroed nothing (refused, or nothing answered) writes nothing,
//     so the next collect keeps everything it would have kept before.
//
// The files are listed after the baseline pull. A process that exits in
// between leaves a file, which is then excluded whole: the reset errs towards
// counting nothing from before it.

// fenceSchema names the fence file's format.
const fenceSchema = "forge.test.collect.reset/v1"

// fence is the reset fence file.
type fence struct {
	Schema string `json:"schema"`
	// ResetAt is when the newest reset ran. It is a record for people, never
	// a cut-off.
	ResetAt time.Time `json:"reset_at"`
	// Entries are the counter files present at a reset, sorted.
	Entries []fenceEntry `json:"entries,omitempty"`
	// Excluded is the first version's form: bare names, fencing a file of
	// that name from any source, with any bytes. It is still read (and kept
	// on rewrite), so a fence written before entries existed keeps excluding
	// what it excluded; nothing writes a new name here.
	Excluded []string `json:"excluded,omitempty"`
}

// fenceEntry is one fenced counter file.
type fenceEntry struct {
	// Source is where the file was read: "dir:<path relative to the project>"
	// for a host GOCOVERDIR, "container:<id>" for a compose container.
	Source string `json:"source"`
	Name   string `json:"name"`
	// SHA256 is the hex sha256 of the file's bytes.
	SHA256 string `json:"sha256"`
}

func (e fenceEntry) less(o fenceEntry) int {
	return strings.Compare(e.Source+"\x00"+e.Name+"\x00"+e.SHA256, o.Source+"\x00"+o.Name+"\x00"+o.SHA256)
}

// fenceSet is a fence read for lookups. The zero value fences nothing.
type fenceSet struct {
	entries map[fenceEntry]bool
	legacy  map[string]bool
}

// excludes reports whether the fence leaves out the counter file f read from
// source. Meta files are never fenced: they carry no counts.
func (s fenceSet) excludes(source string, f covFile) bool {
	if f.meta {
		return false
	}
	if s.legacy[f.name] {
		return true
	}
	return s.entries[entryOf(source, f)]
}

func entryOf(source string, f covFile) fenceEntry {
	sum := sha256.Sum256(f.data)
	return fenceEntry{Source: source, Name: f.name, SHA256: hex.EncodeToString(sum[:])}
}

// dirSource and containerSource name the sources a fence entry records.
func dirSource(projectRoot, dir string) string { return "dir:" + rel(projectRoot, dir) }
func containerSource(id string) string         { return "container:" + id }

// readFence reads the workload's fence; the zero set when no reset has run.
func readFence(path string) (fenceSet, fence, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fenceSet{}, fence{}, nil
	}
	if err != nil {
		return fenceSet{}, fence{}, err
	}
	var f fence
	if err := json.Unmarshal(b, &f); err != nil || f.Schema != fenceSchema {
		return fenceSet{}, fence{}, fmt.Errorf("%s is not a %s reset fence", path, fenceSchema)
	}
	s := fenceSet{entries: make(map[fenceEntry]bool, len(f.Entries)), legacy: make(map[string]bool, len(f.Excluded))}
	for _, e := range f.Entries {
		s.entries[e] = true
	}
	for _, n := range f.Excluded {
		s.legacy[n] = true
	}
	return s, f, nil
}

// writeFence adds the counter files seen at a reset to the workload's fence,
// keeping every entry already in it, and stamps the reset's time.
func writeFence(path string, at time.Time, seen []fenceEntry) error {
	_, prev, err := readFence(path)
	if err != nil {
		// An unreadable fence is replaced: its entries cannot be known, and
		// the files present now (which include them) are recorded below.
		prev = fence{}
	}
	all := append(slices.Clone(prev.Entries), seen...)
	slices.SortFunc(all, fenceEntry.less)
	all = slices.Compact(all)
	legacy := slices.Clone(prev.Excluded)
	slices.Sort(legacy)
	legacy = slices.Compact(legacy)
	b, err := json.MarshalIndent(fence{Schema: fenceSchema, ResetAt: at.UTC(), Entries: all, Excluded: legacy}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, append(b, '\n'), 0o644)
}

// fenceOut drops the counter files read from source that the fence excludes.
func fenceOut(files []covFile, s fenceSet, source string) []covFile {
	if len(s.entries) == 0 && len(s.legacy) == 0 {
		return files
	}
	out := files[:0:0]
	for _, f := range files {
		if !s.excludes(source, f) {
			out = append(out, f)
		}
	}
	return out
}

// fenceEntries are the fence entries of the counter files among files, all
// read from source.
func fenceEntries(source string, files []covFile) []fenceEntry {
	var out []fenceEntry
	for _, f := range files {
		if !f.meta {
			out = append(out, entryOf(source, f))
		}
	}
	return out
}
