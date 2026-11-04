//go:build !windows

// These tests need the entry lock, which only exists off Windows (see
// lock_windows.go): without it nothing proves a staging directory dead, and
// concurrent builders are not serialized.

package forgedelegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/atomicfile"
)

// A build runs in a heavy slot, and a staging directory a dead build left in
// the entry is removed by the next build, which holds the entry's lock.
func TestPinned_SweepsADeadBuildsStagingInsideTheSlot(t *testing.T) {
	r, _, _ := newResolver(t)
	var slots []string
	held := false
	r.Slot = func(_ context.Context, label string) (func(), error) {
		slots = append(slots, label)
		held = true
		return func() { held = false }, nil
	}
	install := r.Install
	r.Install = func(ctx context.Context, pkg, gobin string) error {
		if !held {
			t.Error("the build ran outside a heavy slot")
		}
		return install(ctx, pkg, gobin)
	}
	entry := filepath.Join(os.Getenv(CacheDirEnv), safeName(pin), runtime.GOOS+"-"+runtime.GOARCH)
	dead := filepath.Join(entry, stagingPrefix+"dead", ".work", "go-build123")
	if err := os.MkdirAll(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pinned(context.Background(), t.TempDir(), pin); err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || held {
		t.Errorf("slots taken %v, still held %v; want one, released", slots, held)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(dead))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dead build's staging directory survived: %v", err)
	}
	ents, _ := os.ReadDir(entry)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			t.Errorf("the build left its staging directory %s", e.Name())
		}
	}
}

// Two builders of one entry build it once: the second waits for the entry's
// lock and then finds the first one's binary.
func TestPinned_ConcurrentRequestsBuildOnce(t *testing.T) {
	r, _, _ := newResolver(t)
	r.Slot = func(context.Context, string) (func(), error) { return func() {}, nil }
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	var builds int
	r.Install = func(_ context.Context, _, gobin string) error {
		builds++
		started <- struct{}{}
		<-release
		return atomicfile.WriteFile(filepath.Join(gobin, exeName()), []byte("#!/bin/sh\n"), 0o755)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := r.Pinned(context.Background(), t.TempDir(), pin)
			errs <- err
		}()
	}
	<-started
	select {
	case <-started:
		t.Fatal("a second build started while the first held the entry")
	case <-time.After(3 * lockEntryPoll):
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 {
		t.Errorf("%d builds, want 1", builds)
	}
}
