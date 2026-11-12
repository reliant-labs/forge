package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/gitsource"
	"github.com/reliant-labs/forge/pkg/release"
)

func TestSnapshotFrontendWellKnownPublication(t *testing.T) {
	for _, runtime := range []string{FrontendRuntimeFirebase, FrontendRuntimeBucket} {
		t.Run(runtime, func(t *testing.T) {
			s, e := snapshotFrontendFixture(t, "web")
			if runtime == FrontendRuntimeBucket {
				e.Frontends[0].Runtime = FrontendRuntime{Type: runtime, Bucket: &BucketRuntime{Bucket: "gs://frozen"}}
			}
			want := map[string]string{"apple-app-site-association": "{\"webcredentials\":{\"apps\":[\"TEAM.com.acme.mobile\"]}}\n", "assetlinks.json": "[]\n"}
			e.Frontends[0].WellKnown = want
			captureFrontendFixture(t, &s, nil, e)
			// Round-trip the whole frozen snapshot: entity's json:"-" cannot hide a
			// missing overlay. Publication resolves into an unrelated checkout.
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var frozen release.Snapshot
			if err = json.Unmarshal(data, &frozen); err != nil {
				t.Fatal(err)
			}
			e.Frontends[0].WellKnown = map[string]string{"assetlinks.json": "CURRENT_WRONG"}
			otherCheckout := t.TempDir()
			published := false
			err = snapshotPublishFrontendsWith(context.Background(), frozen, false, snapshotFrontendAdapters{
				resolve: func(_ context.Context, src release.Source) (gitsource.Resolution, func(), error) {
					return gitsource.Resolution{Dir: otherCheckout, Commit: src.Commit}, nil, nil
				},
				preflight: func(_ context.Context, f FrontendEntity) error {
					if f.Path != filepath.Join(otherCheckout, "web") {
						t.Fatalf("not resolved in other checkout: %s", f.Path)
					}
					return nil
				},
				publish: func(_ context.Context, f FrontendEntity, _, _ string) error {
					published = true
					if !reflect.DeepEqual(f.WellKnown, want) {
						t.Fatalf("frozen overlay lost: %v", f.WellKnown)
					}
					// These are the same two adapters used by the real publisher.
					if runtime == FrontendRuntimeFirebase {
						if got := frontendToFirebase(f).WellKnown; !reflect.DeepEqual(got, want) {
							t.Fatalf("Firebase lost overlay: %v", got)
						}
					} else {
						if got := frontendToStaticSite(f).WellKnown; !reflect.DeepEqual(got, want) {
							t.Fatalf("bucket lost overlay: %v", got)
						}
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !published {
				t.Fatal("publication not reached")
			}
		})
	}
}
