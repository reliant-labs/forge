package instance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// registryRel is the on-disk index registry: a stable {name: index} map.
// The default (unnamed) instance is implicitly index 0 and is NOT stored —
// only named instances consume registry slots, starting at 1, so the
// default stack's port/host block (base + 0*100) is never displaced by a
// named worktree. Machine-local; .forge/* is gitignored.
const registryRel = ".forge/instances.json"

// Resolve produces the full instance identity for this command: it derives
// the name (flag → linked-worktree basename, else default) and, for a named
// instance, assigns-or-reads its stable index in the lock-guarded
// registry. A concurrent first-`up` of two worktrees cannot race to the
// same index — the read-modify-write of the registry happens entirely
// under the file lock (see withLock).
//
// The default instance (name "") needs no registry entry and returns
// {"" , 0} without taking the lock, so a plain single-stack `forge up` /
// `forge deploy` pays nothing and behaves exactly as before.
func Resolve(projectDir, flagName string) (Instance, error) {
	name := ResolveName(projectDir, flagName)
	if name == "" {
		return Instance{}, nil
	}
	idx, err := assignIndex(projectDir, name)
	if err != nil {
		return Instance{}, err
	}
	return Instance{Name: name, Index: idx}, nil
}

// assignIndex returns the stable index for name, assigning the next free
// one (>=1) on first use. Atomic under the registry lock.
func assignIndex(projectDir, name string) (int, error) {
	var idx int
	err := withLock(projectDir, func() error {
		reg, err := readRegistry(projectDir)
		if err != nil {
			return err
		}
		if existing, ok := reg[name]; ok {
			idx = existing
			return nil
		}
		idx = nextFreeIndex(reg)
		reg[name] = idx
		return writeRegistry(projectDir, reg)
	})
	if err != nil {
		return 0, err
	}
	return idx, nil
}

// nextFreeIndex returns the lowest unused index >= 1 (0 is reserved for the
// default instance). Filling gaps left by removed instances keeps indices —
// and thus the derived port blocks — small and dense.
func nextFreeIndex(reg map[string]int) int {
	used := make(map[int]bool, len(reg))
	for _, v := range reg {
		used[v] = true
	}
	for i := 1; ; i++ {
		if !used[i] {
			return i
		}
	}
}

func registryPath(projectDir string) string {
	return filepath.Join(projectDir, registryRel)
}

// readRegistry loads the {name: index} map. A missing file is an empty
// registry (the first-ever named instance). A corrupt file is an error —
// silently discarding it would re-assign indices already in use by a live
// stack, colliding ports.
func readRegistry(projectDir string) (map[string]int, error) {
	data, err := os.ReadFile(registryPath(projectDir))
	if os.IsNotExist(err) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read instance registry: %w", err)
	}
	reg := map[string]int{}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse instance registry %s: %w", registryPath(projectDir), err)
	}
	return reg, nil
}

func writeRegistry(projectDir string, reg map[string]int) error {
	p := registryPath(projectDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create .forge dir: %w", err)
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	// Atomic replace: write to a temp file in the same dir, then rename, so
	// a concurrent reader never sees a half-written registry. The lock
	// already serializes writers; this guards readers that don't lock
	// (none today, but cheap insurance).
	tmp, err := os.CreateTemp(filepath.Dir(p), ".instances-*.tmp")
	if err != nil {
		return fmt.Errorf("write instance registry: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write instance registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write instance registry: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace instance registry: %w", err)
	}
	return nil
}

// List returns the registry sorted by index — for diagnostics / a future
// `forge instances` command.
func List(projectDir string) ([]Instance, error) {
	reg, err := readRegistry(projectDir)
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(reg))
	for name, idx := range reg {
		out = append(out, Instance{Name: name, Index: idx})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out, nil
}
