// Package storage owns host storage budgets and the lifecycle of explicitly
// registered local development caches. Persistent application data is never GC'd.
//
//forge:exclude-contract: local CLI maintenance with explicit policy and process adapters, not an injected application component
package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GiB is one gibibyte in bytes.
const GiB = uint64(1 << 30)

// Policy defines one machine’s cache budgets and protected references.
type Policy struct {
	Projects         []string   `json:"projects,omitempty"`
	LogBudgetGiB     uint64     `json:"log_budget_gib"`
	HostReserveGiB   uint64     `json:"host_reserve_gib"`
	HostPaths        []string   `json:"host_paths,omitempty"`
	BuildCacheGiB    uint64     `json:"build_cache_gib"`
	BuildCacheUnused string     `json:"build_cache_unused"`
	ImageUnused      string     `json:"image_unused"`
	RegistryDays     int        `json:"registry_days"`
	RegistryKeep     int        `json:"registry_keep"`
	DockerContext    string     `json:"docker_context,omitempty"`
	Builders         []string   `json:"builders"`
	Registries       []Registry `json:"registries,omitempty"`
	Clusters         []string   `json:"clusters,omitempty"`
	Pins             []string   `json:"pins,omitempty"`
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
	return Policy{LogBudgetGiB: 1, HostReserveGiB: 20, BuildCacheGiB: 20, BuildCacheUnused: "168h", ImageUnused: "168h", RegistryDays: 14, RegistryKeep: 5, Builders: []string{"default"}}
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
	for _, v := range []string{p.ImageUnused, p.BuildCacheUnused} {
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

// Save atomically replaces a validated policy file.
func Save(path string, p Policy) error {
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

// CheckSpace refuses builds that would start below the physical host reserve.
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
		d, err := DiskSpace(path)
		if err != nil {
			return disks, fmt.Errorf("host disk %s: %w", path, err)
		}
		disks = append(disks, d)
		if d.Available < p.HostReserveGiB*GiB {
			return disks, fmt.Errorf("host disk %s has %.1f GiB free, below the %d GiB reserve; run 'forge storage status' and 'forge storage gc --apply' before building (persistent volumes are never automatically deleted)", path, float64(d.Available)/float64(GiB), p.HostReserveGiB)
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
