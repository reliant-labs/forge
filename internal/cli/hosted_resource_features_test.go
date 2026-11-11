package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

type resourceFeatureClient struct {
	doc   string
	calls []string
}

func (c *resourceFeatureClient) Call(_ context.Context, proc string, _, out any) error {
	c.calls = append(c.calls, proc)
	return json.Unmarshal([]byte(c.doc), out)
}

func linkedSnapshotResource() release.SnapshotResource {
	return release.SnapshotResource{Key: release.ResourceKey{Group: "forge.dev", Kind: "StaticSite"}, Spec: []byte("apiVersion: forge.dev/v1alpha1\nkind: StaticSite\nspec:\n  wellKnown:\n    assetlinks.json: '[]'\n")}
}

func TestSnapshotResourceFeatures_FrozenStaticSiteRequiresServingSupport(t *testing.T) {
	for _, tc := range []struct {
		doc string
		ok  bool
	}{
		{`{}`, false},
		{`{"bundleDocumentFeatures":["static-site-well-known"]}`, false},
		{`{"resourceFeatures":["static-site-well-known"]}`, true},
	} {
		c := &resourceFeatureClient{doc: tc.doc}
		err := requireSnapshotResourceFeatures(context.Background(), c, []release.SnapshotResource{linkedSnapshotResource()})
		if (err == nil) != tc.ok {
			t.Fatalf("%s: %v", tc.doc, err)
		}
		if len(c.calls) != 1 || c.calls[0] != deploytarget.ProcDeployCapabilities {
			t.Fatalf("calls=%v", c.calls)
		}
	}
	c := &resourceFeatureClient{}
	if err := requireSnapshotResourceFeatures(context.Background(), c, nil); err != nil || len(c.calls) > 0 {
		t.Fatalf("empty legacy inventory: %v, %v", c.calls, err)
	}
}

func TestRecordBundle_OldServerCannotSealWellKnown(t *testing.T) {
	s := release.Snapshot{Resources: []release.SnapshotResource{linkedSnapshotResource()}}
	cfg, _ := json.Marshal(map[string]any{"snapshot": s})
	c := &resourceFeatureClient{doc: `{}`}
	_, _, err := (hostedBundleClient{client: c}).RecordBundle(context.Background(), "dev", "env", "repo", []byte("manifest"), cfg, release.Run{})
	if err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("err=%v", err)
	}
	if len(c.calls) != 1 || c.calls[0] != deploytarget.ProcDeployCapabilities {
		t.Fatalf("published before capability: %v", c.calls)
	}
}
