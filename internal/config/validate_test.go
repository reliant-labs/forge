package config

import (
	"errors"
	"strings"
	"testing"
)

// validBaseYAML is a minimal forge.yaml that satisfies all required-field
// rules. Tests start from this and inject the fault under test so that
// other validation phases stay green and failures are unambiguous.
const validBaseYAML = `name: demo
module_path: github.com/example/demo
version: 0.1.0
hot_reload: false
services:
  - name: api
    type: go_service
    path: handlers/api
database:
  driver: postgres
  migrations_dir: db/migrations
ci:
  provider: github
docker:
  registry: ghcr.io
k8s:
  kcl_dir: deploy/kcl
lint:
  contract: true
contracts:
  strict: true
auth:
  provider: none
docs: {}
`

func TestLoadStrict_ValidConfig(t *testing.T) {
	cfg, err := LoadStrict([]byte(validBaseYAML), "forge.yaml")
	if err != nil {
		t.Fatalf("expected clean load, got: %v", err)
	}
	if cfg.Name != "demo" || cfg.ModulePath != "github.com/example/demo" {
		t.Errorf("unexpected parse result: %+v", cfg)
	}
}

func TestLoadStrict_UnknownKey_WithCloseMatch(t *testing.T) {
	in := strings.Replace(validBaseYAML, "auth:", "auht:", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "unknown key", "auht", "did you mean", "auth") {
		t.Errorf("expected typo suggestion in error, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_UnknownKey_NoCloseMatch(t *testing.T) {
	in := validBaseYAML + "completely_unrelated_key: 42\n"
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "unknown key", "completely_unrelated_key") {
		t.Errorf("expected unknown-key error, got:\n%s", ve.Error())
	}
	if strings.Contains(ve.Error(), "did you mean") {
		t.Errorf("expected no suggestion for distant key, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_MultipleUnknownKeys(t *testing.T) {
	in := validBaseYAML + "auht: x\nservces: y\n" //nolint:misspell // intentional typo for suggestion test
	// Replace the real auth: block first so we don't have a duplicate
	// issue from a still-valid `auth: none` while testing the typo.
	in = strings.Replace(in, "auth:\n  provider: none\n", "", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "auht", "auth", "servces", "services") { //nolint:misspell // checks suggestion output
		t.Errorf("expected both typos with suggestions, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_MissingRequired_ModulePath(t *testing.T) {
	in := strings.Replace(validBaseYAML, "module_path: github.com/example/demo\n", "", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "module_path", "required") {
		t.Errorf("expected module_path required error, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_MissingRequired_Multiple(t *testing.T) {
	in := strings.Replace(validBaseYAML, "name: demo\n", "", 1)
	in = strings.Replace(in, "module_path: github.com/example/demo\n", "", 1)
	in = strings.Replace(in, "  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - type: go_service\n    path: handlers/api\n", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	got := ve.Error()
	if !strings.Contains(got, "'name' is required") {
		t.Errorf("expected 'name' required, got:\n%s", got)
	}
	if !strings.Contains(got, "'module_path' is required") {
		t.Errorf("expected 'module_path' required, got:\n%s", got)
	}
	if !strings.Contains(got, "services[0].name is required") {
		t.Errorf("expected services[0].name required, got:\n%s", got)
	}
}

func TestLoadStrict_TypeMismatch(t *testing.T) {
	// hot_reload is a bool; pass a string to surface a yaml type error.
	in := strings.Replace(validBaseYAML, "hot_reload: false", `hot_reload: "not-a-bool"`, 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "cannot unmarshal") {
		t.Errorf("expected type-mismatch error mentioning unmarshal, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_NestedUnknownKey(t *testing.T) {
	// services[0] has bogus subkey "naem" — should be detected at the
	// nested level with a path-prefixed message.
	in := strings.Replace(validBaseYAML, "  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - name: api\n    type: go_service\n    path: handlers/api\n    naem: typo\n", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "services[0].naem", "did you mean", "name") {
		t.Errorf("expected nested-path unknown-key + suggestion, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_ServiceMissingName(t *testing.T) {
	// services[].path is loader-defaulted, but services[].name is not.
	in := strings.Replace(validBaseYAML, "  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - type: go_service\n    path: handlers/api\n", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "services[0].name is required") {
		t.Errorf("expected services[0].name required, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_InvalidModulePath(t *testing.T) {
	in := strings.Replace(validBaseYAML, "module_path: github.com/example/demo", "module_path: notamodule", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "does not look like a Go module path") {
		t.Errorf("expected module-path shape warning, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_FourIssuesAtOnce(t *testing.T) {
	// Smoke test mirroring the CLI smoke: 3 typos + 1 missing required
	// field should all surface in a single error.
	in := strings.Replace(validBaseYAML, "auth:\n  provider: none\n", "auht:\n  provider: none\n", 1)
	in = strings.Replace(in, "services:", "servces:", 1) //nolint:misspell // intentional typo for suggestion test
	in = strings.Replace(in, "database:", "databse:", 1)
	in = strings.Replace(in, "module_path: github.com/example/demo\n", "", 1)

	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	got := ve.Error()
	for _, want := range []string{"auht", "servces", "databse", "module_path"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in error, got:\n%s", want, got)
		}
	}
	for _, suggestion := range []string{"auth", "services", "database"} {
		if !strings.Contains(got, suggestion) {
			t.Errorf("expected suggestion %q, got:\n%s", suggestion, got)
		}
	}
}

// TestLoadStrict_UnknownKeyClassification is the table-driven matrix for
// the three unknown-key outcomes:
//
//  1. removed key  → specific migration hint, NO Levenshtein suggestion
//  2. typo'd key   → "did you mean" suggestion
//  3. distant key  → plain unknown-key error, no suggestion, no hint
//
// Removed keys must win over suggestions: an agent that sees
// "did you mean 'kcl_dir'?" for k8s.provider would rename instead of
// migrating.
func TestLoadStrict_UnknownKeyClassification(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(string) string // injects the fault into validBaseYAML
		wantSubstr []string            // all must appear in the error
		notSubstr  []string            // none may appear in the error
	}{
		{
			name: "removed key k8s.provider gets migration hint",
			mutate: func(in string) string {
				return strings.Replace(in, "k8s:\n  kcl_dir: deploy/kcl\n",
					"k8s:\n  kcl_dir: deploy/kcl\n  provider: k3d\n", 1)
			},
			wantSubstr: []string{
				`"k8s.provider" was removed in`,
				"forge.K8sCluster",
				"migrations/environments-to-kcl",
			},
			notSubstr: []string{"did you mean"},
		},
		{
			name: "removed nested key services[].dev_target gets migration hint",
			mutate: func(in string) string {
				// Prepend a service carrying the removed key; the indexed
				// path (services[0].dev_target) must normalize to the
				// services[].dev_target map entry.
				return strings.Replace(in, "services:\n",
					"services:\n  - name: web\n    type: go_service\n    path: handlers/web\n    dev_target: host\n", 1)
			},
			wantSubstr: []string{
				`"services[0].dev_target" was removed in`,
				"deploy:",
				"migrations/dev-target-to-kcl-deploy",
			},
			notSubstr: []string{"did you mean"},
		},
		{
			name: "removed key binaries[].kind gets migration hint",
			mutate: func(in string) string {
				return in + "binaries:\n  - name: proxy\n    path: cmd/proxy.go\n    kind: cron\n"
			},
			wantSubstr: []string{
				`"binaries[0].kind" was removed in`,
				"long-running",
			},
			notSubstr: []string{"did you mean"},
		},
		{
			name: "typo'd key gets a did-you-mean suggestion",
			mutate: func(in string) string {
				return strings.Replace(in, "auth:", "auht:", 1)
			},
			wantSubstr: []string{"unknown key", "auht", "did you mean", "auth"},
			notSubstr:  []string{"was removed in"},
		},
		{
			name: "distant key gets plain unknown-key error",
			mutate: func(in string) string {
				return in + "completely_unrelated_key: 42\n"
			},
			wantSubstr: []string{"unknown key", "completely_unrelated_key"},
			notSubstr:  []string{"did you mean", "was removed in"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadStrict([]byte(tc.mutate(validBaseYAML)), "forge.yaml")
			ve := requireValidationError(t, err)
			got := ve.Error()
			for _, want := range tc.wantSubstr {
				if !strings.Contains(got, want) {
					t.Errorf("expected %q in error, got:\n%s", want, got)
				}
			}
			for _, not := range tc.notSubstr {
				if strings.Contains(got, not) {
					t.Errorf("did not expect %q in error, got:\n%s", not, got)
				}
			}
		})
	}
}

// TestLoadStrict_DeprecatedEnvironmentsStillLoads pins the whitelist
// behaviour: the removed top-level `environments` block is silently
// skipped (mid-migration projects must keep loading), NOT reported as
// an unknown or removed key.
func TestLoadStrict_DeprecatedEnvironmentsStillLoads(t *testing.T) {
	in := validBaseYAML + "environments:\n  - name: dev\n    type: local\n"
	if _, err := LoadStrict([]byte(in), "forge.yaml"); err != nil {
		t.Fatalf("expected deprecated 'environments' block to load cleanly, got: %v", err)
	}
}

func TestNormalizeKeyPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"services[0].dev_target", "services[].dev_target"},
		{"services[12].dev_target", "services[].dev_target"},
		{"k8s.provider", "k8s.provider"},
		{"binaries[3].kind", "binaries[].kind"},
	}
	for _, c := range cases {
		if got := normalizeKeyPath(c.in); got != c.want {
			t.Errorf("normalizeKeyPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		// transposition counts as 2 substitutions in classic Levenshtein
		{"auth", "auht", 2},
		{"environments", "environments", 0},
		// typo: 'enviornments' vs 'environments' is a single transposition,
		// i.e. distance 2 in classic Levenshtein (no transposition op).
		{"enviornments", "environments", 2}, //nolint:misspell // intentional typo for distance test
		{"hello", "world", 4},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestClosestMatch_Threshold(t *testing.T) {
	cands := []string{"auth", "environments", "services"}
	if got := closestMatch("auht", cands); got != "auth" {
		t.Errorf("closestMatch auht: got %q want auth", got)
	}
	if got := closestMatch("totally_different_key", cands); got != "" {
		t.Errorf("expected no match for distant key, got %q", got)
	}
	// 'enviornments' (12 chars) vs 'environments' (12 chars) is distance 2 —
	// well within the 3-char tolerance for length >= 8.
	if got := closestMatch("environments", cands); got != "environments" {
		t.Errorf("closestMatch environments: got %q want environments", got)
	}
}

func requireValidationError(t *testing.T, err error) *ValidationError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	return ve
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
