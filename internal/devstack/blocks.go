// forge:exclude-contract
// devstack is CLI-internal dev-stack orchestration glue (dev-block wiring,
// git-facts, lockfile) for `forge env up`, not a contract-shaped service the
// bootstrap wires. Opt out of the require-contract rule.
package devstack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// registryRel is the on-disk port-block registry: a stable {key: entry} map,
// resolved relative to the REPO ANCHOR (the primary checkout) so every linked
// worktree of a repo shares ONE registry — see repoAnchor in gitfacts.go.
// The block is the INTERNAL index forge multiplies by 100 to offset a
// stack's ports; it is never surfaced in KCL. The default key "" is
// implicitly block 0 and is NOT stored — only named keys consume registry
// slots, starting at 1, so the default stack's port block (base + 0*100 =
// base) is never displaced. Machine-local; .forge/* is gitignored.
const registryRel = ".forge/blocks.json"

// entry is one registry record: the port block, plus WHAT KIND of key it is.
//
// Why the kind matters. This registry serves two unrelated purposes that look
// identical once written down as a bare {key: int} map:
//
//  1. DEV-STACK IDENTITY — one key per active git worktree, the roster a
//     per-stack config generator enumerates to emit one config block per
//     running stack.
//  2. PLAIN PORT-BLOCK ALLOCATION — any KCL expression that just wants a
//     non-colliding host port, e.g. prod's reliant-web dev server keying on
//     "prod" so it does not land on dev's :3000.
//
// Conflating them is not hypothetical. control-plane's dev NATS generator
// enumerated the whole registry, assumed every key was a worktree, and
// rendered a `CP_prod` account with `user: "control-plane-prod"` — a dev NATS
// account for a PROD web port — into a TRACKED deploy/nats/nats.conf. That is
// the same file the "prod-" empty-interpolation fragment corrupted (see
// validateKey), and the same file the generate-path purity guard was built to
// revert (internal/cli/kcl_render_purity.go). Three defects, one root cause:
// forge offered no way to ask "which of these keys are actually dev stacks?",
// so enumerating the raw registry was the only move available — and it is
// wrong by construction.
//
// Stack is therefore recorded at ALLOCATION time, where the answer is known
// for certain, rather than guessed later by pattern-matching key names. A
// generator reads the roster through ListStacks / the fp.dev_stacks() builtin
// and never sees a port-block key at all.
type entry struct {
	Block int  `json:"block"`
	Stack bool `json:"stack,omitempty"`
}

// registry is the decoded {key: entry} map.
type registry map[string]entry

// UnmarshalJSON reads BOTH the current object form and the historical bare-int
// form (`{"wt-a": 1}`), which every registry written before the stack flag
// existed is in.
//
// Migrating in place — rather than discarding and re-allocating — is the whole
// point. A block index is not a cache: it is multiplied by 100 into host ports
// that are pre-mapped in k3d's cluster config and baked into an issuer's `iss`
// claim and registered redirect URIs. Re-assigning one silently breaks sign-in
// and cluster ingress on a developer's machine. So a legacy entry keeps its
// exact block and is simply read as a non-stack key; the next `forge env up`
// from its worktree re-marks it as a stack (markStack), which is the same
// moment forge would have learned that fact anyway.
func (r *registry) UnmarshalJSON(data []byte) error {
	// Try the current form first: {"key": {"block": N, "stack": bool}}.
	var typed map[string]entry
	if err := json.Unmarshal(data, &typed); err == nil {
		*r = typed
		return nil
	}
	// Fall back to the legacy form: {"key": N}.
	var legacy map[string]int
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	out := make(registry, len(legacy))
	for key, block := range legacy {
		out[key] = entry{Block: block}
	}
	*r = out
	return nil
}

