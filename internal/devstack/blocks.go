// forge:exclude-contract
// devstack is CLI-internal dev-stack orchestration glue (dev-block wiring,
// git-facts, lockfile) for `forge up`, not a contract-shaped service the
// bootstrap wires. Opt out of the require-contract rule.
package devstack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// registryRel is the on-disk port-block registry: a stable {key: block}
// map. The block is the INTERNAL index forge multiplies by 100 to offset a
// stack's ports; it is never surfaced in KCL. The default key "" is
// implicitly block 0 and is NOT stored — only named keys consume registry
// slots, starting at 1, so the default stack's port block (base + 0*100 =
// base) is never displaced. Machine-local; .forge/* is gitignored.
const registryRel = ".forge/blocks.json"

// AllocatePort is the engine behind the forge.allocate_port(base, key) KCL
// builtin. It returns base + block(key)*100, where block(key) is the small
// integer forge assigns the FIRST time it sees key and MEMOIZES in the
// lock-guarded registry. The block index is INTERNAL — it never surfaces in
// KCL; KCL only ever sees the final port.
//
// Semantics (the contract):
//   - key == "" ⇒ block 0 ⇒ returns base UNCHANGED, with no registry/lock
//     touch (the byte-identical default-stack path).
//   - One block PER KEY: every allocate_port(*, key) call for the same key
//     shares that key's block, so all of a stack's ports shift by the SAME
//     offset.
//   - DETERMINISTIC: base + block*100, NO availability stepping. A port that
//     must equal an externally-fixed value (a k3d pre-mapped host port; the
//     host reliant's LISTEN port) must never step off a held port, so up and
//     deploy — and the external mapping — always agree.
//
// The registry read-modify-write happens entirely under the file lock, so a
// concurrent first-`up` of two worktrees cannot race two keys to the same
// block. Persistence makes the block stable across runs AND identical under
// both `forge up` and `forge deploy` (both call this through the same
// builtin), which is the permanent up-vs-deploy port fix.
func AllocatePort(projectDir string, base int, key string) (int, error) {
	block, err := AllocateBlock(projectDir, key)
	if err != nil {
		return 0, err
	}
	return base + block*100, nil
}

// AllocateBlock returns the stable block index for key, assigning the next
// free one (≥1) on first use. key == "" is block 0 (the default stack) and
// is never stored or locked. Atomic under the registry lock.
func AllocateBlock(projectDir, key string) (int, error) {
	if key == "" {
		return 0, nil
	}
	var block int
	err := withLock(projectDir, func() error {
		reg, err := readRegistry(projectDir)
		if err != nil {
			return err
		}
		if existing, ok := reg[key]; ok {
			block = existing
			return nil
		}
		block = nextFreeBlock(reg)
		reg[key] = block
		return writeRegistry(projectDir, reg)
	})
	if err != nil {
		return 0, err
	}
	return block, nil
}

// nextFreeBlock returns the lowest unused block >= 1 (0 is reserved for the
// default key ""). Filling gaps left by removed keys keeps blocks — and thus
// the derived port offsets — small and dense.
func nextFreeBlock(reg map[string]int) int {
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

// readRegistry loads the {key: block} map. A missing file is an empty
// registry (the first-ever named key). A corrupt file is an error —
// silently discarding it would re-assign blocks already in use by a live
// stack, colliding ports.
func readRegistry(projectDir string) (map[string]int, error) {
	data, err := os.ReadFile(registryPath(projectDir))
	if os.IsNotExist(err) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read block registry: %w", err)
	}
	reg := map[string]int{}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse block registry %s: %w", registryPath(projectDir), err)
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
	// already serializes writers; this guards readers that don't lock.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".blocks-*.tmp")
	if err != nil {
		return fmt.Errorf("write block registry: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write block registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write block registry: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace block registry: %w", err)
	}
	return nil
}

// Block is one {key: block} registry entry, for diagnostics.
type Block struct {
	Key   string
	Index int
}

// List returns the block registry sorted by block index — for diagnostics /
// a future `forge stacks` command.
func List(projectDir string) ([]Block, error) {
	reg, err := readRegistry(projectDir)
	if err != nil {
		return nil, err
	}
	out := make([]Block, 0, len(reg))
	for key, block := range reg {
		out = append(out, Block{Key: key, Index: block})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out, nil
}
