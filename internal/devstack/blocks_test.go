package devstack

import (
	"sync"
	"testing"
)

// TestAllocatePortDefaultKeyIsBase: key "" ⇒ block 0 ⇒ base unchanged, with
// no registry written (the byte-identical default-stack path).
func TestAllocatePortDefaultKeyIsBase(t *testing.T) {
	dir := t.TempDir()
	p, err := AllocatePort(dir, 28080, "")
	if err != nil {
		t.Fatal(err)
	}
	if p != 28080 {
		t.Errorf("allocate_port(28080, \"\") = %d, want 28080 (base unchanged)", p)
	}
	if list, _ := List(dir); len(list) != 0 {
		t.Errorf("default key wrote a registry entry: %v", list)
	}
}

// TestAllocatePortKeyedOffsets: distinct keys get disjoint blocks; the same
// key always returns the same block (memoized).
func TestAllocatePortKeyedOffsets(t *testing.T) {
	dir := t.TempDir()
	// First key → block 1 → base+100.
	a, err := AllocatePort(dir, 28080, "wt-a")
	if err != nil {
		t.Fatal(err)
	}
	if a != 28180 {
		t.Errorf("wt-a port = %d, want 28180 (28080 + 1*100)", a)
	}
	// A SECOND port for the SAME key shares the key's block (same +100 shift).
	a2, _ := AllocatePort(dir, 3091, "wt-a")
	if a2 != 3191 {
		t.Errorf("wt-a second port = %d, want 3191 (3091 + 1*100)", a2)
	}
	// A distinct key → a disjoint block → base+200.
	b, _ := AllocatePort(dir, 28080, "wt-b")
	if b != 28280 {
		t.Errorf("wt-b port = %d, want 28280 (28080 + 2*100)", b)
	}
	// Re-allocating wt-a after wt-b returns the SAME block (stable).
	again, _ := AllocatePort(dir, 28080, "wt-a")
	if again != a {
		t.Errorf("wt-a re-allocated to %d, want stable %d", again, a)
	}
}

// TestUpDeployIdenticalPort is the up-vs-deploy regression-lock at the engine
// level: two separate AllocatePort calls (simulating the two commands) for
// the same (base, key) resolve identically because the block is persisted.
func TestUpDeployIdenticalPort(t *testing.T) {
	dir := t.TempDir()
	up, err := AllocatePort(dir, 29190, "wt-x")
	if err != nil {
		t.Fatal(err)
	}
	// A fresh "process" (a second call reading the same persisted registry)
	// must land on the identical port.
	deploy, _ := AllocatePort(dir, 29190, "wt-x")
	if up != deploy {
		t.Fatalf("up vs deploy drift: up=%d deploy=%d", up, deploy)
	}
}

func TestNextFreeBlockFillsGaps(t *testing.T) {
	reg := map[string]int{"a": 1, "c": 3}
	if got := nextFreeBlock(reg); got != 2 {
		t.Errorf("nextFreeBlock filling gap = %d, want 2", got)
	}
	reg["b"] = 2
	if got := nextFreeBlock(reg); got != 4 {
		t.Errorf("nextFreeBlock dense = %d, want 4", got)
	}
}

// TestConcurrentAllocateNoDuplicateBlock is the lock regression-lock: the
// concurrent first-`up` of N distinct keys must NOT race two keys to the
// same block. The file lock serializes the read-modify-write so every key
// gets a unique slot. Run with -race.
func TestConcurrentAllocateNoDuplicateBlock(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	blocks := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			blocks[i], errs[i] = AllocateBlock(dir, keyFor(i))
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("AllocateBlock[%d]: %v", i, errs[i])
		}
		if blocks[i] < 1 {
			t.Errorf("block %d < 1", blocks[i])
		}
		if seen[blocks[i]] {
			t.Errorf("duplicate block %d handed to two concurrent keys", blocks[i])
		}
		seen[blocks[i]] = true
	}
	if list, _ := List(dir); len(list) != n {
		t.Errorf("registry has %d entries, want %d", len(list), n)
	}
}

// TestConcurrentSameKeyOneBlock: many goroutines allocating the SAME key all
// land on one block — no churn, no duplicates.
func TestConcurrentSameKeyOneBlock(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	blocks := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			blocks[i], _ = AllocateBlock(dir, "shared")
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if blocks[i] != blocks[0] {
			t.Errorf("same key got differing blocks: %d vs %d", blocks[i], blocks[0])
		}
	}
	if list, _ := List(dir); len(list) != 1 {
		t.Errorf("registry has %d entries for one key, want 1", len(list))
	}
}

func keyFor(i int) string {
	return "wt-" + string(rune('a'+i))
}
