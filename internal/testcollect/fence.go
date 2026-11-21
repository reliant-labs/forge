package testcollect

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/reliant-labs/forge/internal/atomicfile"
)

// The reset fence: what `forge test collect --reset` excludes from every
// later collect of one workload.
//
// A reset starts a run's coverage from zero, and the rule is:
//
//   - Live counters are zeroed in the process: each reachable process gets the
//     reset POST, so whatever it counts afterwards, pulled live or flushed when
//     it exits, begins at zero.
//   - Flushed counters are fenced off by NAME. Every counter file in the
//     workload's GOCOVERDIR (for compose, the container's) at the moment of the
//     reset is recorded in .forge/tests/covdata/<env>/.reset/<workload>, and
//     every later collect leaves out exactly those files. That includes the
//     file of a process that exited before the reset and was never collected:
//     it counts the time before the run began.
//   - A file that was not there at the reset is always collected: a process
//     that exits after it, or a pull after it. A name's encoded timestamp is
//     never compared with anything, so no clock (a container's, a skewed one)
//     can drop a file the reset did not see.
//   - The fence only grows. A later reset adds the names present then to the
//     ones already excluded, so a file excluded once is never re-included.
//   - A reset that zeroed nothing (refused, or nothing answered) writes nothing,
//     so the next collect keeps everything it would have kept before.
//
// The names are listed after the reset POST. A process that exits in between
// leaves a file holding only what it counted since the reset, which is then
// excluded too: the reset errs towards counting nothing from before it.

// fenceSchema names the fence file's format.
const fenceSchema = "forge.test.collect.reset/v1"

// fence is the reset fence file.
type fence struct {
	Schema string `json:"schema"`
	// ResetAt is when the newest reset ran. It is a record for people, never
	// a cut-off: exclusion is by name.
	ResetAt time.Time `json:"reset_at"`
	// Excluded are the counter-file names flushed before a reset, sorted.
	Excluded []string `json:"excluded"`
}

// readFence returns the names the workload's fence excludes; nil when no
// reset has run.
func readFence(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f fence
	if err := json.Unmarshal(b, &f); err != nil || f.Schema != fenceSchema {
		return nil, fmt.Errorf("%s is not a %s reset fence", path, fenceSchema)
	}
	out := make(map[string]bool, len(f.Excluded))
	for _, n := range f.Excluded {
		out[n] = true
	}
	return out, nil
}

// writeFence adds names to the workload's fence, keeping every name already
// in it, and stamps the reset's time.
func writeFence(path string, at time.Time, names []string) error {
	prev, err := readFence(path)
	if err != nil {
		// An unreadable fence is replaced: its names cannot be known, and
		// the files present now (which include them) are recorded below.
		prev = nil
	}
	all := slices.Clone(names)
	for n := range prev {
		all = append(all, n)
	}
	slices.Sort(all)
	all = slices.Compact(all)
	if all == nil {
		all = []string{}
	}
	b, err := json.MarshalIndent(fence{Schema: fenceSchema, ResetAt: at.UTC(), Excluded: all}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, append(b, '\n'), 0o644)
}

// fenceOut drops the counter files the fence excludes.
func fenceOut(files []covFile, excluded map[string]bool) []covFile {
	if len(excluded) == 0 {
		return files
	}
	out := files[:0:0]
	for _, f := range files {
		if f.meta || !excluded[f.name] {
			out = append(out, f)
		}
	}
	return out
}

// counterNames are the names of the counter files among files.
func counterNames(files []covFile) []string {
	var out []string
	for _, f := range files {
		if !f.meta {
			out = append(out, f.name)
		}
	}
	return out
}

// dirCounterNames lists the counter-file names directly in dir, without
// reading them. A missing dir has none.
func dirCounterNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if f, ok := parseCovName(e.Name()); ok && !f.meta && e.Type().IsRegular() {
			out = append(out, f.name)
		}
	}
	return out, nil
}
