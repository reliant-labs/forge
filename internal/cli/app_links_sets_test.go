package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

func TestAppLinkDomainSets(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second []string
		separate      bool
		want          string
	}{
		{"reversed", []string{"a.example", "b.example"}, []string{"b.example", "a.example"}, false, ""},
		{"overlapping same site", []string{"a.example", "b.example"}, []string{"b.example", "c.example"}, false, "incompatible associated domain sets"},
		{"subset same site", []string{"a.example", "b.example"}, []string{"b.example"}, false, "incompatible associated domain sets"},
		{"disjoint same site", []string{"a.example"}, []string{"b.example"}, false, "incompatible associated domain sets"},
		{"overlapping different sites", []string{"a.example", "b.example"}, []string{"b.example", "c.example"}, true, "served by two sites"},
		{"disjoint different sites", []string{"a.example"}, []string{"b.example"}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := appLinkEntities(nil)
			e.Identity.Applications[0].AssociatedDomains = slices.Clone(tc.first)
			e.Identity.Applications = append(e.Identity.Applications, release.ShapeIdentityApplication{Name: "dev:other", Kind: release.IdentityAppNative, Frontend: "other", AssociatedDomains: slices.Clone(tc.second)})
			other := e.Frontends[0]
			other.Name = "other"
			other.Path = "frontends/other"
			e.Frontends[0].AssociatedSite = "web"
			other.AssociatedSite = "web"
			if tc.separate {
				other.AssociatedSite = "links"
				e.Frontends = append(e.Frontends, FrontendEntity{Name: "links", Type: "vite", Runtime: FrontendRuntime{Type: FrontendRuntimeHosted}})
			}
			e.Frontends = append(e.Frontends, other)
			src := appLinkSourceFor("dev", e)
			src.ReadAppIDs = func(dir string) (nativeAppIDs, error) {
				id := "com.acme.mobile"
				if strings.HasSuffix(dir, "other") {
					id = "com.acme.other"
				}
				return nativeAppIDs{BundleID: id, Package: id}, nil
			}
			err := attachAppLinkFiles(context.Background(), src)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v want %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			aasa := e.Frontends[1].WellKnown["apple-app-site-association"]
			if !strings.Contains(aasa, "com.acme.mobile") {
				t.Fatal(aasa)
			}
			if !tc.separate && !strings.Contains(aasa, "com.acme.other") {
				t.Fatal("reversed sets lost second app: " + aasa)
			}
			if tc.separate && strings.Contains(aasa, "com.acme.other") {
				t.Fatal("broadened association: " + aasa)
			}
			if !slices.Equal(e.Identity.Applications[0].AssociatedDomains, tc.first) || !slices.Equal(e.Identity.Applications[1].AssociatedDomains, tc.second) {
				t.Fatal("changed callback precedence")
			}
			if err := attachAppLinkFiles(context.Background(), src); err != nil {
				t.Fatal(err)
			}
			if e.Frontends[1].WellKnown["apple-app-site-association"] != aasa {
				t.Fatal("render not stable")
			}
		})
	}
}
