package codegen

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// perBinaryKCL renders the per-binary config module for the two-binary
// fixture: a shared BaseConfig composed by admin (which owns a SENSITIVE
// key) and gateway (which owns a plain int).
func perBinaryKCL(t *testing.T) string {
	t.Helper()
	out, err := GenerateConfigKCLPerBinary(nil, BinaryConfigFieldsFrom(perBinaryMessages()), "myproj")
	if err != nil {
		t.Fatalf("GenerateConfigKCLPerBinary: %v", err)
	}
	return out
}

// One schema + one projection lambda PER BINARY, each carrying only that
// binary's fields.
func TestPerBinaryKCL_EmitsOneSchemaAndLambdaPerBinary(t *testing.T) {
	got := perBinaryKCL(t)

	for _, want := range []string{
		"schema ConfigSecretRef:", // declared once for the module
		"schema AdminConfig:",
		"schema GatewayConfig:",
		"adminConfigEnvMap = lambda c: AdminConfig -> {str: forge.EnvSource} {",
		"gatewayConfigEnvMap = lambda c: GatewayConfig -> {str: forge.EnvSource} {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("per-binary config module missing %q\n%s", want, got)
		}
	}

	// ConfigSecretRef is declared exactly once — a second declaration in the
	// same KCL module would be a redefinition.
	if n := strings.Count(got, "schema ConfigSecretRef:"); n != 1 {
		t.Errorf("ConfigSecretRef declared %d times, want exactly 1", n)
	}
}

// The security property, at the level of the emitted projection: admin's
// secret appears ONLY in admin's lambda, so only admin's workload can carry
// it into a manifest.
func TestPerBinaryKCL_SecretIsScopedToItsBinary(t *testing.T) {
	got := perBinaryKCL(t)

	adminBlock := got[strings.Index(got, "adminConfigEnvMap"):]
	if end := strings.Index(adminBlock, "schema GatewayConfig"); end > 0 {
		adminBlock = adminBlock[:end]
	}
	gatewayBlock := got[strings.Index(got, "gatewayConfigEnvMap"):]

	if !strings.Contains(adminBlock, `"ADMIN_API_KEY" = {from_secret =`) {
		t.Error("admin's projection should carry ADMIN_API_KEY as a secret reference")
	}
	if strings.Contains(gatewayBlock, "ADMIN_API_KEY") {
		t.Error("gateway's projection must NOT mention admin's secret")
	}
	if strings.Contains(adminBlock, "UPSTREAM_TIMEOUT_MS") {
		t.Error("admin's projection must NOT mention gateway's field")
	}

	// The shared base is present in BOTH — a shared DEFINITION, resolved
	// per-process.
	for _, block := range []string{adminBlock, gatewayBlock} {
		if !strings.Contains(block, `"LOG_LEVEL"`) {
			t.Error("both binaries should project the shared LOG_LEVEL")
		}
	}
}

// The hermetic end-to-end proof: evaluate the generated module with the real
// kcl binary and assert on the projected env maps. This is what a workload's
// Deployment env is built from, so it is the honest statement of "each
// binary carries only its own env vars".
func TestPerBinaryKCL_EndToEndDisjointEnvMaps(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping per-binary env-map e2e proof")
	}

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("kcl.mod", "[package]\nname = \"perbin_proof\"\n")
	write(ConfigSchemaModule+".k", perBinaryKCL(t))
	// A minimal local `forge` module supplying just EnvSource. The real
	// forge module is deliberately NOT used here: this test is about the
	// CONFIG projection, and the deploy vocabulary around it is being
	// reshaped separately.
	write("forge/kcl.mod", "[package]\nname = \"forge\"\n")
	write("forge/core.k", "schema EnvSource:\n    value?: str\n    from_secret?: SecretKeySel\n\nschema SecretKeySel:\n    name: str\n    key: str\n")

	write("main.k", `import `+ConfigSchemaModule+` as config_gen

_admin = config_gen.AdminConfig {log_level = "debug"}
_gateway = config_gen.GatewayConfig {upstream_timeout_ms = 1500}

_admin_env = config_gen.adminConfigEnvMap(_admin)
_gateway_env = config_gen.gatewayConfigEnvMap(_gateway)

# Each binary carries its OWN leaf ...
assert_admin_has_own = "ADMIN_API_KEY" in _admin_env
assert_gateway_has_own = "UPSTREAM_TIMEOUT_MS" in _gateway_env
# ... and NOT its sibling's. This is the disjointness claim: admin's
# credential is ABSENT from gateway's env, not merely unread.
assert_gateway_lacks_admin_secret = "ADMIN_API_KEY" not in _gateway_env
assert_admin_lacks_gateway_field = "UPSTREAM_TIMEOUT_MS" not in _admin_env

# The sensitive field routes through the secret channel, never inline.
# Compared field-by-field: a typed schema instance does not compare equal to
# a bare dict literal in KCL, so == against {name = ..., key = ...} would be
# false even when the value is right.
assert_secret_name = _admin_env["ADMIN_API_KEY"].from_secret.name == "myproj-secrets"
assert_secret_key = _admin_env["ADMIN_API_KEY"].from_secret.key == "admin_api_key"
assert_secret_not_inline = _admin_env["ADMIN_API_KEY"].value == Undefined

# The SHARED base is present in both, resolved independently per process:
# admin pinned debug, gateway inherited the default.
assert_shared_admin = _admin_env["LOG_LEVEL"].value == "debug"
assert_shared_gateway = _gateway_env["LOG_LEVEL"].value == "info"
`)

	cmd := exec.CommandContext(t.Context(), "kcl", "run", ".", "--format", "json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kcl run failed: %v\n%s", err, out)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal kcl json: %v\n%s", err, out)
	}
	var sawAssert bool
	for k, v := range parsed {
		if !strings.HasPrefix(k, "assert_") {
			continue
		}
		sawAssert = true
		b, ok := v.(bool)
		if !ok {
			t.Errorf("identifier %q not a bool: %v", k, v)
			continue
		}
		if !b {
			t.Errorf("assertion %q is false", k)
		}
	}
	if !sawAssert {
		t.Fatalf("no assert_* identifiers found in kcl output:\n%s", out)
	}
}

// Authoring a field on the WRONG binary's config must be a KCL type error,
// not a silently ignored line — the schema split is enforced by the type
// system, not by convention.
func TestPerBinaryKCL_CrossBinaryFieldIsATypeError(t *testing.T) {
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH; skipping cross-binary type-error proof")
	}

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("kcl.mod", "[package]\nname = \"perbin_typeerr\"\n")
	write(ConfigSchemaModule+".k", perBinaryKCL(t))
	write("forge/kcl.mod", "[package]\nname = \"forge\"\n")
	write("forge/core.k", "schema EnvSource:\n    value?: str\n    from_secret?: SecretKeySel\n\nschema SecretKeySel:\n    name: str\n    key: str\n")
	write("main.k", "import "+ConfigSchemaModule+" as config_gen\n\nbad = config_gen.GatewayConfig {admin_api_key = \"leak\"}\n")

	cmd := exec.CommandContext(t.Context(), "kcl", "run", ".", "--format", "json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a KCL type error authoring admin's field on GatewayConfig, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "admin_api_key") {
		t.Errorf("error should name the offending field:\n%s", out)
	}
}
