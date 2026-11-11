package applinks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	fpA = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	fpB = "01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF"
)

func TestRenderAASA_ExactContents(t *testing.T) {
	got, err := RenderAASA([]App{{Frontend: "mobile", TeamID: "23P64LQTZD", BundleID: "com.acme.mobile"}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{
  "applinks": {
    "details": [
      {
        "appIDs": [
          "23P64LQTZD.com.acme.mobile"
        ],
        "components": [
          {
            "/": "/oauthredirect"
          },
          {
            "/": "/signedout"
          }
        ]
      }
    ]
  },
  "webcredentials": {
    "apps": [
      "23P64LQTZD.com.acme.mobile"
    ]
  }
}
`
	if string(got) != want {
		t.Fatalf("AASA =\n%s\nwant\n%s", got, want)
	}
	if !json.Valid(got) {
		t.Error("AASA is not valid JSON")
	}
}

func TestRenderAASA_SeveralAppsAreSortedAndDeduplicated(t *testing.T) {
	got, err := RenderAASA([]App{
		{TeamID: "23P64LQTZD", BundleID: "com.acme.zed"},
		{TeamID: "23P64LQTZD", BundleID: "com.acme.mobile"},
		{TeamID: "23P64LQTZD", BundleID: "com.acme.mobile"},
		{Package: "com.acme.androidonly", Fingerprints: []string{fpA}}, // no iOS side
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		WebCredentials struct{ Apps []string } `json:"webcredentials"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if strings.Join(doc.WebCredentials.Apps, ",") != "23P64LQTZD.com.acme.mobile,23P64LQTZD.com.acme.zed" {
		t.Errorf("apps = %v", doc.WebCredentials.Apps)
	}
}

func TestRenderAASA_NoIOSAppRendersNothing(t *testing.T) {
	for _, apps := range [][]App{nil, {{BundleID: "com.acme.mobile"}}, {{TeamID: "23P64LQTZD"}}} {
		if got, err := RenderAASA(apps); err != nil || got != nil {
			t.Errorf("RenderAASA(%v) = %q, %v; want nothing", apps, got, err)
		}
	}
}

func TestRenderAssetLinks_ExactContents(t *testing.T) {
	got, err := RenderAssetLinks([]App{{Frontend: "mobile", Package: "com.acme.mobile", Fingerprints: []string{strings.ToLower(fpB), fpA}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[
  {
    "relation": [
      "delegate_permission/common.handle_all_urls"
    ],
    "target": {
      "namespace": "android_app",
      "package_name": "com.acme.mobile",
      "sha256_cert_fingerprints": [
        "` + fpB + `",
        "` + fpA + `"
      ]
    }
  }
]
`
	if string(got) != want {
		t.Fatalf("assetlinks =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderAssetLinks_RejectsAMalformedFingerprint(t *testing.T) {
	for _, bad := range []string{"AA:BB", "debug", "", strings.Repeat("ZZ:", 31) + "ZZ"} {
		if _, err := RenderAssetLinks([]App{{Package: "com.acme.mobile", Fingerprints: []string{bad}}}); err == nil {
			t.Errorf("fingerprint %q accepted", bad)
		}
	}
	if got, err := RenderAssetLinks([]App{{Package: "com.acme.mobile"}, {Fingerprints: []string{fpA}}}); err != nil || got != nil {
		t.Errorf("an app with no fingerprints or no package must render nothing, got %q %v", got, err)
	}
}

func TestRender_FilesAreKeyedByWellKnownPath(t *testing.T) {
	files, err := Render([]App{{TeamID: "23P64LQTZD", BundleID: "com.acme.mobile", Package: "com.acme.mobile", Fingerprints: []string{fpA}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[AASAPath] == nil || files[AssetLinksPath] == nil {
		t.Fatalf("files = %v", files)
	}
	if AASAPath != ".well-known/apple-app-site-association" || AssetLinksPath != ".well-known/assetlinks.json" || ContentType != "application/json" {
		t.Error("the served paths or content type changed: the control plane and every deploy target depend on them")
	}
	again, _ := Render([]App{{TeamID: "23P64LQTZD", BundleID: "com.acme.mobile", Package: "com.acme.mobile", Fingerprints: []string{fpA}}})
	for p := range files {
		if string(files[p]) != string(again[p]) {
			t.Errorf("%s is not byte-stable across renders", p)
		}
	}
	if only, _ := Render([]App{{Package: "com.acme.mobile", Fingerprints: []string{fpA}}}); len(only) != 1 || only[AssetLinksPath] == nil {
		t.Errorf("an android-only app must render only assetlinks.json: %v", only)
	}
}

func TestDebugKeystoreFingerprint(t *testing.T) {
	const keytool = `Alias name: androiddebugkey
Certificate fingerprints:
	 SHA1: 5E:8F:16:06:2E:A3:CD:2C:4A:0D:54:78:76:BA:A6:F3:8C:AB:F6:25
	 SHA256: ` + "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99" + `
Signature algorithm name: SHA256withRSA
`
	var gotArgs []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return []byte(keytool), nil
	}
	fp, err := DebugKeystoreFingerprint(context.Background(), run, "android/app/debug.keystore")
	if err != nil || fp != fpA {
		t.Fatalf("fp = %q, %v; want %q", fp, err, fpA)
	}
	if strings.Join(gotArgs, " ") != "keytool -list -v -keystore android/app/debug.keystore -alias androiddebugkey -storepass android" {
		t.Errorf("keytool invoked as %v", gotArgs)
	}
	if _, err := DebugKeystoreFingerprint(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		return []byte("no fingerprint here"), nil
	}, "k"); err == nil {
		t.Error("a keytool output with no SHA-256 line was accepted")
	}
	if _, err := DebugKeystoreFingerprint(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("keytool: not found")
	}, "k"); err == nil {
		t.Error("a keytool failure was swallowed")
	}
}
