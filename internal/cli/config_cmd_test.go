// File: internal/cli/config_cmd_test.go
//
// Tests for `forge config set`. After the KCL-canonical refactor
// dropped `environments[].config` from forge.yaml, the writer targets
// sibling `config.<env>.yaml` files. These tests pin the new behaviour
// + guard against regressing to the old (silently-broken) shape.
//
// They exercise:
//
//   - writing to a fresh config.<env>.yaml (file doesn't exist yet)
//   - preserving other keys when updating an existing sibling file
//   - preserving the user's header comment block on update
//   - secret references write through verbatim (no quoting drift)
//   - type validation (against proto/config/v1/config.proto) fires
//     before the writer touches disk
//   - forge.yaml is NOT modified (regression guard)
//   - missing forge.yaml in cwd produces the standard error
//   - --unset on a missing file / missing key is a quiet no-op
//   - --unset removes the key while keeping the file (so later sets
//     stay deterministic)

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalConfigForgeYAML is a forge.yaml just rich enough for
// findProjectConfigFile to anchor the project directory. The contents
// don't matter for `forge config set` — it never reads or writes this
// file — but we want a deterministic baseline to assert NO modification.
const minimalConfigForgeYAML = `# project manifest — hand-edited
name: testproj
module_path: example.com/testproj
version: 0.1.0
kind: service
`

func TestRunConfigSet_FreshFileGetsHeader(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)

	if err := runConfigSet("dev", "log_level", "debug", false); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "config.dev.yaml"))
	if err != nil {
		t.Fatalf("read config.dev.yaml: %v", err)
	}
	body := string(got)

	if !strings.Contains(body, "# config.dev.yaml") {
		t.Errorf("fresh file should carry the canonical header comment, got:\n%s", body)
	}
	if !strings.Contains(body, "log_level: debug") {
		t.Errorf("fresh file should contain the written entry, got:\n%s", body)
	}

	// forge.yaml must be untouched.
	yamlBytes, err := os.ReadFile(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("read forge.yaml: %v", err)
	}
	if string(yamlBytes) != minimalConfigForgeYAML {
		t.Errorf("forge.yaml was modified — `forge config set` must never touch it\n got:\n%s\nwant:\n%s", yamlBytes, minimalConfigForgeYAML)
	}
}

func TestRunConfigSet_PreservesExistingKeys(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	seed := `# user header
log_level: warn
port: 8080
`
	if err := os.WriteFile(filepath.Join(dir, "config.prod.yaml"), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed config.prod.yaml: %v", err)
	}

	if err := runConfigSet("prod", "database_url", "${prod-db#dsn}", false); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "config.prod.yaml"))
	if err != nil {
		t.Fatalf("read config.prod.yaml: %v", err)
	}
	s := string(body)

	for _, want := range []string{"# user header", "log_level: warn", "port: 8080", "database_url:", "${prod-db#dsn}"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	// The canonical header should NOT have been stamped over the user's.
	if strings.Contains(s, "# config.prod.yaml — per-environment runtime config") {
		t.Errorf("canonical header was stamped on top of an existing file; should preserve user header:\n%s", s)
	}
}

func TestRunConfigSet_UpdatesExistingKeyInPlace(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	seed := "log_level: warn\nport: 8080\n"
	if err := os.WriteFile(filepath.Join(dir, "config.dev.yaml"), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := runConfigSet("dev", "log_level", "debug", false); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "config.dev.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "log_level: warn") {
		t.Errorf("old value `warn` should have been replaced:\n%s", s)
	}
	if !strings.Contains(s, "log_level: debug") {
		t.Errorf("new value `debug` missing:\n%s", s)
	}
	if !strings.Contains(s, "port: 8080") {
		t.Errorf("sibling port key was dropped:\n%s", s)
	}
}

