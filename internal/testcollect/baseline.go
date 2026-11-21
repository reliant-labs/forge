package testcollect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/atomicfile"
)

// The reset baseline: how `forge test collect --reset` starts a live
// process's counts from zero without asking the process to change anything.
//
// serverkit's /debug/coverage is read-only (a counter-clearing endpoint is a
// write no auth can protect on a debug listener a web page can reach), so a
// reset pulls each live process's counters as they stand and records them,
// per process, as its baseline. Every later collect subtracts the baseline
// from that process's counts, block by block, clamped at zero. A
// -covermode=atomic counter only ever grows while its process runs, so what
// is left is exactly what the process counted since the reset.
//
// A process is (scope, build, pid): the workload's host processes share the
// scope "host" (a live pull and the file the same process flushes at exit
// are one process, told apart from others by pid), and a cluster pod is its
// own scope, "pod:<ns>/<name>". The build is the covmeta hash.
//
// The baseline applies only to the process it was taken from. A counter file
// with no baseline (a process started after the reset: another pid, another
// pod, a rebuild) is counted whole. So is one whose counts fell below the
// baseline in any block: that is a new process that reused the pid (pid 1
// after a container restart), whose counters started again from zero.
//
// Counter files flushed before the reset are left out by the fence
// (fence.go), not by the baseline. Compose has no live pull, so its reset is
// the fence alone.

// baselineSchema names the baseline file's format.
const baselineSchema = "forge.test.collect.baseline/v1"

// SinceResetProfile is the file a collect writes, beside a build's covdata,
// when it subtracted a baseline: the build's counts since the reset, as a Go
// text profile. It is the lane's coverage input in place of the covdata,
// which is kept as pulled.
const SinceResetProfile = "since-reset.out"

// baseline is the baseline file of one workload.
type baseline struct {
	Schema    string            `json:"schema"`
	TakenAt   time.Time         `json:"taken_at"`
	Processes []baselineProcess `json:"processes"`
}

// baselineProcess is one process's counts at the reset.
type baselineProcess struct {
	Scope    string `json:"scope"`
	MetaHash string `json:"meta_hash"`
	PID      string `json:"pid"`
	// Profile is the process's counters at the reset, as `go tool covdata
	// textfmt` prints them.
	Profile string `json:"profile"`
}

type processKey struct{ scope, hash, pid string }

func (p baselineProcess) key() processKey { return processKey{p.Scope, p.MetaHash, p.PID} }

// BaselineFile is the baseline of one workload:
// .forge/tests/covdata/<env>/.reset/<workload>.baseline.json, beside its
// fence (a workload name has no '.', so the two cannot collide).
func BaselineFile(projectRoot, env, workload string) string {
	return ResetMarker(projectRoot, env, workload) + ".baseline.json"
}

// Scopes a counter file's process is told apart in.
const hostScope = "host"

func podScope(namespace, pod string) string { return "pod:" + namespace + "/" + pod }

// readBaseline reads a workload's baseline; empty when no reset took one.
func readBaseline(path string) (map[processKey]baselineProcess, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f baseline
	if err := json.Unmarshal(b, &f); err != nil || f.Schema != baselineSchema {
		return nil, fmt.Errorf("%s is not a %s reset baseline", path, baselineSchema)
	}
	out := make(map[processKey]baselineProcess, len(f.Processes))
	for _, p := range f.Processes {
		out[p.key()] = p
	}
	return out, nil
}

// writeBaseline records the processes' counts, replacing the baseline of a
// process seen again and keeping the others (a pod the reset could not reach
// keeps the baseline an earlier reset took).
func writeBaseline(path string, at time.Time, taken []baselineProcess) error {
	prev, err := readBaseline(path)
	if err != nil {
		prev = nil // unreadable: replaced by what this reset took
	}
	all := map[processKey]baselineProcess{}
	for k, p := range prev {
		all[k] = p
	}
	for _, p := range taken {
		all[p.key()] = p
	}
	procs := make([]baselineProcess, 0, len(all))
	for _, p := range all {
		procs = append(procs, p)
	}
	sort.Slice(procs, func(i, j int) bool {
		a, b := procs[i], procs[j]
		return a.Scope+"\x00"+a.MetaHash+"\x00"+a.PID < b.Scope+"\x00"+b.MetaHash+"\x00"+b.PID
	})
	data, err := json.Marshal(baseline{Schema: baselineSchema, TakenAt: at.UTC(), Processes: procs})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, append(data, '\n'), 0o644)
}

