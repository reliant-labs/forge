// Package storage owns host storage budgets and the lifecycle of explicitly
// registered local development caches. Persistent application data is never GC'd.
//
//forge:exclude-contract: local CLI maintenance with explicit policy and process adapters, not an injected application component
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GiB is one gibibyte in bytes.
const GiB = uint64(1 << 30)

// Policy defines one machine’s cache budgets and protected references.
type Policy struct {
	Projects         []string `json:"projects,omitempty"`
	LogBudgetGiB     uint64   `json:"log_budget_gib"`
	HostReserveGiB   uint64   `json:"host_reserve_gib"`
	HostPaths        []string `json:"host_paths,omitempty"`
	BuildCacheGiB    uint64   `json:"build_cache_gib"`
	BuildCacheUnused string   `json:"build_cache_unused"`
	ImageUnused      string   `json:"image_unused"`
	// SourceCacheUnused is how long a cross-repo source clone may go
	// unresolved before it becomes an eviction candidate. Longer than the
	// image and build-cache windows because the cost of being wrong is a
	// full re-clone of a repository, not a re-pull of a layer.
	SourceCacheUnused string `json:"source_cache_unused"`
	// SourceCacheKeep is how many of the most recently used clones of one
	// repository are retained regardless of age. This is the floor that
	// keeps an idle project's current pin warm, so it is validated at 1 or
	// more: a policy that can empty a repository's only pin is not a policy
	// anyone wants by accident.
	SourceCacheKeep int        `json:"source_cache_keep"`
	RegistryDays    int        `json:"registry_days"`
	RegistryKeep    int        `json:"registry_keep"`
	DockerContext   string     `json:"docker_context,omitempty"`
	Builders        []string   `json:"builders"`
	Registries      []Registry `json:"registries,omitempty"`
	Clusters        []string   `json:"clusters,omitempty"`
	Pins            []string   `json:"pins,omitempty"`

	// Repos are git repositories whose linked worktrees the worktree layer
	// reclaims, beyond the repository of each registered project: sibling
	// repos a project builds from (docker.build_contexts, go.mod replaces).
	// Filled by Converge; git itself enumerates the worktrees, wherever they live.
	Repos []string `json:"repos,omitempty"`
	// WorktreeRebuildable replaces the built-in allowlist of ignored paths a
	// removable worktree may contain. Nil means the defaults.
	WorktreeRebuildable []string `json:"worktree_rebuildable,omitempty"`
	// WorktreeReap lets GC --apply remove worktrees. Off, the layer only reports.
	WorktreeReap bool `json:"worktree_reap,omitempty"`
}

// Registry explicitly identifies a local registry and all of its consumers.
type Registry struct {
	Container    string   `json:"container"`
	Repositories []string `json:"repositories"`
	Aliases      []string `json:"aliases"`
	Contexts     []string `json:"contexts"`
}

// DefaultPolicy returns conservative local development budgets.
func DefaultPolicy() Policy {
	return Policy{LogBudgetGiB: 1, HostReserveGiB: 20, BuildCacheGiB: 20, BuildCacheUnused: "168h", ImageUnused: "168h", SourceCacheUnused: "336h", SourceCacheKeep: 2, RegistryDays: 14, RegistryKeep: 5, Builders: []string{"default"}}
}

// DefaultPath resolves the machine policy location.
func DefaultPath() (string, error) {
	if path := os.Getenv("FORGE_STORAGE_POLICY"); path != "" {
		return filepath.Abs(path)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "forge", "storage.json"), nil
}

// Load overlays a strict JSON document on the default policy.
func Load(path string) (Policy, error) {
	p := DefaultPolicy()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, fmt.Errorf("storage policy: %w", err)
	}
	return p, p.Validate()
}

// Validate rejects unbounded budgets and unscoped registry targets.
func (p Policy) Validate() error {
	if p.LogBudgetGiB == 0 || p.HostReserveGiB == 0 || p.BuildCacheGiB == 0 || p.RegistryDays < 1 || p.RegistryKeep < 2 {
		return fmt.Errorf("storage: positive budgets and retention, and at least two registry versions are required")
	}
	if p.SourceCacheKeep < 1 {
		return fmt.Errorf("storage: source_cache_keep must retain at least one clone per repository, got %d", p.SourceCacheKeep)
	}
	for _, v := range []string{p.ImageUnused, p.BuildCacheUnused, p.SourceCacheUnused} {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Hour {
			return fmt.Errorf("storage: invalid unused duration %q (minimum 1h)", v)
		}
	}
	for _, r := range p.Registries {
		if !strings.HasPrefix(r.Container, "k3d-") || len(r.Contexts) == 0 || len(r.Repositories) == 0 || len(r.Aliases) == 0 {
			return fmt.Errorf("registry %q requires explicit repositories, aliases, and protecting cluster contexts", r.Container)
		}
		for _, c := range r.Contexts {
			if !strings.HasPrefix(c, "k3d-") {
				return fmt.Errorf("registry retention only accepts local k3d contexts: %s", c)
			}
		}
	}
	return nil
}