// validateKey rejects a key that is not a canonical DNS-safe label — the
// form Sanitize produces and the form every consumer of this registry
// assumes.
//
// Why the registry refuses rather than repairs. A KCL author composes a key
// by interpolation ("prod-" + option("worktree")), and when the interpolated
// half is empty the key that arrives is a trailing-dash FRAGMENT rather than
// a name. forge used to accept it and memoize it, at which point
// .forge/blocks.json held an entry indistinguishable from a real stack.
//
// Nothing failed there. It failed two steps downstream, in version control:
// control-plane's per-stack NATS generator enumerates this registry, and the
// "prod-" fragment rendered an account named CP_prod_ with
// `user: "control-plane-prod-"` into a TRACKED deploy/nats/nats.conf. So
// `forge generate` emitted different bytes on a machine that had once run
// `forge env up prod` than on a fresh clone, and CI's Verify Generated Code
// job failed for whoever's machine-local state differed.
//
// The malformed shape is only recognizable HERE. Once a generator reads the
// registry, "prod-" is just a string, and by then the port has been issued.
// Silently sanitizing it would be worse than rejecting: "prod-" and "prod"
// would collapse onto one block, so a key that had already been handed a port
// could move — and a moved port invalidates a k3d host mapping or an issuer's
// baked-in `iss` claim. Every key forge itself supplies (Worktree, Branch) is
// already canonical, so a non-canonical key can only come from a composed KCL
// expression, and naming it is what makes that expression findable.
func validateKey(key string) error {
	if key == "" { // the default stack, block 0
		return nil
	}
	if canonical := Sanitize(key); canonical != key {
		return fmt.Errorf(
			"port-block key %q is not a canonical name (canonical form: %q).\n"+
				"This is almost always an EMPTY INTERPOLATION in a KCL key expression — e.g.\n"+
				"  fp.allocate_port(3000, \"prod-\" + (option(\"worktree\") or \"\"))\n"+
				"which composes the literal \"prod-\" on the primary checkout, where\n"+
				"option(\"worktree\") is \"\".\n"+
				"forge refuses the key rather than recording it: the registry is enumerated by\n"+
				"per-stack config generators, and a fragment there renders junk names into\n"+
				"generated config. Guard the suffix instead, so the default stack keys on the\n"+
				"bare prefix:\n"+
				"  _wt = option(\"worktree\") or \"\"\n"+
				"  _key = \"prod-\" + _wt if _wt else \"prod\"",
			key, canonical)
	}
	return nil
}

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
// both `forge env up` and `forge env deploy` (both call this through the same
// builtin), which is the permanent up-vs-deploy port fix.
func AllocatePort(projectDir string, base int, key string) (int, error) {
	block, err := AllocateBlock(projectDir, key)
	if err != nil {
		return 0, err
	}
	return base + block*100, nil
}

// AllocatePortAvoidingForeign is AllocatePort with one addition: on the FIRST
// run of a given (base, key) it steps past a port some OTHER project is
// already holding, then memoizes the answer like any other block.
//
// It applies to the DEFAULT key ONLY — see the named-key guard in the body
// for why probing a dev stack's port is actively wrong.
//
// Why this exists. The block registry is per-repo, so the default key ""
// is block 0 in EVERY repo — and a second project on the same machine
// therefore asks for the identical base port. For the dev IdP that is fatal
// rather than inconvenient: forge (correctly) refuses to adopt an identity
// provider it did not start, because registering this project's application
// against another stack's IdP would succeed and mint tokens the wrong issuer
// signed. So the second project simply could not bring up sign-in.
//
// Determinism is preserved where it actually matters. The reason this port
// cannot float is that the issuer bakes it into every token's `iss` claim and
// into a registered redirect URI, so it must not move BETWEEN RUNS — which is
// a constraint on the value AFTER it is chosen, not on how it is chosen the
// first time. Once assigned, the block is in the registry and every later run
// returns it unchanged even if the port is momentarily busy.
//
// isFree is injected so the decision is testable without binding real ports.
func AllocatePortAvoidingForeign(projectDir string, base int, key string, isFree func(port int) bool) (int, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	// A key that already has a block keeps it, busy or not: that is the
	// stability guarantee, and re-deciding here would move an issuer.
	if assigned, ok := lookupBlock(projectDir, key); ok {
		return base + assigned*100, nil
	}
	// A NAMED key is a dev stack, and a dev stack's block comes from the
	// registry — never from a socket probe.
	//
	// Availability probing exists for the DEFAULT key only, and the reason is
	// specific (see the doc comment): the registry is per-repo, so key "" is
	// block 0 in EVERY repo, and two different projects on one machine ask
	// for the identical base port. A named key cannot have that problem —
	// within a repo the shared registry already hands out distinct blocks.
	//
	// Probing a named key is not merely unnecessary, it is WRONG, because
	// "the port answers" does not mean "the block is taken". A cluster
	// pre-maps every stack's host port at CREATE time precisely so a new
	// worktree needs no cluster recreate, so all N ports answer from the
	// moment the cluster exists, whether or not any stack is using them.
	// The probe therefore reads a full machine off an empty registry, walks
	// every block, and reports the ceiling — which is exactly how the FIRST
	// worktree on this machine was refused with "already reached the 8-stack
	// ceiling" while `forge env devstack list` printed nothing at all.
	if key != "" {
		return AllocatePort(projectDir, base, key)
	}
	if isFree == nil || isFree(base) {
		// Record the choice even though it is the default block. Without an
		// entry there is nothing to look up next run, so a base that happened
		// to be busy at that moment would be re-decided and the port would
		// move — exactly what the issuer cannot tolerate.
		if err := setBlock(projectDir, key, 0); err != nil {
			return 0, err
		}
		return base, nil
	}
	// Base is held by another stack. Claim the first free offset instead and
	// record it, so this project keeps that port from now on.
	for block := 1; block <= maxForeignProbeBlocks; block++ {
		candidate := base + block*100
		if !isFree(candidate) {
			continue
		}
		if err := checkCeiling(key, block); err != nil {
			return 0, err
		}
		if err := setBlock(projectDir, key, block); err != nil {
			return 0, err
		}
		return candidate, nil
	}
	// Nothing free in range: fall back to the deterministic answer and let
	// the caller's own port guard report the collision with its real
	// context, which is a better message than anything this layer could give.
	return AllocatePort(projectDir, base, key)
}

