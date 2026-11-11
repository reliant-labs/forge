package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func nativeConfigFixture() FrontendConfig {
	return FrontendConfig{Frontend: "mobile", MessageName: "MobileConfig", Fields: []ConfigField{
		{Name: "api_url", EnvVar: "API_URL", ProtoType: "string", DefaultValue: "http://localhost:8080"},
		{Name: "mock", EnvVar: "MOCK_API", ProtoType: "string", DefaultValue: "off"},
		{Name: "otel_endpoint", EnvVar: "OTEL_ENDPOINT", ProtoType: "string"},
		{Name: "environment", EnvVar: "ENVIRONMENT", ProtoType: "string", DefaultValue: "dev"},
		{Name: "enabled", EnvVar: "FEATURE_ENABLED", ProtoType: "bool", DefaultValue: "false"},
		{Name: "interval", EnvVar: "INTERVAL", ProtoType: "int32", DefaultValue: "5000"},
		{Name: "issuer", EnvVar: "OIDC_ISSUER", ProtoType: "string", Required: true},
		{Name: "fallback", ProtoType: "string"},
		{Name: "nested", MessageType: "Nested"},
	}}
}

func TestFrontendConfigNativeStaticMapping(t *testing.T) {
	fc := nativeConfigFixture()
	out, err := GenerateFrontendConfigTS(fc, "demo", "react-native")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"API_URL", "MOCK_API", "OTEL_ENDPOINT", "ENVIRONMENT", "FEATURE_ENABLED", "INTERVAL", "OIDC_ISSUER", "fallback"} {
		if !strings.Contains(out, key+": process.env.EXPO_PUBLIC_"+key+",") {
			t.Errorf("missing static mapping for %s", key)
		}
	}
	for _, forbidden := range []string{"process.env[", "window", "Window", "config.js", "nothing is rebuilt", "EXPO_PUBLIC_nested", "EXPO_PUBLIC_mock,"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("native output contains %q", forbidden)
		}
	}
	for _, want := range []string{"globalThis as typeof globalThis", "if (override) return override;", "requires a new bundle", "EXPO_PUBLIC_<key>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Only acquisition and delivery instructions change, not field validation.
	browser, err := GenerateFrontendConfigTS(fc, "demo", "nextjs")
	if err != nil {
		t.Fatal(err)
	}
	schema := func(s string) string {
		start := strings.Index(s, "export const configSchema")
		return s[start : start+strings.Index(s[start:], "export type")]
	}
	if schema(out) != schema(browser) {
		t.Fatal("native schema differs from browser schema")
	}
	if !strings.Contains(browser, configLoaderBody(fc, "demo")) {
		t.Fatal("legacy loader helper changed")
	}
	conditional := cloudFrontendConfig()
	native, err := GenerateFrontendConfigTS(conditional, "demo", "react-native")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(native, requiredWhenTS(conditional)) || !strings.Contains(native, "const missing = missingRequiredConfig(raw);") {
		t.Fatal("native must enforce shared conditional requiredness on raw values")
	}
}

func TestFrontendConfigNativeRejectsUnsafeEnvKeys(t *testing.T) {
	for _, key := range []string{"BAD-KEY", "X;throw new Error()", "X\nY", "X\"Y", "1KEY"} {
		t.Run(key, func(t *testing.T) {
			fc := FrontendConfig{Fields: []ConfigField{{Name: "safe", EnvVar: key, ProtoType: "string"}}}
			if out, err := GenerateFrontendConfigTS(fc, "demo", "react-native"); err == nil || out != "" {
				t.Fatal("unsafe native key accepted")
			}
			// Browser documents may use non-identifier keys, which must be quoted.
			out, err := GenerateFrontendConfigTS(fc, "demo", "vite")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tsConfigKey(fc.Fields[0])+": z.string()") {
				t.Fatal("browser schema key not escaped")
			}
		})
	}
	if got := tsConfigKey(ConfigField{Name: "__proto__"}); got != `["__proto__"]` {
		t.Fatalf("prototype key = %s", got)
	}
}

// Execute the rendered acquisition function without installing Expo or npm
// dependencies. Node's type stripping keeps the generated TypeScript intact.
func TestFrontendConfigNativeAcquisitionExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("executes Node TypeScript; runs in task test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	version, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Fatalf("node version: %v", err)
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(string(version)), "v"), ".")
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	if major < 22 || (major == 22 && minor < 6) {
		t.Skipf("Node TypeScript stripping requires Node >=22.6; available %s", strings.TrimSpace(string(version)))
	}
	out, err := GenerateFrontendConfigTS(nativeConfigFixture(), "demo", "react-native")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(out, "function rawConfig()")
	end := start + strings.Index(out[start:], "\n}\n") + len("\n}\n")
	source := out[start:end] + `
import assert from "node:assert/strict";
process.env.EXPO_PUBLIC_API_URL = "https://api.production.example";
process.env.EXPO_PUBLIC_MOCK_API = "hybrid";
process.env.EXPO_PUBLIC_ENVIRONMENT = "production";
process.env.EXPO_PUBLIC_OTEL_ENDPOINT = "https://otel.production.example";
process.env.EXPO_PUBLIC_FEATURE_ENABLED = "false";
process.env.EXPO_PUBLIC_INTERVAL = "1200";
process.env.EXPO_PUBLIC_OIDC_ISSUER = "https://issuer.production.example";
delete process.env.EXPO_PUBLIC_fallback;
const raw = rawConfig();
assert.equal(raw.API_URL, "https://api.production.example");
assert.equal(raw.MOCK_API, "hybrid");
assert.equal(raw.ENVIRONMENT, "production");
assert.equal(raw.OTEL_ENDPOINT, "https://otel.production.example");
assert.equal(raw.FEATURE_ENABLED, "false");
assert.equal(raw.INTERVAL, "1200");
assert.equal(raw.OIDC_ISSUER, "https://issuer.production.example");
assert.equal(raw.fallback, undefined);
const override = { API_URL: "https://override.example" };
globalThis.__FORGE_CONFIG__ = override;
assert.equal(rawConfig(), override);
delete globalThis.__FORGE_CONFIG__;
assert.equal(rawConfig().API_URL, raw.API_URL);
`
	dir := t.TempDir()
	path := filepath.Join(dir, "native.mts")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, "--experimental-strip-types", path).CombinedOutput(); err != nil {
		t.Fatalf("native acquisition: %v\n%s", err, output)
	}
}