// ErrMachinePolicyUnderTest refuses a write to the developer's real machine
// policy from inside `go test`. Callers on a warn-never-fail path (the
// zero-touch converge hooks, builder registration, release pinning) treat it
// as "skip": a test has nothing real to register.
var ErrMachinePolicyUnderTest = errors.New("storage: refusing to write the machine storage policy under test " +
	"(set FORGE_STORAGE_POLICY to a t.TempDir() path to exercise policy writes)")

// guardMachinePolicy refuses path when it is the machine-default policy and
// the process is a test binary that did not name it explicitly.
//
// The zero-touch activation (converge.go) makes `forge build`, a release cut
// and the cluster phase WRITE the policy as a side effect. Under `go test`
// those paths are exercised constantly by tests that are about something else
// entirely, and each one wrote the developer's real storage.json — measured:
// 65 leaked `TestBuildTag_*/001` temp projects and fake pins in one machine's
// policy, and Policy.Projects is the set the Logs layer expires files under.
// This is the same hole TempRoot and SourceCacheRoot close (tempsweep.go,
// sources.go), closed the same way: the default is unreachable from a test, so
// a silent write becomes a message naming what to set.
//
// FORGE_STORAGE_POLICY set explicitly is a deliberate scope and is honoured —
// DefaultPath returns it, so it is never "the default" here. Reads are not
// guarded: loading the policy cannot change the machine.
func guardMachinePolicy(path string) error {
	if !testing.Testing() || os.Getenv("FORGE_STORAGE_POLICY") != "" {
		return nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		// No resolvable machine policy location means path cannot be it.
		return nil
	}
	machine := filepath.Join(dir, "forge", "storage.json")
	if absolute, err := filepath.Abs(path); err == nil && filepath.Clean(absolute) == filepath.Clean(machine) {
		return ErrMachinePolicyUnderTest
	}
	return nil
}

// Save atomically replaces a validated policy file.
func Save(path string, p Policy) error {
	if err := guardMachinePolicy(path); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".storage-*.json")
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

// Disk describes physical filesystem capacity.
type Disk struct {
	Path      string `json:"path"`
	Available uint64 `json:"available_bytes"`
	Capacity  uint64 `json:"capacity_bytes"`
}

// ReserveError is a MEASURED shortfall: the filesystem under Path was read and
// holds less than the policy's host reserve. It is a distinct type because
// it is the one CheckSpace outcome a caller may knowingly accept — an
// ephemeral CI runner, whose disk is discarded with the job — while an
// unreadable disk or path is an unknown that no caller should wave through.
type ReserveError struct {
	Path       string
	Available  uint64
	ReserveGiB uint64
}

func (e *ReserveError) Error() string {
	return fmt.Sprintf("host disk %s has %.1f GiB free, below the %d GiB reserve; run 'forge storage status' and 'forge storage gc --apply' before building (persistent volumes are never automatically deleted)", e.Path, float64(e.Available)/float64(GiB), e.ReserveGiB)
}

// CheckSpace refuses builds that would start below the physical host reserve.
// A shortfall is returned as a *ReserveError; any other error means a disk
// could not be measured at all.
func CheckSpace(p Policy, paths ...string) ([]Disk, error) {
	paths = append(paths, p.HostPaths...)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths = append(paths, home)
	var disks []Disk
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		existing, err := existingStoragePath(path)
		if err != nil {
			return disks, fmt.Errorf("host disk %s: %w", path, err)
		}
		d, err := DiskSpace(existing)
		if err != nil {
			return disks, fmt.Errorf("host disk %s: %w", path, err)
		}
		d.Path = path
		disks = append(disks, d)
		if d.Available < p.HostReserveGiB*GiB {
			return disks, &ReserveError{Path: path, Available: d.Available, ReserveGiB: p.HostReserveGiB}
		}
	}
	return disks, nil
}

// KubeletConfig relies on kubelet's own usage tracking and GC, never on deleting
// containerd files underneath it. Unused age is independent of VM disk fullness.
func (p Policy) KubeletConfig() []byte {
	b, _ := json.MarshalIndent(map[string]any{"apiVersion": "kubelet.config.k8s.io/v1beta1", "kind": "KubeletConfiguration", "imageMaximumGCAge": p.ImageUnused, "imageMinimumGCAge": "10m", "imageGCHighThresholdPercent": 80, "imageGCLowThresholdPercent": 70, "containerLogMaxSize": "10Mi", "containerLogMaxFiles": 3}, "", "  ")
	return b
}

// CheckBuildSpace includes build scratch and explicitly configured Go caches,
// which can live on different filesystems from the project and home directory.
func CheckBuildSpace(p Policy, paths ...string) ([]Disk, error) {
	paths = append(paths, os.TempDir())
	for _, name := range []string{"GOTMPDIR", "GOCACHE", "GOMODCACHE"} {
		if path := os.Getenv(name); path != "" && path != "off" {
			paths = append(paths, path)
		}
	}
	return CheckSpace(p, paths...)
}

// A new output/cache directory consumes the filesystem of its nearest existing
// parent. Check that filesystem before the build creates the directory.
func existingStoragePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("no existing parent for %s", path)
		}
		path = parent
	}
}
