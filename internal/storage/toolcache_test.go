package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/openfiles"
	"github.com/reliant-labs/forge/internal/toolcache"
)

// The tool-cache layer reclaims from an absolute path outside any project,
// so a test that runs GC for another reason must never reach the real cache.
func TestToolsRefusesTheRealCacheUnderTest(t *testing.T) {
	var out strings.Builder
	r := Runner{Policy: DefaultPolicy(), Out: &out}
	if err := r.Tools(true); err != nil {
		t.Fatalf("Tools must skip, not fail, so it never aborts an unrelated GC assertion: %v", err)
	}
	if !strings.Contains(out.String(), "ToolCacheRoot is unset under test") {
		t.Fatalf("the skip must say what to set:\n%s", out.String())
	}
}

// toolEntry writes an entry of the current layout whose last use was at used.
func toolEntry(t *testing.T, root, kind, key string, used time.Time) string {
	t.Helper()
	dir := filepath.Join(root, kind, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, toolcache.UsedMarker)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, used, used); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The layer applies the machine policy's budget: tool_cache_unused and
// tool_cache_keep decide, through the pass's governor and open-file snapshot.
func TestToolsReclaimsByThePolicyBudget(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stale := toolEntry(t, root, toolcache.KindGate, "buf@v1.50.0", now.Add(-5*24*time.Hour))
	idle := toolEntry(t, root, toolcache.KindGate, "buf@v1.57.0", now.Add(-4*24*time.Hour)) // buf's newest: kept
	recent := toolEntry(t, root, toolcache.KindPinned, "protoc-gen-go@v1.36.11", now.Add(-time.Hour))
	newest := toolEntry(t, root, toolcache.KindPinned, "protoc-gen-go@v1.36.12", now)

	p := DefaultPolicy()
	p.ToolCacheUnused = "48h"
	var out strings.Builder
	r := Runner{Policy: p, Out: &out, ToolCacheRoot: root,
		OpenPaths: func(context.Context) (openfiles.Snapshot, error) { return openfiles.Snapshot{}, nil }}
	r = r.beginPass(context.Background())

	if err := r.Tools(false); err != nil {
		t.Fatal(err)
	}
	if !dirExists(stale) {
		t.Fatal("a preview removed an entry")
	}
	if err := r.Tools(true); err != nil {
		t.Fatal(err)
	}
	if dirExists(stale) {
		t.Errorf("an entry unused past tool_cache_unused, beyond buf's newest, survived:\n%s", out.String())
	}
	for _, kept := range []string{idle, recent, newest} {
		if !dirExists(kept) {
			t.Errorf("%s was removed", kept)
		}
	}
	if !strings.Contains(out.String(), "tool cache: 1 entries") {
		t.Errorf("the layer must report what it reclaimed:\n%s", out.String())
	}
}

func TestPolicyValidatesTheToolCacheBudget(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("default policy: %v", err)
	}
	for _, bad := range []func(*Policy){
		func(p *Policy) { p.ToolCacheUnused = "2h" },
		func(p *Policy) { p.ToolCacheUnused = "soon" },
		func(p *Policy) { p.ToolCacheKeep = -1 },
	} {
		p := DefaultPolicy()
		bad(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("policy %+v validated", p)
		}
	}
	// A policy written before these keys existed loads with the defaults.
	path := filepath.Join(t.TempDir(), "storage.json")
	if err := os.WriteFile(path, []byte(`{"log_budget_gib": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil || p.ToolCacheUnused != "336h" || p.ToolCacheKeep != 1 {
		t.Errorf("Load = %+v, %v; want the tool-cache defaults", p, err)
	}
}

func dirExists(path string) bool { _, err := os.Stat(path); return err == nil }
