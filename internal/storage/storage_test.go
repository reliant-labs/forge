package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistryRetentionProtectsDigestAliasesAndReleaseTags(t *testing.T) {
	now := time.Now()
	p := DefaultPolicy()
	p.RegistryKeep = 2
	reg := Registry{Repositories: []string{"app"}}
	tags := []Tag{{"app", "new", "new", now}, {"app", "second", "second", now.Add(-time.Hour)}, {"app", "deployed", "keep", now.Add(-365 * 24 * time.Hour)}, {"app", "old-alias", "keep", now.Add(-100 * 24 * time.Hour)}, {"app", "v1.2.3", "release", now.Add(-365 * 24 * time.Hour)}, {"app", "expired", "drop", now.Add(-60 * 24 * time.Hour)}, {"unregistered/config.v1/app", "old", "artifact", now.Add(-60 * 24 * time.Hour)}}
	got := RegistryCandidates(tags, p, reg, map[string]map[string]bool{"app": {"deployed": true}}, now)
	if len(got) != 1 || got[0].Digest != "drop" {
		t.Fatalf("unsafe plan: %+v", got)
	}
}

func TestRegistryVersionFloorCountsDistinctDigests(t *testing.T) {
	now := time.Now()
	p := DefaultPolicy()
	p.RegistryKeep = 2
	tags := []Tag{{"app", "a", "one", now}, {"app", "alias", "one", now}, {"app", "b", "two", now.Add(-30 * 24 * time.Hour)}, {"app", "c", "three", now.Add(-40 * 24 * time.Hour)}}
	got := RegistryCandidates(tags, p, Registry{Repositories: []string{"app"}}, nil, now)
	if len(got) != 1 || got[0].Digest != "three" {
		t.Fatalf("plan: %+v", got)
	}
}

func TestRecentImagesSurviveRegardlessOfRank(t *testing.T) {
	now := time.Now()
	p := DefaultPolicy()
	p.RegistryKeep = 2
	var tags []Tag
	for i := 0; i < 20; i++ {
		tags = append(tags, Tag{"app", fmt.Sprint(i), fmt.Sprint(i), now.Add(-time.Duration(i) * time.Hour)})
	}
	if got := RegistryCandidates(tags, p, Registry{Repositories: []string{"app"}}, nil, now); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestPolicyRejectsUnsafeValues(t *testing.T) {
	for _, change := range []func(*Policy){func(p *Policy) { p.RegistryKeep = 1 }, func(p *Policy) { p.HostReserveGiB = 0 }, func(p *Policy) { p.ImageUnused = "0s" }, func(p *Policy) { p.SourceCacheUnused = "30m" }, func(p *Policy) { p.SourceCacheUnused = "" }, func(p *Policy) { p.SourceCacheKeep = 0 }, func(p *Policy) { p.Registries = []Registry{{Container: "remote"}} }} {
		p := DefaultPolicy()
		change(&p)
		if p.Validate() == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}

// TestPolicyFileWithoutSourceCacheKeysGetsDefaults pins the compatibility
// contract for every policy.json already on a developer's disk. Load decodes
// over DefaultPolicy with DisallowUnknownFields, so a document that omits the
// source-cache keys inherits the defaults — and must still Validate, because
// Load returns Validate's error.
func TestPolicyFileWithoutSourceCacheKeysGetsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"registry_keep":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatalf("a policy predating the source-cache keys no longer loads: %v", err)
	}
	if p.SourceCacheUnused != "336h" || p.SourceCacheKeep != 2 {
		t.Fatalf("source cache budgets = %q/%d, want the defaults 336h/2", p.SourceCacheUnused, p.SourceCacheKeep)
	}
	if p.RegistryKeep != 3 {
		t.Fatalf("the document's own value was lost: registry_keep = %d", p.RegistryKeep)
	}
}

func TestUnknownPolicyFieldRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	_ = os.WriteFile(path, []byte(`{"registry_kepp":2}`), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("misspelled destructive policy accepted")
	}
}

func TestKubeletHasAgeGCIndependentOfPressure(t *testing.T) {
	var config map[string]any
	_ = json.Unmarshal(DefaultPolicy().KubeletConfig(), &config)
	if config["imageMaximumGCAge"] != "168h" {
		t.Fatal(config)
	}
	if config["imageGCLowThresholdPercent"].(float64) >= config["imageGCHighThresholdPercent"].(float64) {
		t.Fatal(config)
	}
}

func TestHostReserveChecked(t *testing.T) {
	p := DefaultPolicy()
	p.HostReserveGiB = 1 << 30
	dir := t.TempDir()
	_, err := CheckSpace(p, dir)
	if err == nil {
		t.Fatal("unavailable reserve accepted")
	}
	// A measured shortfall is the one outcome a caller (the CI admission
	// path) may knowingly accept, so it must be distinguishable from a disk
	// that could not be read at all.
	var short *ReserveError
	if !errors.As(err, &short) {
		t.Fatalf("a shortfall is not a *ReserveError: %T %v", err, err)
	}
	if short.Path != dir || short.ReserveGiB != 1<<30 || short.Available == 0 {
		t.Errorf("shortfall = %+v, want the checked path, the policy reserve and the measured free bytes", short)
	}
}

