package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRegistryGraphRetainsChildrenOfUntaggedIndexes(t *testing.T) {
	now := time.Now()
	digest := func(letter string) string { return "sha256:" + strings.Repeat(letter, 64) }
	parent, child, recent, second, expired := digest("a"), digest("b"), digest("c"), digest("d"), digest("e")
	p := DefaultPolicy()
	p.RegistryKeep = 2
	reg := Registry{Repositories: []string{"app"}}
	tags := []Tag{
		{"app", "recent", recent, now},
		{"app", "second", second, now.Add(-time.Hour)},
		{"app", "old-child", child, now.Add(-30 * 24 * time.Hour)},
		{"app", "expired", expired, now.Add(-40 * 24 * time.Hour)},
	}
	revisions := []Version{{"app", parent}, {"app", child}, {"app", recent}, {"app", second}, {"app", expired}}
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("wildcard_pin=%t", pinned), func(t *testing.T) {
			protected := map[string]map[string]bool{}
			if pinned {
				protected["*"] = map[string]bool{parent: true}
			}
			visited := map[string]bool{}
			got, err := registryGraphPlan(tags, revisions, p, reg, protected, now, func(v Version) ([]byte, error) {
				visited[v.Digest] = true
				if v.Digest == parent {
					return []byte(`{"manifests":[{"digest":"` + child + `"}]}`), nil
				}
				return []byte(`{"schemaVersion":2}`), nil
			})
			if err != nil || len(got) != 1 || got[0] != (Version{"app", expired}) {
				t.Fatalf("untagged index child must remain pullable: plan=%v err=%v", got, err)
			}
			if !visited[parent] || !visited[child] {
				t.Fatalf("retained graph was not traversed: %v", visited)
			}
		})
	}
}

func TestRegistryGraphDoesNotProtectEligibleIndexes(t *testing.T) {
	now := time.Now()
	digest := func(letter string) string { return "sha256:" + strings.Repeat(letter, 64) }
	parent, child, recent, second := digest("a"), digest("b"), digest("c"), digest("d")
	p := DefaultPolicy()
	p.RegistryKeep = 2
	tags := []Tag{
		{"app", "recent", recent, now}, {"app", "second", second, now},
		{"app", "old-index", parent, now.Add(-30 * 24 * time.Hour)},
		{"app", "old-child", child, now.Add(-30 * 24 * time.Hour)},
	}
	revisions := []Version{{"app", parent}, {"app", child}, {"app", recent}, {"app", second}}
	got, err := registryGraphPlan(tags, revisions, p, Registry{Repositories: []string{"app"}}, nil, now, func(v Version) ([]byte, error) {
		if v.Digest == parent || v.Digest == child {
			t.Fatal("eligible manifest treated as retained graph root")
		}
		return []byte(`{}`), nil
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("eligible graph must remain reclaimable: %v, %v", got, err)
	}
}

func TestRegistryGraphFailsClosedOnUnreadableUntaggedRoot(t *testing.T) {
	root := Version{"app", "sha256:" + strings.Repeat("a", 64)}
	_, err := registryGraphPlan(nil, []Version{root}, DefaultPolicy(), Registry{Repositories: []string{"app"}}, nil, time.Now(), func(Version) ([]byte, error) {
		return nil, fmt.Errorf("missing retained manifest")
	})
	if err == nil {
		t.Fatal("unreadable untagged root allowed deletion planning")
	}
}

func TestRegistryRevisionInventoryValidatesLinks(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	path := "/var/lib/registry/docker/registry/v2/repositories/team/app/_manifests/revisions/sha256/" + strings.TrimPrefix(digest, "sha256:") + "/link"
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			link := digest
			if corrupt {
				link = "sha256:" + strings.Repeat("b", 64)
			}
			r := Runner{Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "docker" || args[0] != "exec" || !strings.Contains(args[len(args)-1], "_manifests/revisions/sha256/") {
					t.Fatalf("unexpected inventory command: %s %v", name, args)
				}
				return []byte(path + "\t" + link + "\n"), nil
			}}
			got, err := r.revisions(context.Background(), "registry")
			if corrupt {
				if err == nil {
					t.Fatal("mismatched revision link accepted")
				}
			} else if err != nil || len(got) != 1 || got[0] != (Version{"team/app", digest}) {
				t.Fatalf("revision inventory: %v, %v", got, err)
			}
		})
	}
}
