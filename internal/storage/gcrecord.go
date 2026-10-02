package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Two records beside the policy say what maintenance has actually done.
//
//   - last-full-gc.json is written by every APPLIED full pass (`forge storage
//     gc --apply` and the scheduled job, which runs that command). It records
//     whether the pass succeeded, and which layers failed. This is the only
//     evidence that registry retention, the layer that reclaims the most, is
//     running at all.
//   - last-auto-gc.json is the opportunistic pass's own attempt record. It
//     rate-limits that pass to once per interval, and that is all it is for.
//
// They used to be one stamp, last-gc.json, which the opportunistic pass
// advanced even when it failed and even though it never runs the registry
// layer. `forge doctor` then reported "GC scheduled, last ran 2h ago" on a
// machine where registry retention had never once succeeded.

// GCResult is the outcome of one maintenance pass.
type GCResult struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
	// FailedLayers names each layer that failed: "logs", "temp sweep",
	// "source cache", "builder <name>", "registry <container>", "docker".
	FailedLayers []string `json:"failed_layers,omitempty"`
	// Error is the combined error text, for a human reading the record.
	Error string `json:"error,omitempty"`
}

// LayerError attributes a failure to the maintenance layer it came from, so a
// result can name the layers that failed without parsing error text.
type LayerError struct {
	Layer string
	Err   error
}

func (e *LayerError) Error() string { return e.Layer + ": " + e.Err.Error() }
func (e *LayerError) Unwrap() error { return e.Err }

func layerErr(layer string, err error) error {
	if err == nil {
		return nil
	}
	return &LayerError{Layer: layer, Err: err}
}

// NewGCResult summarises a pass's error into a record.
func NewGCResult(at time.Time, err error) GCResult {
	r := GCResult{At: at, OK: err == nil}
	if err == nil {
		return r
	}
	r.Error = err.Error()
	seen := map[string]bool{}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		// A joined error is descended FIRST: errors.As on it would stop at
		// the first layer and never name the rest.
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			for _, inner := range joined.Unwrap() {
				walk(inner)
			}
			return
		}
		var layer *LayerError
		if errors.As(e, &layer) && layer != nil && !seen[layer.Layer] {
			seen[layer.Layer] = true
			r.FailedLayers = append(r.FailedLayers, layer.Layer)
		}
	}
	walk(err)
	sort.Strings(r.FailedLayers)
	return r
}

// Summary is one line naming what failed, for `env up` and `doctor`.
func (r GCResult) Summary() string {
	if r.OK {
		return "succeeded"
	}
	if len(r.FailedLayers) > 0 {
		return "failed in " + strings.Join(r.FailedLayers, ", ")
	}
	return "failed"
}

// FullGCRecordPath is the full pass's record, beside the policy.
func FullGCRecordPath(policyPath string) string {
	return filepath.Join(filepath.Dir(policyPath), "last-full-gc.json")
}

// AutoGCRecordPath is the opportunistic pass's attempt record.
func AutoGCRecordPath(policyPath string) string {
	return filepath.Join(filepath.Dir(policyPath), "last-auto-gc.json")
}

// RecordFullGC records an applied full pass.
func RecordFullGC(policyPath string, r GCResult) error {
	return writeRecord(policyPath, FullGCRecordPath(policyPath), r)
}

// RecordAutoGC records an opportunistic pass's attempt.
func RecordAutoGC(policyPath string, r GCResult) error {
	return writeRecord(policyPath, AutoGCRecordPath(policyPath), r)
}

// LastFullGC reads the full pass's record. ok is false when there is none, or
// it cannot be read: "never ran" and "cannot tell" lead to the same advice.
func LastFullGC(policyPath string) (GCResult, bool) {
	return readRecord(FullGCRecordPath(policyPath))
}

// LastAutoGC reads the opportunistic pass's attempt record.
func LastAutoGC(policyPath string) (GCResult, bool) {
	return readRecord(AutoGCRecordPath(policyPath))
}

// FullGCStaleAfter is when a successful full pass stops being evidence that
// retention is running: the schedule is daily, so two days means it is not
// firing.
const FullGCStaleAfter = 48 * time.Hour

// FullGCProblem reports, in one sentence, why registry retention is not known
// to be working, or "" if it is. It applies only when registries are
// registered: a machine with none has nothing that only the full pass can
// reclaim. Doctor and `forge env up` both read it, so they cannot disagree.
func FullGCProblem(p Policy, last GCResult, ok bool, now time.Time) string {
	if len(p.Registries) == 0 {
		return ""
	}
	switch {
	case !ok:
		return fmt.Sprintf("%d local registr(ies) registered, but no full storage GC has ever completed", len(p.Registries))
	case !last.OK:
		return fmt.Sprintf("the last full storage GC (%s ago) %s", ageText(now.Sub(last.At)), last.Summary())
	case now.Sub(last.At) > FullGCStaleAfter:
		return fmt.Sprintf("the last full storage GC was %s ago; the daily pass is not running", ageText(now.Sub(last.At)))
	}
	return ""
}

func ageText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return fmt.Sprintf("%dh", int(d/time.Hour))
}

func writeRecord(policyPath, path string, r GCResult) error {
	if err := guardMachinePolicy(policyPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gc-record-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func readRecord(path string) (GCResult, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return GCResult{}, false
	}
	var r GCResult
	if json.Unmarshal(b, &r) != nil || r.At.IsZero() {
		return GCResult{}, false
	}
	return r, true
}
