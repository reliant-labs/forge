package config

import (
	"errors"
	"strconv"
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

// TestLoadStrict_FrontendOutput_ValidValues_Accepted covers the three
// supported `frontends[].output` values: "static" (the new default),
// "standalone" (Node sidecar, the legacy default), and "server" (full
// Next.js dev+prod). Empty (unset) is also accepted — the scaffold
// canonicalises that to "static" downstream.
func TestLoadStrict_FrontendOutput_ValidValues_Accepted(t *testing.T) {
	cases := []string{"static", "standalone", "server"}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			in := validBaseYAML + "frontends:\n  - name: web\n    type: nextjs\n    path: frontends/web\n    port: 3000\n    output: " + value + "\n"
			if _, err := LoadStrict([]byte(in), "forge.yaml"); err != nil {
				t.Errorf("expected output=%q to validate, got: %v", value, err)
			}
		})
	}
}

// TestLoadStrict_FrontendOutput_InvalidValue_Rejected pins the
// validator's error shape so anyone adding a new mode (e.g. "edge")
// must remember to extend the validator at the same time. Catching the
// invalid value at load time turns a runtime template fall-through
// into a clear actionable error.
func TestLoadStrict_FrontendOutput_InvalidValue_Rejected(t *testing.T) {
	in := validBaseYAML + "frontends:\n  - name: web\n    type: nextjs\n    path: frontends/web\n    port: 3000\n    output: edge\n"
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "frontends[0].output", "invalid", "static", "standalone", "server") {
		t.Errorf("expected output validation error mentioning the supported values, got:\n%s", ve.Error())
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

// TestLoadStrict_ServiceNameCollision_AfterNormalisation covers the
// validateServices lint: two service entries whose canonical Go-package
// form is the same would race for the same scaffold directory. The
// lint must surface that BEFORE the user discovers it via a confusing
// downstream codegen error.
func TestLoadStrict_ServiceNameCollision_AfterNormalisation(t *testing.T) {
	in := strings.Replace(validBaseYAML,
		"  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - name: admin-server\n    type: go_service\n    path: handlers/admin-server\n"+
			"  - name: admin_server\n    type: go_service\n    path: handlers/admin_server\n",
		1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	got := ve.Error()
	// Post-2026-06-08 snake-canonicalisation: "admin-server" and
	// "admin_server" both normalize to "admin_server" (hyphen → underscore),
	// so the collision message names the snake form.
	if !containsAll(got, "collides", "admin_server") {
		t.Errorf("expected collision message naming admin_server, got:\n%s", got)
	}
}

// TestLoadStrict_ServiceName_ReservedWord_Rejected covers names whose
// canonical Go-package form lands on a Go keyword / predeclared
// identifier. The downstream symptom is a broken `package <reserved>`
// declaration that fails compilation deep in the codegen output; the
// lint catches it at forge.yaml-parse time.
func TestLoadStrict_ServiceName_ReservedWord_Rejected(t *testing.T) {
	in := strings.Replace(validBaseYAML,
		"  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - name: select\n    type: go_service\n    path: handlers/select\n",
		1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "reserved word", "select") {
		t.Errorf("expected reserved-word rejection, got:\n%s", ve.Error())
	}
}

// TestLoadStrict_ServiceName_LeadingDigit_Rejected covers names whose
// canonical Go-package form starts with a digit. Go package idents must
// begin with a letter or underscore; the lint guards against the
// surprising downstream parse error.
func TestLoadStrict_ServiceName_LeadingDigit_Rejected(t *testing.T) {
	in := strings.Replace(validBaseYAML,
		"  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - name: 2fast\n    type: go_service\n    path: handlers/2fast\n",
		1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "invalid Go package", "2fast") {
		t.Errorf("expected leading-digit rejection, got:\n%s", ve.Error())
	}
}

// TestLoadStrict_ServiceName_PunctuationSurvivingNormalisation_Rejected
// covers names containing punctuation that the canonical
// ServicePackage transform leaves intact (dots, slashes). Those
// characters can never produce a legal package directory.
func TestLoadStrict_ServiceName_PunctuationSurvivingNormalisation_Rejected(t *testing.T) {
	in := strings.Replace(validBaseYAML,
		"  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - name: \"foo.bar\"\n    type: go_service\n    path: handlers/foo\n",
		1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "invalid Go package", "foo.bar") {
		t.Errorf("expected punctuation rejection, got:\n%s", ve.Error())
	}
}

// TestLoadStrict_ServiceVsBinaryCollision verifies the cross-slice
// collision check: a service and a binary with names that normalise to
// the same Go package would write to the same scaffold directory.
func TestLoadStrict_ServiceVsBinaryCollision(t *testing.T) {
	in := strings.Replace(validBaseYAML,
		"  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - name: gateway\n    type: go_service\n    path: handlers/gateway\n",
		1)
	in += "binaries:\n  - name: Gateway\n    path: cmd/gateway.go\n"
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "collides", "gateway") {
		t.Errorf("expected cross-slice collision, got:\n%s", ve.Error())
	}
}

// TestLoadStrict_ValidServiceVariants_Accepted is the positive case for
// the lint: hyphenated, snake_case, and plain-lowercase names all coexist
// peacefully as long as their canonical forms differ.
func TestLoadStrict_ValidServiceVariants_Accepted(t *testing.T) {
	in := strings.Replace(validBaseYAML,
		"  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - name: api\n    type: go_service\n    path: handlers/api\n"+
			"  - name: admin-server\n    type: go_service\n    path: handlers/admin-server\n"+
			"  - name: billing_v2\n    type: go_service\n    path: handlers/billing_v2\n",
		1)
	if _, err := LoadStrict([]byte(in), "forge.yaml"); err != nil {
		t.Fatalf("expected clean load, got: %v", err)
	}
}

// TestLoadStrict_NestedUnknownKey_LineAndPath pins down both the
// dot-notation path and the YAML line number for an unknown nested
// key. Earlier reports observed the wrong line / wrong path in
// nested cases, so this test asserts both invariants explicitly: the
// reported line must be the literal line of the offending key in the
// input, and the dot-path must match where the key actually lives in
// the YAML tree.
func TestLoadStrict_NestedUnknownKey_LineAndPath(t *testing.T) {
	// Inject an unknown subkey "provider: k3d" inside k8s: at a known
	// position so we can compute the expected line precisely.
	in := strings.Replace(validBaseYAML,
		"k8s:\n  kcl_dir: deploy/kcl\n",
		"k8s:\n  provider: k3d\n  kcl_dir: deploy/kcl\n",
		1)
	wantLine := lineOf(t, in, "  provider: k3d")
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	got := ve.Error()
	if !strings.Contains(got, `"k8s.provider"`) {
		t.Errorf("expected key path 'k8s.provider' in error, got:\n%s", got)
	}
	// Standard compiler/editor format: `path:line:col:`. Lets an LLM
	// (or human in vim/emacs/VS Code) jump straight to the offending
	// token without grep round-trips.
	wantLineMarker := "forge.yaml:" + strconv.Itoa(wantLine) + ":"
	if !strings.Contains(got, wantLineMarker) {
		t.Errorf("expected %q in error (the literal line of `provider: k3d`), got:\n%s", wantLineMarker, got)
	}
}

// TestLoadStrict_RemovedSchemaKey_K8sProvider asserts the
// migration-aware "Fix:" suggestion for a key the forge schema once
// owned and has since dropped. The generic "rename or remove" framing
// misleads users into hunting for a typo when the real answer is "this
// field moved to KCL".
func TestLoadStrict_RemovedSchemaKey_K8sProvider(t *testing.T) {
	in := strings.Replace(validBaseYAML,
		"k8s:\n  kcl_dir: deploy/kcl\n",
		"k8s:\n  provider: k3d\n  kcl_dir: deploy/kcl\n",
		1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	got := ve.Error()
	if !containsAll(got, `"k8s.provider"`, "removed", "K8sCluster") {
		t.Errorf("expected schema-drift hint naming K8sCluster, got:\n%s", got)
	}
	// The migration hint must replace the generic suggestion — not
	// stack alongside it. A "did you mean kcl_dir?" tail would be
	// misleading here.
	if strings.Contains(got, "did you mean") {
		t.Errorf("expected schema-drift hint to suppress typo suggestion, got:\n%s", got)
	}
}

// TestLoadStrict_StackDeployProvider_NotFalsePositive guards against
// regressions where the `provider` key inside `stack.deploy:` could
// accidentally trip the removedSchemaKeys lookup for the old
// `k8s.provider` location. The validator stores the qualified path as
// `stack.deploy.provider` (not `k8s.provider`), so the lookup must
// match exact paths only — never an unqualified suffix.
//
// This is the cp-forge dogfood shape: `stack.deploy.target: k8s` plus
// a sibling `provider: k3d`. Pre-2026-06-08 there was a report (since
// proven phantom) that the validator misread this as `k8s.provider`;
// pinning the clean-load behavior here keeps any future path-construction
// refactor from regressing the case.
func TestLoadStrict_StackDeployProvider_NotFalsePositive(t *testing.T) {
	in := validBaseYAML + `stack:
  deploy:
    target: k8s
    provider: k3d
    registry: ghcr.io
`
	cfg, err := LoadStrict([]byte(in), "forge.yaml")
	if err != nil {
		t.Fatalf("clean load expected — `stack.deploy.provider` must not be confused with removed `k8s.provider` key. err=%v", err)
	}
	if cfg.Stack.Deploy.Provider != "k3d" {
		t.Errorf("stack.deploy.provider = %q, want %q", cfg.Stack.Deploy.Provider, "k3d")
	}
}

// TestLoadStrict_RemovedSchemaKey_ServiceDevTarget covers the slice-
// wildcard arm of the removed-keys table: services[N].dev_target
// should resolve to the services[*].dev_target migration hint for any
// index N.
func TestLoadStrict_RemovedSchemaKey_ServiceDevTarget(t *testing.T) {
	in := strings.Replace(validBaseYAML,
		"  - name: api\n    type: go_service\n    path: handlers/api\n",
		"  - name: api\n    type: go_service\n    path: handlers/api\n    dev_target: host\n",
		1)
	_, err := LoadStrict([]byte(in), "forge.yaml")
	ve := requireValidationError(t, err)
	got := ve.Error()
	if !containsAll(got, "services[0].dev_target", "removed", "forge.Service.deploy") {
		t.Errorf("expected dev_target migration hint, got:\n%s", got)
	}
}

// TestWildcardIndices guards the helper that normalises [N] -> [*]
// before lookup in removedSchemaKeys. Edge cases that previously
// tripped naive string-replace implementations are pinned here.
func TestWildcardIndices(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"k8s.provider", "k8s.provider"},
		{"services[0].dev_target", "services[*].dev_target"},
		{"services[12].webhooks[3].name", "services[*].webhooks[*].name"},
		// Non-numeric brackets must pass through (we only want array
		// indices, not e.g. map keys that happen to be bracketed).
		{"foo[bar].baz", "foo[bar].baz"},
		// Empty brackets stay empty (no index inside).
		{"foo[].bar", "foo[].bar"},
	}
	for _, c := range cases {
		if got := wildcardIndices(c.in); got != c.want {
			t.Errorf("wildcardIndices(%q) = %q, want %q", c.in, got, c.want)
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

// lineOf returns the 1-based line number of the first line in input
// whose trimmed content equals (or starts with) marker. Test helper
// used to pin "the validator reports the exact line of <X>" without
// hard-coding fragile line numbers that drift as validBaseYAML
// evolves.
func lineOf(t *testing.T, input, marker string) int {
	t.Helper()
	for i, line := range strings.Split(input, "\n") {
		if line == marker || strings.HasPrefix(strings.TrimRight(line, "\r"), marker) {
			return i + 1
		}
	}
	t.Fatalf("marker %q not found in input", marker)
	return 0
}