// maxForeignProbeBlocks bounds the search for a free block. Ten stacks of one
// base port on one machine is already far past the case this exists for.
const maxForeignProbeBlocks = 10

// lookupBlock reports the block recorded for key, if any. The default key ""
// is implicitly block 0 and is never stored, so it is only "assigned" once a
// real entry exists.
func lookupBlock(projectDir, key string) (int, bool) {
	var (
		block int
		found bool
	)
	_ = withLock(projectDir, func() error {
		reg, err := readRegistry(projectDir)
		if err != nil {
			return err
		}
		var e entry
		e, found = reg[key]
		block = e.Block
		return nil
	})
	return block, found
}

// setBlock records key -> block, preserving the entry's existing stack flag
// (this is a port decision, not an identity decision).
func setBlock(projectDir, key string, block int) error {
	return withLock(projectDir, func() error {
		reg, err := readRegistry(projectDir)
		if err != nil {
			return err
		}
		e := reg[key]
		e.Block = block
		e.Stack = e.Stack || isStackKey(key)
		reg[key] = e
		return writeRegistry(projectDir, reg)
	})
}

// isStackKey reports whether key identifies THIS command's dev stack — i.e.
// it is exactly the worktree name forge pushed into KCL as option("worktree")
// for this render.
//
// Equality with the active worktree is the whole test, and it is reliable in
// both directions. A dev KCL keys its stack on option("worktree") directly
// (control-plane: `_key = option("worktree") or ""`), so a stack key always
// arrives byte-equal to the active fact. A port-block key is always a COMPOSED
// expression — "prod", "prod-<worktree>" — which cannot equal the bare
// worktree name. Note the composed form is not a near-miss to be pattern-
// matched: "prod-wt-a" is a different string from "wt-a", so no heuristic is
// involved.
//
// The default key "" is never a stack key here: it is implicit block 0, is
// never stored in the registry, and every generator emits the default stack's
// config unconditionally rather than reading it from the roster.
//
// Unset active options (forge generate, forge ci, tests) mean no worktree, so
// nothing is marked — which is exactly right for a read-only render that must
// not record identity it was never given.
func isStackKey(key string) bool {
	if key == "" {
		return false
	}
	return key == Active().Worktree
}

