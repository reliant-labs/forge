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

// baseComponents is the single api server component the validBaseYAML
// fixtures imply; injected via LoadStrict's variadic components arg now
// that components live outside forge.yaml.
func baseComponents() []ComponentConfig {
	return []ComponentConfig{{Name: "api", Kind: "server", Path: "handlers/api"}}
}

func TestLoadStrict_ValidConfig(t *testing.T) {
	cfg, err := LoadStrict([]byte(validBaseYAML), "forge.yaml", baseComponents()...)
	if err != nil {
		t.Fatalf("expected clean load, got: %v", err)
	}
	if cfg.Name != "demo" || cfg.ModulePath != "github.com/example/demo" {
		t.Errorf("unexpected parse result: %+v", cfg)
	}
}

func TestLoadStrict_UnknownKey_WithCloseMatch(t *testing.T) {
	in := strings.Replace(validBaseYAML, "auth:", "auht:", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "unknown key", "auht", "did you mean", "auth") {
		t.Errorf("expected typo suggestion in error, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_UnknownKey_NoCloseMatch(t *testing.T) {
	in := validBaseYAML + "completely_unrelated_key: 42\n"
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "unknown key", "completely_unrelated_key") {
		t.Errorf("expected unknown-key error, got:\n%s", ve.Error())
	}
	if strings.Contains(ve.Error(), "did you mean") {
		t.Errorf("expected no suggestion for distant key, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_MultipleUnknownKeys(t *testing.T) {
	// Two typo'd top-level keys, each near a still-present forge.yaml key:
	// `auht`→`auth` and `databse`→`database`. (`components` is no longer a
	// forge.yaml key, so the prior `componnts`→`components` pairing moved to
	// `databse`→`database`, which still exercises the multi-typo path.)
	in := validBaseYAML + "auht: x\ndatabse: y\n" //nolint:misspell // intentional typo for suggestion test
	// Drop the real auth:/database: blocks first so we don't get a duplicate
	// issue from the still-valid originals while testing the typos.
	in = strings.Replace(in, "auth:\n  provider: none\n", "", 1)
	in = strings.Replace(in, "database:\n  driver: postgres\n  migrations_dir: db/migrations\n", "", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "auht", "auth", "databse", "database") { //nolint:misspell // checks suggestion output
		t.Errorf("expected both typos with suggestions, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_ConfigGuard_InvalidEnforceValue(t *testing.T) {
	in := validBaseYAML + "config:\n  enforce_typed_access: nonsense\n"
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "config.enforce_typed_access", "nonsense", "off", "warn", "error") {
		t.Errorf("expected enum-rejection error listing valid values, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_ConfigGuard_ValidValues(t *testing.T) {
	for _, v := range []string{"off", "warn", "error", "warning", "Error"} {
		in := validBaseYAML + "config:\n  enforce_typed_access: " + v + "\n"
		if _, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...); err != nil {
			t.Errorf("value %q should load clean, got: %v", v, err)
		}
	}
}

func TestLoadStrict_ConfigGuard_AbsentDefaultsToWarn(t *testing.T) {
	cfg, err := LoadStrict([]byte(validBaseYAML), "forge.yaml", baseComponents()...)
	if err != nil {
		t.Fatalf("clean load: %v", err)
	}
	if got := cfg.Config.EffectiveEnforceTypedAccess(); got != EnforceTypedAccessWarn {
		t.Errorf("absent config: block → %q, want warn", got)
	}
	if got := cfg.Config.EffectiveLoaderPackage(); got != DefaultLoaderPackage {
		t.Errorf("absent loader_package → %q, want %q", got, DefaultLoaderPackage)
	}
}

func TestLoadStrict_MissingRequired_ModulePath(t *testing.T) {
	in := strings.Replace(validBaseYAML, "module_path: github.com/example/demo\n", "", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "module_path", "required") {
		t.Errorf("expected module_path required error, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_MissingRequired_Multiple(t *testing.T) {
	in := strings.Replace(validBaseYAML, "name: demo\n", "", 1)
	in = strings.Replace(in, "module_path: github.com/example/demo\n", "", 1)
	// The faulty (nameless) component is now injected via the variadic arg.
	_, err := LoadStrict([]byte(in), "forge.yaml", ComponentConfig{Kind: "server", Path: "handlers/api"})
	ve := requireValidationError(t, err)
	got := ve.Error()
	if !strings.Contains(got, "'name' is required") {
		t.Errorf("expected 'name' required, got:\n%s", got)
	}
	if !strings.Contains(got, "'module_path' is required") {
		t.Errorf("expected 'module_path' required, got:\n%s", got)
	}
	if !strings.Contains(got, "components[0].name is required") {
		t.Errorf("expected services[0].name required, got:\n%s", got)
	}
}

func TestLoadStrict_TypeMismatch(t *testing.T) {
	// hot_reload is a bool; pass a string to surface a yaml type error.
	in := strings.Replace(validBaseYAML, "hot_reload: false", `hot_reload: "not-a-bool"`, 1)
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "cannot unmarshal") {
		t.Errorf("expected type-mismatch error mentioning unmarshal, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_NestedUnknownKey(t *testing.T) {
	// database: has bogus subkey "migrations_dur" — should be detected at
	// the nested level with a path-prefixed message and a suggestion.
	// (Components moved out of forge.yaml, so the nested-walk path is now
	// exercised against a still-YAML-parsed block.)
	in := strings.Replace(validBaseYAML, "  migrations_dir: db/migrations\n",
		"  migrations_dir: db/migrations\n  migrations_dur: typo\n", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "database.migrations_dur", "did you mean", "migrations_dir") {
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
			if _, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...); err != nil {
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
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "frontends[0].output", "invalid", "static", "standalone", "server") {
		t.Errorf("expected output validation error mentioning the supported values, got:\n%s", ve.Error())
	}
}

// TestLoadStrict_FrontendBasePath_ValidValues_Accepted covers the
// accepted shapes for `frontends[].base_path`: a "/"-prefixed path with
// no trailing slash, segments limited to [A-Za-z0-9._-]. Multi-segment
// prefixes are legal (an app proxied two levels deep).
func TestLoadStrict_FrontendBasePath_ValidValues_Accepted(t *testing.T) {
	cases := []string{"/admin", "/internal/admin", "/v2.1_beta", "/app-shell"}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			in := validBaseYAML + "frontends:\n  - name: web\n    type: nextjs\n    path: frontends/web\n    port: 3000\n    base_path: " + value + "\n"
			if _, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...); err != nil {
				t.Errorf("expected base_path=%q to validate, got: %v", value, err)
			}
		})
	}
}

// TestLoadStrict_FrontendBasePath_InvalidValues_Rejected pins the shape
// contract: the literal is spliced verbatim into next.config.ts and
// generated TypeScript string literals, so anything outside the strict
// grammar must fail at forge.yaml load time — not as a silently broken
// deploy. Values are written in their YAML-quoted form where quoting is
// needed for the YAML parser to see the intended string.
func TestLoadStrict_FrontendBasePath_InvalidValues_Rejected(t *testing.T) {
	cases := []struct {
		name  string
		value string // YAML form (quoted where needed)
	}{
		{"no_leading_slash", `admin`},
		{"trailing_slash", `/admin/`},
		{"bare_root", `"/"`},
		{"embedded_space", `"/ad min"`},
		{"double_slash", `"/admin//x"`},
		{"percent_escape", `"/a%2Fb"`},
		{"quote_injection", `"/ad\"min"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validBaseYAML + "frontends:\n  - name: web\n    type: nextjs\n    path: frontends/web\n    port: 3000\n    base_path: " + tc.value + "\n"
			_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
			ve := requireValidationError(t, err)
			if !containsAll(ve.Error(), "frontends[0].base_path", "invalid") {
				t.Errorf("expected base_path validation error for %s, got:\n%s", tc.value, ve.Error())
			}
		})
	}
}

func TestLoadStrict_ServiceMissingName(t *testing.T) {
	// components[].path is loader-defaulted, but components[].name is not.
	// The nameless component is now injected via the variadic arg.
	_, err := LoadStrict([]byte(validBaseYAML), "forge.yaml", ComponentConfig{Kind: "server", Path: "handlers/api"})
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "components[0].name is required") {
		t.Errorf("expected services[0].name required, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_InvalidModulePath(t *testing.T) {
	in := strings.Replace(validBaseYAML, "module_path: github.com/example/demo", "module_path: notamodule", 1)
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "does not look like a Go module path") {
		t.Errorf("expected module-path shape warning, got:\n%s", ve.Error())
	}
}

func TestLoadStrict_FourIssuesAtOnce(t *testing.T) {
	// Smoke test mirroring the CLI smoke: 3 typos + 1 missing required
	// field should all surface in a single error.
	in := strings.Replace(validBaseYAML, "auth:\n  provider: none\n", "auht:\n  provider: none\n", 1)
	// `components` is no longer a forge.yaml key, so the third typo targets
	// another still-present top-level key: `docker`→`dockr`.
	in = strings.Replace(in, "docker:", "dockr:", 1)
	in = strings.Replace(in, "database:", "databse:", 1)
	in = strings.Replace(in, "module_path: github.com/example/demo\n", "", 1)

	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	got := ve.Error()
	for _, want := range []string{"auht", "dockr", "databse", "module_path"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in error, got:\n%s", want, got)
		}
	}
	for _, suggestion := range []string{"auth", "docker", "database"} {
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
	_, err := LoadStrict([]byte(validBaseYAML), "forge.yaml",
		ComponentConfig{Name: "admin-server", Kind: "server", Path: "handlers/admin-server"},
		ComponentConfig{Name: "admin_server", Kind: "server", Path: "handlers/admin_server"},
	)
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
	_, err := LoadStrict([]byte(validBaseYAML), "forge.yaml",
		ComponentConfig{Name: "select", Kind: "server", Path: "handlers/select"},
	)
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
	_, err := LoadStrict([]byte(validBaseYAML), "forge.yaml",
		ComponentConfig{Name: "2fast", Kind: "server", Path: "handlers/2fast"},
	)
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
	_, err := LoadStrict([]byte(validBaseYAML), "forge.yaml",
		ComponentConfig{Name: "foo.bar", Kind: "server", Path: "handlers/foo"},
	)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "invalid Go package", "foo.bar") {
		t.Errorf("expected punctuation rejection, got:\n%s", ve.Error())
	}
}

// TestLoadStrict_ServiceVsBinaryCollision verifies the cross-slice
// collision check: a service and a binary with names that normalise to
// the same Go package would write to the same scaffold directory.
func TestLoadStrict_ServiceVsBinaryCollision(t *testing.T) {
	_, err := LoadStrict([]byte(validBaseYAML), "forge.yaml",
		ComponentConfig{Name: "gateway", Kind: "server", Path: "handlers/gateway"},
		ComponentConfig{Name: "Gateway", Kind: "binary", Path: "cmd/gateway.go"},
	)
	ve := requireValidationError(t, err)
	if !containsAll(ve.Error(), "collides", "gateway") {
		t.Errorf("expected cross-kind collision, got:\n%s", ve.Error())
	}
}

// TestLoadStrict_ValidServiceVariants_Accepted is the positive case for
// the lint: hyphenated, snake_case, and plain-lowercase names all coexist
// peacefully as long as their canonical forms differ.
func TestLoadStrict_ValidServiceVariants_Accepted(t *testing.T) {
	_, err := LoadStrict([]byte(validBaseYAML), "forge.yaml",
		ComponentConfig{Name: "api", Kind: "server", Path: "handlers/api"},
		ComponentConfig{Name: "admin-server", Kind: "server", Path: "handlers/admin-server"},
		ComponentConfig{Name: "billing_v2", Kind: "server", Path: "handlers/billing_v2"},
	)
	if err != nil {
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
	// Inject a genuinely unknown subkey "bogus_knob: 1" inside k8s: at a
	// known position so we can compute the expected line precisely. We use
	// a NEVER-a-real-key name (not a removed key like k8s.provider, which
	// is now a non-fatal warning) so this stays a fatal error and keeps
	// exercising the line/path reporting it was written to pin.
	in := strings.Replace(validBaseYAML,
		"k8s:\n  kcl_dir: deploy/kcl\n",
		"k8s:\n  bogus_knob: 1\n  kcl_dir: deploy/kcl\n",
		1)
	wantLine := lineOf(t, in, "  bogus_knob: 1")
	_, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...)
	ve := requireValidationError(t, err)
	got := ve.Error()
	if !strings.Contains(got, `"k8s.bogus_knob"`) {
		t.Errorf("expected key path 'k8s.bogus_knob' in error, got:\n%s", got)
	}
	// Standard compiler/editor format: `path:line:col:`. Lets an LLM
	// (or human in vim/emacs/VS Code) jump straight to the offending
	// token without grep round-trips.
	wantLineMarker := "forge.yaml:" + strconv.Itoa(wantLine) + ":"
	if !strings.Contains(got, wantLineMarker) {
		t.Errorf("expected %q in error (the literal line of `bogus_knob: 1`), got:\n%s", wantLineMarker, got)
	}
}

// TestLoadStrict_RemovedSchemaKey_K8sProvider asserts the
// migration-aware behaviour for a key the forge schema once owned and
// has since dropped: it is a non-fatal WARNING (fr-57edf33aca) — a
// forge.yaml forge itself wrote must keep loading across a schema
// removal — carrying the migration hint, never a generic "rename or
// remove" typo suggestion that would mislead the user into hunting for
// a misspelling when the real answer is "this field moved to KCL".
func TestLoadStrict_RemovedSchemaKey_K8sProvider(t *testing.T) {
	var sink strings.Builder
	prev := SetConfigWarningSink(&sink)
	defer SetConfigWarningSink(prev)

	in := strings.Replace(validBaseYAML,
		"k8s:\n  kcl_dir: deploy/kcl\n",
		"k8s:\n  provider: k3d\n  kcl_dir: deploy/kcl\n",
		1)
	// Removed keys no longer gate the load: the project must keep running.
	if _, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...); err != nil {
		t.Fatalf("removed key k8s.provider must WARN, not fail the load; got: %v", err)
	}
	got := sink.String()
	if !containsAll(got, `"k8s.provider"`, "removed", "K8sCluster") {
		t.Errorf("expected schema-drift warning naming K8sCluster, got:\n%s", got)
	}
	// The migration hint must replace the generic suggestion — not
	// stack alongside it. A "did you mean kcl_dir?" tail would be
	// misleading here.
	if strings.Contains(got, "did you mean") {
		t.Errorf("expected schema-drift hint to suppress typo suggestion, got:\n%s", got)
	}
}

// TestLoadStrict_StackDeploy_RemovedKeyWarns covers the forge.yaml schema
// cleanup: the `stack.deploy:` sub-block (target/provider/registry) was an
// unconsumed duplicate of docker.registry + per-env KCL and was removed.
// An old forge.yaml carrying it must still LOAD (removed keys are non-fatal
// migration WARNINGS, not errors), so mid-migration projects aren't
// stranded — the next forge.yaml rewrite drops the dead block.
func TestLoadStrict_StackDeploy_RemovedKeyWarns(t *testing.T) {
	var sink strings.Builder
	prev := SetConfigWarningSink(&sink)
	defer SetConfigWarningSink(prev)

	in := validBaseYAML + `stack:
  deploy:
    target: k8s
    provider: k3d
    registry: ghcr.io
`
	// LoadStrict returns nil error for warning-only keys (they don't gate),
	// so a clean load is the expected outcome for a removed-but-tolerated key.
	if _, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...); err != nil {
		t.Fatalf("removed `stack.deploy` must load (warn, not fail) so mid-migration projects aren't stranded. err=%v", err)
	}
	got := sink.String()
	if !containsAll(got, `"stack.deploy" was removed in`, "docker.registry") {
		t.Errorf("expected stack.deploy→docker.registry migration warning, got:\n%s", got)
	}
}

// TestLoadStrict_RemovedSchemaKey_ServicesBlock covers the
// component-model migration hint: a top-level `services:` block (the
// pre-unification shape) resolves to the components migration message
// rather than a bare unknown-key error — and as a non-fatal WARNING, so
// a project carrying the retired block still loads (fr-57edf33aca).
func TestLoadStrict_RemovedSchemaKey_ServicesBlock(t *testing.T) {
	var sink strings.Builder
	prev := SetConfigWarningSink(&sink)
	defer SetConfigWarningSink(prev)

	// A stale top-level `services:` block (the pre-unification shape) must
	// resolve to the components migration hint, not a bare unknown-key error.
	in := validBaseYAML + "services:\n  - name: api\n    type: go_service\n    path: handlers/api\n"
	if _, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...); err != nil {
		t.Fatalf("removed top-level `services:` block must WARN, not fail; got: %v", err)
	}
	got := sink.String()
	if !containsAll(got, `"services" was removed in`, "components") {
		t.Errorf("expected services→components migration warning, got:\n%s", got)
	}
}

// TestLoadStrict_UnknownKeyClassification is the table-driven matrix for
// the unknown-key outcomes:
//
//  1. removed key  → non-fatal WARNING with migration hint, NO Levenshtein
//     suggestion, load SUCCEEDS (a key forge itself wrote
//     must not strand the project — fr-57edf33aca)
//  2. typo'd key   → fatal error, "did you mean" suggestion
//  3. distant key  → fatal error, plain unknown-key, no suggestion/hint
//
// Removed keys must win over suggestions: an agent that sees
// "did you mean 'kcl_dir'?" for k8s.provider would rename instead of
// migrating. The fatal/warn split is the load-bearing distinction —
// genuine typos (cases 2/3) stay hard errors so typo detection stays
// useful.
func TestLoadStrict_UnknownKeyClassification(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(string) string // injects the fault into validBaseYAML
		wantWarn   bool                // true: load succeeds, message lands in the warning sink
		wantSubstr []string            // all must appear in the surfaced message
		notSubstr  []string            // none may appear in the surfaced message
	}{
		{
			name: "removed key k8s.provider warns with migration hint",
			mutate: func(in string) string {
				return strings.Replace(in, "k8s:\n  kcl_dir: deploy/kcl\n",
					"k8s:\n  kcl_dir: deploy/kcl\n  provider: k3d\n", 1)
			},
			wantWarn: true,
			wantSubstr: []string{
				`"k8s.provider" was removed in`,
				"forge.K8sCluster",
				"migrations/environments-to-kcl",
			},
			notSubstr: []string{"did you mean"},
		},
		{
			name: "removed top-level key services warns with components migration hint",
			mutate: func(in string) string {
				// A stale top-level services: block (the pre-unification
				// shape) must point at the components migration.
				return in + "services:\n  - name: api\n    type: go_service\n    path: handlers/api\n"
			},
			wantWarn: true,
			wantSubstr: []string{
				`"services" was removed in`,
				"components",
			},
			notSubstr: []string{"did you mean"},
		},
		{
			name: "removed top-level key binaries warns with components migration hint",
			mutate: func(in string) string {
				return in + "binaries:\n  - name: proxy\n    path: cmd/proxy.go\n"
			},
			wantWarn: true,
			wantSubstr: []string{
				`"binaries" was removed in`,
				"kind: binary",
			},
			notSubstr: []string{"did you mean"},
		},
		{
			name: "typo'd key gets a fatal did-you-mean suggestion",
			mutate: func(in string) string {
				return strings.Replace(in, "auth:", "auht:", 1)
			},
			wantWarn:   false,
			wantSubstr: []string{"unknown key", "auht", "did you mean", "auth"},
			notSubstr:  []string{"was removed in"},
		},
		{
			name: "distant key gets a fatal plain unknown-key error",
			mutate: func(in string) string {
				return in + "completely_unrelated_key: 42\n"
			},
			wantWarn:   false,
			wantSubstr: []string{"unknown key", "completely_unrelated_key"},
			notSubstr:  []string{"did you mean", "was removed in"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sink strings.Builder
			prev := SetConfigWarningSink(&sink)
			defer SetConfigWarningSink(prev)

			_, err := LoadStrict([]byte(tc.mutate(validBaseYAML)), "forge.yaml", baseComponents()...)
			var got string
			if tc.wantWarn {
				if err != nil {
					t.Fatalf("removed key must WARN, not fail the load; got: %v", err)
				}
				got = sink.String()
			} else {
				ve := requireValidationError(t, err)
				got = ve.Error()
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(got, want) {
					t.Errorf("expected %q in surfaced message, got:\n%s", want, got)
				}
			}
			for _, not := range tc.notSubstr {
				if strings.Contains(got, not) {
					t.Errorf("did not expect %q in surfaced message, got:\n%s", not, got)
				}
			}
		})
	}
}

// TestLoadStrict_DeprecatedEnvironmentsStillLoads pins the whitelist
// behaviour: the removed top-level `environments` block does NOT gate the
// load (mid-migration projects must keep loading), is NOT reported as an
// unknown or removed key — but IS surfaced as a non-fatal WARNING so the
// user migrates it before the next forge.yaml rewrite drops it.
func TestLoadStrict_DeprecatedEnvironmentsStillLoads(t *testing.T) {
	var sink strings.Builder
	prev := SetConfigWarningSink(&sink)
	defer SetConfigWarningSink(prev)

	in := validBaseYAML + "environments:\n  - name: dev\n    type: local\n"
	if _, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...); err != nil {
		t.Fatalf("expected deprecated 'environments' block to load cleanly, got: %v", err)
	}

	out := sink.String()
	if !strings.Contains(out, "environments") {
		t.Errorf("expected warning to name the deprecated 'environments' key, got: %q", out)
	}
	if !strings.Contains(out, "deprecated top-level key") {
		t.Errorf("expected warning to flag a deprecated top-level key, got: %q", out)
	}
	if !strings.Contains(out, "migrations/environments-to-kcl") {
		t.Errorf("expected warning to point at the environments-to-kcl migration skill, got: %q", out)
	}
}

// TestLoadStrict_RetiredNestedKey_FeaturesExperimentalDeploy pins the
// headline fr-57edf33aca case: `features.experimental.deploy: true` —
// a key forge ITSELF wrote in the experimental window — must NOT
// hard-fail every config-loading command after the schema graduated the
// flag. It loads with a non-fatal warning carrying the migration hint.
func TestLoadStrict_RetiredNestedKey_FeaturesExperimentalDeploy(t *testing.T) {
	var sink strings.Builder
	prev := SetConfigWarningSink(&sink)
	defer SetConfigWarningSink(prev)

	in := validBaseYAML + "features:\n  experimental:\n    deploy: true\n"
	if _, err := LoadStrict([]byte(in), "forge.yaml", baseComponents()...); err != nil {
		t.Fatalf("forge's own retired key features.experimental.deploy must WARN, not fail; got: %v", err)
	}
	got := sink.String()
	if !containsAll(got, `"features.experimental.deploy" was removed in`, "features.deploy") {
		t.Errorf("expected migration warning pointing at features.deploy, got:\n%s", got)
	}
	if strings.Contains(got, "did you mean") {
		t.Errorf("retired key must not emit a typo suggestion, got:\n%s", got)
	}
}

// TestLoadStrict_NoDeprecatedKeyNoWarning guards against false-positive
// warnings: a clean config must produce no warning output.
func TestLoadStrict_NoDeprecatedKeyNoWarning(t *testing.T) {
	var sink strings.Builder
	prev := SetConfigWarningSink(&sink)
	defer SetConfigWarningSink(prev)

	if _, err := LoadStrict([]byte(validBaseYAML), "forge.yaml", baseComponents()...); err != nil {
		t.Fatalf("expected clean config to load, got: %v", err)
	}
	if out := sink.String(); out != "" {
		t.Errorf("expected no warnings on a clean config, got: %q", out)
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
