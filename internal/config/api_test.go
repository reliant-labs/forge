package config

import "testing"

// TestAPIConfigRoundTrip verifies the new api: block parses correctly
// under LoadStrict and that an unknown sub-key (e.g. typo) is reported
// with a suggestion. This is a load-bearing sanity check: forge.yaml
// validation rides on the reflect walk picking up the APIConfig fields.
func TestAPIConfigRoundTrip(t *testing.T) {
	data := []byte(`name: test
module_path: github.com/foo/bar
version: 0.1.0
environments:
  - name: dev
    type: local
api:
  openapi: true
  rest: false
`)
	cfg, err := LoadStrict(data, "t.yaml")
	if err != nil {
		t.Fatalf("LoadStrict: %v", err)
	}
	if !cfg.API.OpenAPI {
		t.Errorf("api.openapi = false, want true")
	}
	if cfg.API.REST {
		t.Errorf("api.rest = true, want false")
	}
}

// TestAPIConfigRejectsUnknownKey ensures a typo in an api: sub-field
// is caught by the unknown-keys walker so users get an actionable
// validation error rather than a silently-ignored setting.
func TestAPIConfigRejectsUnknownKey(t *testing.T) {
	data := []byte(`name: test
module_path: github.com/foo/bar
version: 0.1.0
environments:
  - name: dev
    type: local
api:
  openpi: true
`)
	_, err := LoadStrict(data, "t.yaml")
	if err == nil {
		t.Fatal("expected validation error for unknown key 'openpi', got nil")
	}
	if !contains(err.Error(), "openpi") {
		t.Errorf("error should mention the unknown key 'openpi': %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