// AllocateBlock returns the stable block index for key, assigning the next
// free one (≥1) on first use. key == "" is block 0 (the default stack)
// unless a previous availability-aware allocation recorded otherwise (see
// AllocatePortAvoidingForeign), which is why the registry is consulted for
// it rather than short-circuited. Atomic under the registry lock.
//
// A NEW block past the armed ceiling (see SetMaxStacks / checkCeiling) is
// refused. An EXISTING entry — one already in the registry, at whatever
// index it was assigned — always resolves, ceiling or not: the ceiling
// bounds what forge is willing to HAND OUT, never what it is willing to
// return for a block someone already holds.
func AllocateBlock(projectDir, key string) (int, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	if key == "" {
		if recorded, ok := lookupBlock(projectDir, ""); ok {
			return recorded, nil
		}
		return 0, nil
	}
	var block int
	err := withLock(projectDir, func() error {
		reg, err := readRegistry(projectDir)
		if err != nil {
			return err
		}
		stack := isStackKey(key)
		if existing, ok := reg[key]; ok {
			block = existing.Block
			// The block is settled and must never move. Only promote the
			// stack flag, which is how a legacy (bare-int) entry and any
			// entry first seen through a non-devstack path get labelled the
			// next time their own worktree brings the stack up.
			if stack && !existing.Stack {
				existing.Stack = true
				reg[key] = existing
				return writeRegistry(projectDir, reg)
			}
			return nil
		}
		block = nextFreeBlock(reg)
		if err := checkCeiling(key, block); err != nil {
			return err
		}
		reg[key] = entry{Block: block, Stack: stack}
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
func nextFreeBlock(reg registry) int {
	used := make(map[int]bool, len(reg))
	for _, e := range reg {
		used[e.Block] = true
	}
	for i := 1; ; i++ {
		if !used[i] {
			return i
		}
	}
}

// registryPath resolves the registry against the REPO ANCHOR (the primary
// checkout), not projectDir — so every linked worktree of one repo reads and
// writes the SAME registry. See repoAnchor.
func registryPath(projectDir string) string {
	return filepath.Join(repoAnchor(projectDir), registryRel)
}

// readRegistry loads the {key: entry} map, accepting the legacy bare-int form
// (see registry.UnmarshalJSON). A missing file is an empty registry (the
// first-ever named key). A corrupt file is an error — silently discarding it
// would re-assign blocks already in use by a live stack, colliding ports.
func readRegistry(projectDir string) (registry, error) {
	data, err := os.ReadFile(registryPath(projectDir))
	if os.IsNotExist(err) {
		return registry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read block registry: %w", err)
	}
	reg := registry{}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse block registry %s: %w", registryPath(projectDir), err)
	}
	return reg, nil
}

func writeRegistry(projectDir string, reg registry) error {
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

// Block is one registry entry, for diagnostics.
type Block struct {
	Key   string
	Index int
	// Stack marks a DEV-STACK key (a registered git worktree) as opposed to
	// a plain port-block key. See entry for why the two must not be
	// confused.
	Stack bool
}

// List returns the WHOLE block registry sorted by block index — every key,
// both dev stacks and plain port-block allocations. This is the diagnostic
// view ("what has consumed a port block on this machine?").
//
// A per-stack CONFIG GENERATOR must use ListStacks instead. Enumerating this
// one and treating each key as a running stack is precisely the bug that put a
// dev NATS account for a prod web port into version control.
func List(projectDir string) ([]Block, error) {
	reg, err := readRegistry(projectDir)
	if err != nil {
		return nil, err
	}
	out := make([]Block, 0, len(reg))
	for key, e := range reg {
		out = append(out, Block{Key: key, Index: e.Block, Stack: e.Stack})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out, nil
}

// ListStacks returns just the DEV-STACK keys — the registered git worktrees —
// sorted by block index. This is the roster a per-stack config generator
// enumerates to emit one config block per running stack.
//
// The DEFAULT stack (the primary checkout, key "") is NOT included: it is
// implicit block 0, never stored, and a generator always emits its config
// unconditionally. So this is strictly the named worktree stacks layered on
// top, and an empty result is the ordinary primary-checkout-only case.
func ListStacks(projectDir string) ([]string, error) {
	blocks, err := List(projectDir)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Stack {
			keys = append(keys, b.Key)
		}
	}
	return keys, nil
}
