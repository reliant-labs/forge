package templates

import (
	"encoding/json"
	"regexp"
	"testing"
)

var (
	uriSchemeRE   = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)
	androidPkgRE  = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	iosBundleIDRE = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)
)

// Project names allow uppercase, '_' and '-'; none of those is legal in all of
// a URI scheme, an iOS bundle id and an Android package segment. A bad scheme
// is registered by nobody, so the app silently stays on the mock provider.
func TestAppJSONNativeIdentityIsValidForEveryProjectName(t *testing.T) {
	for _, tc := range []struct {
		project, frontend string
		wantID            string
	}{
		{"acme", "mobile", "com.acme.mobile"},
		{"MyApp", "mobile", "com.myapp.mobile"},
		{"my_app", "mobile", "com.myapp.mobile"},
		{"my-app", "Mobile_App", "com.myapp.mobileapp"},
		{"9lives", "2fa", "com.app9lives.app2fa"},
		{"new", "native", "com.newapp.nativeapp"},
	} {
		t.Run(tc.project+"/"+tc.frontend, func(t *testing.T) {
			b, err := FrontendTemplates().Render("react-native/app.json.tmpl", FrontendTemplateData{FrontendName: tc.frontend, ProjectName: tc.project})
			if err != nil {
				t.Fatal(err)
			}
			var app struct {
				Expo struct {
					Scheme  string
					IOS     struct{ BundleIdentifier string }
					Android struct{ Package string }
				}
			}
			if err := json.Unmarshal(b, &app); err != nil {
				t.Fatalf("app.json is not valid JSON: %v\n%s", err, b)
			}
			e := app.Expo
			if e.IOS.BundleIdentifier != tc.wantID || e.Android.Package != tc.wantID {
				t.Errorf("bundle id %q / android package %q, want both %q", e.IOS.BundleIdentifier, e.Android.Package, tc.wantID)
			}
			if e.Scheme != tc.wantID {
				t.Errorf("expo.scheme = %q, want the single reverse-DNS id %q", e.Scheme, tc.wantID)
			}
			if !uriSchemeRE.MatchString(e.Scheme) {
				t.Errorf("scheme %q is not a valid URI scheme", e.Scheme)
			}
			if !androidPkgRE.MatchString(e.Android.Package) {
				t.Errorf("android package %q is not valid", e.Android.Package)
			}
			if !iosBundleIDRE.MatchString(e.IOS.BundleIdentifier) {
				t.Errorf("bundle id %q is not valid", e.IOS.BundleIdentifier)
			}
		})
	}
}

// The dev IdP derives its instance domain from InstanceName; Zitadel refuses
// to boot on a project name a domain cannot hold (observed: "Auth_Acc2" fails
// migration 03_default_instance with Errors.Instance.Domain.InvalidCharacter).
func TestIDPStepsInstanceNameIsADNSLabel(t *testing.T) {
	label := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	for _, project := range []string{"acme", "Auth_Acc2", "my_app", "My--App_", "_x", "9lives"} {
		b, err := ProjectTemplates().Render("idp-steps.yaml.tmpl", struct{ ProjectName string }{project})
		if err != nil {
			t.Fatalf("%s: render: %v", project, err)
		}
		m := regexp.MustCompile(`(?m)^\s*InstanceName:\s*(\S+)\s*$`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: no InstanceName in idp-steps.yaml", project)
		}
		if got := string(m[1]); !label.MatchString(got) {
			t.Errorf("%s: InstanceName %q is not a DNS label", project, got)
		}
	}
	if got := DNSLabel("Auth_Acc2"); got != "auth-acc2" {
		t.Errorf("DNSLabel(Auth_Acc2) = %q, want auth-acc2", got)
	}
}
