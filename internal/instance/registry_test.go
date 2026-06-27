package instance

import (
	"sync"
	"testing"
)

func TestResolveDefaultNoRegistry(t *testing.T) {
	dir := t.TempDir()
	// No flag, and t.TempDir() is not a git worktree → default instance.
	inst, err := Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.IsDefault() {
		t.Errorf("expected default instance, got %v", inst)
	}
	// The default must NOT write a registry file (zero cost for plain stacks).
	if list, _ := List(dir); len(list) != 0 {
		t.Errorf("default instance wrote a registry entry: %v", list)
	}
}

func TestAssignIndexStable(t *testing.T) {
	dir := t.TempDir()
	// First use assigns index 1 (0 reserved for default).
	i1, err := assignIndex(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if i1 != 1 {
		t.Errorf("first named index = %d, want 1", i1)
	}
	// A second distinct name gets the next free index.
	i2, _ := assignIndex(dir, "beta")
	if i2 != 2 {
		t.Errorf("second named index = %d, want 2", i2)
	}
	// Re-resolving the SAME name returns the SAME index — stability across runs.
	again, _ := assignIndex(dir, "alpha")
	if again != i1 {
		t.Errorf("alpha re-assigned to %d, want stable %d", again, i1)
	}
}

func TestNextFreeIndexFillsGaps(t *testing.T) {
	reg := map[string]int{"a": 1, "c": 3}
	if got := nextFreeIndex(reg); got != 2 {
		t.Errorf("nextFreeIndex filling gap = %d, want 2", got)
	}
	reg["b"] = 2
	if got := nextFreeIndex(reg); got != 4 {
		t.Errorf("nextFreeIndex dense = %d, want 4", got)
	}
}

func TestResolveSurvivesAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	a, err := Resolve(dir, "myworktree")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "myworktree" || a.Index != 1 {
		t.Fatalf("first resolve = %v, want myworktree(#1)", a)
	}
	// A fresh process (simulated by a second Resolve reading the same dir)
	// reuses the persisted index.
	b, _ := Resolve(dir, "myworktree")
	if b != a {
		t.Errorf("resolve not stable across calls: %v != %v", a, b)
	}
}

// TestConcurrentAssignNoDuplicateIndex is the lock regression-lock: the
// concurrent first-`up` of N distinct worktrees must NOT race two names to
// the same index. The file lock serializes the read-modify-write so every
// name gets a unique slot.
func TestConcurrentAssignNoDuplicateIndex(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	idxs := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := nameFor(i)
			idxs[i], errs[i] = assignIndex(dir, name)
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("assignIndex[%d]: %v", i, errs[i])
		}
		if idxs[i] < 1 {
			t.Errorf("index %d < 1", idxs[i])
		}
		if seen[idxs[i]] {
			t.Errorf("duplicate index %d handed to two concurrent names", idxs[i])
		}
		seen[idxs[i]] = true
	}
	// And the persisted registry agrees: n distinct entries.
	list, _ := List(dir)
	if len(list) != n {
		t.Errorf("registry has %d entries, want %d", len(list), n)
	}
}

// TestConcurrentSameNameOneIndex: many goroutines resolving the SAME name
// (e.g. retries) all land on one index — no churn, no duplicates.
func TestConcurrentSameNameOneIndex(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	idxs := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idxs[i], _ = assignIndex(dir, "shared")
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if idxs[i] != idxs[0] {
			t.Errorf("same name got differing indices: %d vs %d", idxs[i], idxs[0])
		}
	}
	if list, _ := List(dir); len(list) != 1 {
		t.Errorf("registry has %d entries for one name, want 1", len(list))
	}
}

func nameFor(i int) string {
	return "wt-" + string(rune('a'+i))
}