func TestGCRejectsRemoteDockerBeforeMutation(t *testing.T) {
	var calls []string
	r := Runner{Policy: DefaultPolicy(), Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return []byte(`[{"Endpoints":{"docker":{"Host":"ssh://prod"}}}]`), nil
	}}
	if err := r.GC(context.Background(), true); err == nil {
		t.Fatal("remote accepted")
	}
	if len(calls) != 1 {
		t.Fatal(calls)
	}
}

func TestGCRejectsRemoteBuilder(t *testing.T) {
	r := Runner{Policy: DefaultPolicy(), Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "context" {
			return []byte(`[{"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil
		}
		if len(args) > 1 && args[1] == "inspect" {
			return []byte(`{"Driver":"docker-container","Nodes":[{"Endpoint":"ssh://prod"}]}`), nil
		}
		t.Fatalf("mutation: %v", args)
		return nil, nil
	}}
	if err := r.GC(context.Background(), true); err == nil {
		t.Fatal("remote builder accepted")
	}
}

func TestUnreachableClusterFailsProtection(t *testing.T) {
	r := Runner{Policy: DefaultPolicy(), Command: func(context.Context, string, ...string) ([]byte, error) { return nil, fmt.Errorf("unreachable") }}
	if _, err := r.protected(context.Background(), Registry{Contexts: []string{"k3d-offline"}}); err == nil {
		t.Fatal("unresolved protected set accepted")
	}
}

func TestMaintenanceLockExcludesConcurrentRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := WithLock(path, func() error {
		if err := WithLock(path, func() error { t.Fatal("second writer admitted"); return nil }); err == nil {
			t.Fatal("lock succeeded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLogRetentionKeepsCurrentStreamsAndRecentDiagnostics(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".forge", "logs", "dev")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("worker.2026-01-%02dT00-00-00.log", i+1)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("log"), 0600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-time.Duration(i+10) * 24 * time.Hour)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"worker.log", "not-a-rotated-log.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := DefaultPolicy()
	p.Projects = []string{root}
	r := Runner{Policy: p}
	if err := r.Logs(false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(dir)
	if len(before) != 11 {
		t.Fatal("preview mutated logs")
	}
	if err := r.Logs(true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadDir(dir)
	if len(after) != 7 {
		t.Fatalf("want 5 rotated + current + unrelated, got %d", len(after))
	}
	for _, name := range []string{"worker.log", "not-a-rotated-log.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoricalLedgerPinsFailClosed(t *testing.T) {
	dir := t.TempDir()
	digest := "sha256:" + strings.Repeat("a", 64)
	b := []byte(`{"artifacts":{"api":{"digests":{"*":"` + digest + `"}}}}`)
	if err := os.WriteFile(filepath.Join(dir, "v1.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	pins, err := LedgerPins(dir)
	if err != nil || len(pins) != 1 || pins[0] != digest {
		t.Fatalf("pins %v: %v", pins, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v2.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LedgerPins(dir); err == nil {
		t.Fatal("accepted unreadable ledger")
	}
}

func TestWorktreesOnlyRemoveOldCleanMergedCheckouts(t *testing.T) {
	root := t.TempDir()
	paths := map[string]string{}
	var listing strings.Builder
	for _, name := range []string{"primary", "clean", "dirty", "unmerged", "locked"} {
		path := filepath.Join(root, name)
		paths[name] = path
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-60 * 24 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&listing, "worktree %s\nHEAD %s\n", path, name)
		if name == "locked" {
			listing.WriteString("locked held by user\n")
		}
		listing.WriteString("\n")
	}
	fakeLsof(t, unrelatedOpenFile, "exit 0")
	var removed []string
	r := Runner{Command: func(_ context.Context, command string, args ...string) ([]byte, error) {
		if command != "git" {
			return nil, fmt.Errorf("unexpected command %s", command)
		}
		switch args[2] {
		case "rev-parse":
			return []byte("base"), nil
		case "worktree":
			if args[3] == "list" {
				return []byte(listing.String()), nil
			}
			for _, a := range args {
				if a == "--force" || a == "-f" {
					t.Fatalf("worktree removal forced: %v", args)
				}
			}
			removed = append(removed, args[4])
			return nil, nil
		case "merge-base":
			if args[4] == "unmerged" {
				return nil, fmt.Errorf("not merged")
			}
			return nil, nil
		case "status":
			if args[1] == paths["dirty"] {
				return []byte("?? uncommitted.txt"), nil
			}
			return nil, nil
		case "show":
			return []byte(fmt.Sprint(time.Now().Add(-60 * 24 * time.Hour).Unix())), nil
		}
		return nil, fmt.Errorf("unexpected args %v", args)
	}}
	if err := r.Worktrees(context.Background(), root, "origin/main", 30*24*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatal("preview removed a tree")
	}
	if err := r.Worktrees(context.Background(), root, "origin/main", 30*24*time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != paths["clean"] {
		t.Fatalf("removed %v", removed)
	}
}

func TestKubeletDurationNormalization(t *testing.T) {
	if !sameDuration("168h0m0s", "168h") || sameDuration("0s", "168h") || sameDuration("bad", "bad") {
		t.Fatal("incorrect kubelet duration comparison")
	}
}
