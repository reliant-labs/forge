package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Refresh after enumeration AND while the governor is pacing: checking before
// grantLocked still deletes the live artifact. No sleeps or real cache access.
func TestGoCacheRetainsEntriesRefreshedDuringPacing(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "archive", true: "executable"}[directory], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "ab", hexName(1, "-d"))
			if directory {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "program"), []byte("binary"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				path = putEntry(t, root, "ab", hexName(1, "-d"), 10, 72*time.Hour)
			}
			old := time.Now().Add(-72 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			del := newDeleter(DefaultPolicy(), nil, func(context.Context, time.Duration) error {
				now := time.Now()
				return os.Chtimes(path, now, now)
			})
			del.pass.inBatch = deleteBatch
			del.pass.batchFrom = time.Now()
			pass := streamTrimGoCache(context.Background(), root, []string{"ab"}, 0, time.Now().Add(-goCacheFloor), true, del)
			if !exists(path) || (directory && !exists(filepath.Join(path, "program"))) {
				t.Fatal("deleted an artifact Go marked used while the reaper waited")
			}
			if pass.removed != 0 || pass.freed != 0 || !pass.finished {
				t.Fatalf("retained entry must not count as freed or stop the pass: %+v", pass)
			}
		})
	}
}