// counterPID is the pid in a counter file's name.
func counterPID(name string) string {
	parts := strings.Split(strings.TrimPrefix(name, counterPrefix), ".")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// processProfile converts one counter file, alone with its build's meta file,
// to a text profile: one process's counts.
func processProfile(ctx context.Context, opts Options, hash string, meta []byte, c covFile) ([]byte, error) {
	dir, err := os.MkdirTemp("", "forge-collect-process-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := atomicfile.WriteFile(filepath.Join(dir, metaPrefix+hash), meta, 0o644); err != nil {
		return nil, err
	}
	if err := atomicfile.WriteFile(filepath.Join(dir, c.name), c.data, 0o644); err != nil {
		return nil, err
	}
	builds, err := opts.Convert(ctx, "", dir)
	if err != nil {
		return nil, err
	}
	for _, b := range builds {
		if b.MetaHash == hash {
			return b.Profile, nil
		}
	}
	return nil, fmt.Errorf("converting %s produced no profile of build %s", c.name, short(hash))
}

// takeBaselines converts each live counter file in files to the baseline of
// its process.
func takeBaselines(ctx context.Context, opts Options, scope string, files []covFile) ([]baselineProcess, error) {
	metas := map[string][]byte{}
	for _, f := range files {
		if f.meta {
			metas[f.hash] = f.data
		}
	}
	var out []baselineProcess
	for _, f := range files {
		if f.meta {
			continue
		}
		meta, ok := metas[f.hash]
		if !ok {
			return nil, fmt.Errorf("counter file %s came without its build's meta file", f.name)
		}
		prof, err := processProfile(ctx, opts, f.hash, meta, f)
		if err != nil {
			return nil, err
		}
		out = append(out, baselineProcess{Scope: scope, MetaHash: f.hash, PID: counterPID(f.name), Profile: string(prof)})
	}
	return out, nil
}

// profile is a parsed Go text profile: its mode and each block's statements
// and count, keyed by the block ("file:sl.sc,el.ec"), in the order read.
type profile struct {
	mode   string
	order  []string
	blocks map[string]profBlock
}

type profBlock struct {
	stmts int
	count int64
}

// parseProfile reads a Go text profile. A block repeated (textfmt over
// several counter files) is summed.
func parseProfile(data []byte) (profile, error) {
	p := profile{blocks: map[string]profBlock{}}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m, ok := strings.CutPrefix(line, "mode: "); ok && p.mode == "" {
			p.mode = m
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return profile{}, fmt.Errorf("profile line %d: %q is not <block> <statements> <count>", i+1, line)
		}
		stmts, err1 := strconv.Atoi(f[1])
		count, err2 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil {
			return profile{}, fmt.Errorf("profile line %d: %q is not <block> <statements> <count>", i+1, line)
		}
		b, seen := p.blocks[f[0]]
		if !seen {
			p.order = append(p.order, f[0])
		}
		p.blocks[f[0]] = profBlock{stmts: stmts, count: b.count + count}
	}
	if p.mode == "" {
		return profile{}, errors.New("profile has no mode line")
	}
	return p, nil
}

func (p profile) bytes() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "mode: %s\n", p.mode)
	for _, k := range p.order {
		fmt.Fprintf(&b, "%s %d %d\n", k, p.blocks[k].stmts, p.blocks[k].count)
	}
	return b.Bytes()
}

// below reports whether any block of base counted more than live did: live
// is then not the process base was taken from.
func below(live, base profile) bool {
	for k, b := range base.blocks {
		if live.blocks[k].count < b.count {
			return true
		}
	}
	return false
}

// subtract takes base's counts off p, block by block, clamped at zero.
func (p profile) subtract(base profile) {
	for k, b := range base.blocks {
		if cur, ok := p.blocks[k]; ok {
			cur.count = max(cur.count-b.count, 0)
			p.blocks[k] = cur
		}
	}
}

// sinceReset is a build's counts since the reset, when a baseline applied to
// any of its processes: its full profile less each applicable baseline.
// subtracted counts those processes; restarted names the counter files whose
// baseline did not apply because the process had restarted.
func sinceReset(ctx context.Context, opts Options, b *build, dir string, bases map[processKey]baselineProcess) (out []byte, subtracted int, restarted []string, err error) {
	if len(bases) == 0 {
		return nil, 0, nil, nil
	}
	var apply []profile
	for _, c := range b.counters {
		base, ok := bases[processKey{c.scope, b.hash, counterPID(c.name)}]
		if !ok || c.scope == "" {
			continue
		}
		bp, err := parseProfile([]byte(base.Profile))
		if err != nil {
			return nil, 0, nil, fmt.Errorf("baseline of %s: %w", c.name, err)
		}
		lp, err := processProfile(ctx, opts, b.hash, b.meta, c)
		if err != nil {
			return nil, 0, nil, err
		}
		live, err := parseProfile(lp)
		if err != nil {
			return nil, 0, nil, err
		}
		if below(live, bp) {
			restarted = append(restarted, c.name)
			continue
		}
		apply = append(apply, bp)
	}
	if len(apply) == 0 {
		return nil, 0, restarted, nil
	}
	builds, err := opts.Convert(ctx, "", dir)
	if err != nil {
		return nil, 0, nil, err
	}
	var full []byte
	for _, cb := range builds {
		if cb.MetaHash == b.hash {
			full = cb.Profile
		}
	}
	if full == nil {
		return nil, 0, nil, fmt.Errorf("converting the build produced no profile of %s", short(b.hash))
	}
	p, err := parseProfile(full)
	if err != nil {
		return nil, 0, nil, err
	}
	for _, bp := range apply {
		p.subtract(bp)
	}
	return p.bytes(), len(apply), restarted, nil
}