func TestRunConfigSet_IntCoercionWritesUnquoted(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	if err := os.MkdirAll(filepath.Join(dir, "proto", "config", "v1"), 0o755); err != nil {
		t.Fatalf("mkdir proto dir: %v", err)
	}
	protoSrc := `syntax = "proto3";
package config.v1;
message AppConfig {
  int32 port = 1;
  string log_level = 2;
  bool auto_migrate = 3;
}
`
	if err := os.WriteFile(filepath.Join(dir, "proto", "config", "v1", "config.proto"), []byte(protoSrc), 0o644); err != nil {
		t.Fatalf("write proto: %v", err)
	}

	if err := runConfigSet("dev", "port", "9090", false); err != nil {
		t.Fatalf("runConfigSet port: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "config.dev.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(body)
	// int32 → !!int → unquoted scalar.
	if !strings.Contains(s, "port: 9090") {
		t.Errorf("port should emit as unquoted int, got:\n%s", s)
	}
	if strings.Contains(s, `port: "9090"`) {
		t.Errorf("port should NOT be quoted (proto says int32):\n%s", s)
	}
}

func TestRunConfigSet_RejectsBadIntValue(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	if err := os.MkdirAll(filepath.Join(dir, "proto", "config", "v1"), 0o755); err != nil {
		t.Fatalf("mkdir proto dir: %v", err)
	}
	protoSrc := `syntax = "proto3";
message AppConfig {
  int32 port = 1;
}
`
	if err := os.WriteFile(filepath.Join(dir, "proto", "config", "v1", "config.proto"), []byte(protoSrc), 0o644); err != nil {
		t.Fatalf("write proto: %v", err)
	}

	err := runConfigSet("dev", "port", "not-a-number", false)
	if err == nil {
		t.Fatal("expected type-validation error for non-int port value")
	}
	if !strings.Contains(err.Error(), "not a valid integer") {
		t.Errorf("error should mention integer parsing, got: %v", err)
	}
	// Failure must happen *before* any file write.
	if _, statErr := os.Stat(filepath.Join(dir, "config.dev.yaml")); !os.IsNotExist(statErr) {
		t.Errorf("config.dev.yaml should not have been created on validation failure (stat err=%v)", statErr)
	}
}

func TestRunConfigSet_RejectsBadKey(t *testing.T) {
	withTempProject(t, minimalConfigForgeYAML)
	err := runConfigSet("dev", "Bad-Key", "x", false)
	if err == nil {
		t.Fatal("expected invalid-key error")
	}
	if !strings.Contains(err.Error(), "invalid config key") {
		t.Errorf("error should call out the bad key, got: %v", err)
	}
}

func TestRunConfigSet_NoForgeYAMLReturnsStandardErr(t *testing.T) {
	t.Chdir(t.TempDir())
	err := runConfigSet("dev", "log_level", "debug", false)
	if err == nil {
		t.Fatal("expected ErrProjectConfigNotFound when forge.yaml is absent")
	}
	if !errors.Is(err, ErrProjectConfigNotFound) {
		t.Errorf("expected ErrProjectConfigNotFound, got: %v", err)
	}
}

func TestRunConfigSet_UnsetMissingFileIsNoOp(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	if err := runConfigSet("dev", "log_level", "", true); err != nil {
		t.Fatalf("runConfigSet --unset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.dev.yaml")); !os.IsNotExist(err) {
		t.Errorf("unset against missing file must not create the file (stat err=%v)", err)
	}
}

func TestRunConfigSet_UnsetRemovesKeyKeepsOthers(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	seed := "log_level: warn\nport: 8080\n"
	if err := os.WriteFile(filepath.Join(dir, "config.dev.yaml"), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := runConfigSet("dev", "log_level", "", true); err != nil {
		t.Fatalf("unset: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "config.dev.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "log_level:") {
		t.Errorf("log_level should be gone:\n%s", s)
	}
	if !strings.Contains(s, "port: 8080") {
		t.Errorf("port should remain:\n%s", s)
	}
}

func TestRunConfigSet_UnsetMissingKeyReportsAndKeepsFile(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	seed := "log_level: warn\n"
	if err := os.WriteFile(filepath.Join(dir, "config.dev.yaml"), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := runConfigSet("dev", "port", "", true); err != nil {
		t.Fatalf("unset missing key: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "config.dev.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "log_level: warn") {
		t.Errorf("unset of missing key should leave existing keys intact, got:\n%s", body)
	}
}

func TestRunConfigSet_NeverModifiesForgeYAML(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)

	// Run a mix of operations: set, update, set new key, unset.
	if err := runConfigSet("dev", "log_level", "debug", false); err != nil {
		t.Fatalf("set log_level: %v", err)
	}
	if err := runConfigSet("dev", "port", "8080", false); err != nil {
		t.Fatalf("set port: %v", err)
	}
	if err := runConfigSet("dev", "log_level", "info", false); err != nil {
		t.Fatalf("update log_level: %v", err)
	}
	if err := runConfigSet("dev", "log_level", "", true); err != nil {
		t.Fatalf("unset log_level: %v", err)
	}

	yamlBytes, err := os.ReadFile(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("read forge.yaml: %v", err)
	}
	if string(yamlBytes) != minimalConfigForgeYAML {
		t.Errorf("forge.yaml was modified by config set\n got:\n%s\nwant:\n%s", yamlBytes, minimalConfigForgeYAML)
	}

	// Cross-check: forge.yaml has no `environments:` key (regression
	// guard against writing back the dead inline shape).
	if strings.Contains(string(yamlBytes), "environments:") {
		t.Errorf("regression: forge.yaml grew an `environments:` block — `forge config set` reverted to the old shape\n%s", yamlBytes)
	}
}

func TestRunConfigSet_SecretRefPassesThrough(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	if err := runConfigSet("prod", "database_url", "${prod-db#dsn}", false); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "config.prod.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "${prod-db#dsn}") {
		t.Errorf("secret ref must round-trip verbatim, got:\n%s", body)
	}
}

func TestRunConfigSet_PerEnvFilesAreIndependent(t *testing.T) {
	dir := withTempProject(t, minimalConfigForgeYAML)
	if err := runConfigSet("dev", "log_level", "debug", false); err != nil {
		t.Fatalf("set dev: %v", err)
	}
	if err := runConfigSet("prod", "log_level", "warn", false); err != nil {
		t.Fatalf("set prod: %v", err)
	}
	devBody, err := os.ReadFile(filepath.Join(dir, "config.dev.yaml"))
	if err != nil {
		t.Fatalf("read dev: %v", err)
	}
	prodBody, err := os.ReadFile(filepath.Join(dir, "config.prod.yaml"))
	if err != nil {
		t.Fatalf("read prod: %v", err)
	}
	if !strings.Contains(string(devBody), "log_level: debug") {
		t.Errorf("dev should have debug:\n%s", devBody)
	}
	if !strings.Contains(string(prodBody), "log_level: warn") {
		t.Errorf("prod should have warn:\n%s", prodBody)
	}
	if strings.Contains(string(devBody), "warn") {
		t.Errorf("dev file shouldn't carry prod's value:\n%s", devBody)
	}
}
