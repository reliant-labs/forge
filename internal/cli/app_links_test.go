package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

const appLinkTestFP = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"

func appLinkEntities(mut func(e *KCLEntities)) *KCLEntities {
	e := &KCLEntities{
		Identity: &release.ShapeIdentity{Name: "identity", Runtime: release.IdentityRuntimeHost, Applications: []release.ShapeIdentityApplication{{
			Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile",
			AssociatedDomains: []string{"app.acme.example"},
		}}},
		Frontends: []FrontendEntity{
			{
				Name: "mobile", Type: "rn", Path: "frontends/mobile", AppleTeamID: "23P64LQTZD",
				AndroidCertFingerprints: map[string][]string{"dev": {"debug"}, "prod": {appLinkTestFP}},
			},
			{Name: "web", Type: "vite", Path: "frontends/web", Runtime: FrontendRuntime{Type: FrontendRuntimeHosted}},
		},
	}
	if mut != nil {
		mut(e)
	}
	return e
}

func appLinkSourceFor(env string, e *KCLEntities) appLinkSource {
	return appLinkSource{
		Env: env, ProjectDir: "/proj", Entities: e,
		ReadAppIDs: func(string) (nativeAppIDs, error) {
			return nativeAppIDs{BundleID: "com.acme.mobile", Package: "com.acme.mobile"}, nil
		},
		DebugPrint: func(string) (string, error) { return appLinkTestFP, nil },
	}
}

func TestAttachAppLinkFiles_DevResolvesDebugKeystoreOntoTheOnlySite(t *testing.T) {
	e := appLinkEntities(nil)
	if err := attachAppLinkFiles(context.Background(), appLinkSourceFor("dev", e)); err != nil {
		t.Fatal(err)
	}
	web := e.Frontends[1]
	if len(web.WellKnown) != 2 {
		t.Fatalf("site files = %v", web.WellKnown)
	}
	if !strings.Contains(web.WellKnown["apple-app-site-association"], "23P64LQTZD.com.acme.mobile") {
		t.Errorf("AASA = %s", web.WellKnown["apple-app-site-association"])
	}
	if !strings.Contains(web.WellKnown["assetlinks.json"], appLinkTestFP) {
		t.Errorf("assetlinks = %s", web.WellKnown["assetlinks.json"])
	}
	if len(e.Frontends[0].WellKnown) != 0 {
		t.Error("the native frontend must not carry the overlay")
	}
}

func TestAttachAppLinkFiles_ProdUsesItsOwnFingerprintsNotDebug(t *testing.T) {
	prodFP := strings.ReplaceAll(appLinkTestFP, "AA", "BB")
	e := appLinkEntities(func(e *KCLEntities) {
		e.Frontends[0].AndroidCertFingerprints["prod"] = []string{prodFP}
	})
	src := appLinkSourceFor("prod", e)
	src.DebugPrint = func(string) (string, error) { return "", errors.New("the debug keystore must not be read for prod") }
	if err := attachAppLinkFiles(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	links := e.Frontends[1].WellKnown["assetlinks.json"]
	if !strings.Contains(links, prodFP) || strings.Contains(links, appLinkTestFP) {
		t.Errorf("prod assetlinks = %s", links)
	}
}

func TestAttachAppLinkFiles_NoDomainsChangesNothing(t *testing.T) {
	e := appLinkEntities(func(e *KCLEntities) { e.Identity.Applications[0].AssociatedDomains = nil })
	if err := attachAppLinkFiles(context.Background(), appLinkSourceFor("dev", e)); err != nil {
		t.Fatal(err)
	}
	for _, f := range e.Frontends {
		if len(f.WellKnown) != 0 {
			t.Errorf("%s got files: %v", f.Name, f.WellKnown)
		}
	}
}

func TestAttachAppLinkFiles_RefusesWhatItCannotBind(t *testing.T) {
	cases := []struct {
		name string
		env  string
		mut  func(e *KCLEntities)
		want string
	}{
		{"no signing identity at all", "staging", func(e *KCLEntities) { e.Frontends[0].AppleTeamID = "" }, "nothing binds the app"},
		{"no static site", "dev", func(e *KCLEntities) { e.Frontends = e.Frontends[:1] }, "no static-site frontend"},
		{"two candidate sites", "dev", func(e *KCLEntities) {
			e.Frontends = append(e.Frontends, FrontendEntity{Name: "docs", Type: "vite", Runtime: FrontendRuntime{Type: FrontendRuntimeBucket}})
		}, "set associated_site"},
		{"associated_site names the native app", "dev", func(e *KCLEntities) { e.Frontends[0].AssociatedSite = "mobile" }, "is a native frontend"},
		{"associated_site names nothing", "dev", func(e *KCLEntities) { e.Frontends[0].AssociatedSite = "nope" }, "not a frontend of this env"},
		{"https redirect off the domains", "dev", func(e *KCLEntities) {
			e.Identity.Applications[0].RedirectURIs = []string{"https://other.example/oauthredirect"}
		}, "not in associated_domains"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := attachAppLinkFiles(context.Background(), appLinkSourceFor(tc.env, appLinkEntities(tc.mut)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestAttachAppLinkFiles_AssociatedSiteSelectsAmongSeveral(t *testing.T) {
	e := appLinkEntities(func(e *KCLEntities) {
		e.Frontends = append(e.Frontends, FrontendEntity{Name: "docs", Type: "vite", Runtime: FrontendRuntime{Type: FrontendRuntimeBucket}})
		e.Frontends[0].AssociatedSite = "docs"
	})
	if err := attachAppLinkFiles(context.Background(), appLinkSourceFor("dev", e)); err != nil {
		t.Fatal(err)
	}
	if len(e.Frontends[2].WellKnown) != 2 || len(e.Frontends[1].WellKnown) != 0 {
		t.Errorf("web=%v docs=%v", e.Frontends[1].WellKnown, e.Frontends[2].WellKnown)
	}
}

func TestAttachAppLinkFiles_AMissingDebugKeystoreSaysHowToFixIt(t *testing.T) {
	src := appLinkSourceFor("dev", appLinkEntities(nil))
	src.DebugPrint = func(string) (string, error) { return "", errors.New("keystore not found") }
	err := attachAppLinkFiles(context.Background(), src)
	if err == nil || !strings.Contains(err.Error(), "expo prebuild") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadNativeAppIDs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"expo":{"ios":{"bundleIdentifier":"com.acme.mobile"},"android":{"package":"com.acme.mobile"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readNativeAppIDs(dir)
	if err != nil || got.BundleID != "com.acme.mobile" || got.Package != "com.acme.mobile" {
		t.Fatalf("ids = %+v, %v", got, err)
	}
	if _, err := readNativeAppIDs(t.TempDir()); err == nil {
		t.Error("a missing app.json was accepted")
	}
}

func TestAttachAppLinkFiles_IOSOnlyEnvRendersOnlyTheAASA(t *testing.T) {
	e := appLinkEntities(nil)
	if err := attachAppLinkFiles(context.Background(), appLinkSourceFor("staging", e)); err != nil {
		t.Fatal(err)
	}
	got := e.Frontends[1].WellKnown
	if len(got) != 1 || got["apple-app-site-association"] == "" {
		t.Errorf("files = %v: staging declares no Android fingerprint", got)
	}
}
