package generator

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The frontend config system — the typed zod module (src/lib/config_gen.ts),
// the KCL schema (deploy/kcl/frontend_config_gen.k) and the per-env
// config.js assigning window.__FORGE_CONFIG__ — is keyed entirely on a
// message-level (forge.v1.frontend_config) annotation. All three generators
// are correct: given an annotated message they emit all three artifacts.
//
// Given NO annotated message they correctly emit NOTHING, and that is the
// state every scaffolded project shipped in: `forge project new X --frontend
// web` wrote a config proto declaring only AppConfig, so a fresh project got
// none of the system and its frontend kept reading build-time
// process.env.NEXT_PUBLIC_* — the exact shape the config system exists to
// replace, and one that cannot be promoted between environments because a
// bundler inlines it.
//
// Every prior verification of the config system passed because it HAND-WROTE
// the annotation. Nobody checked the scaffold default, so the feature was
// complete and unreachable at the same time. These tests check the scaffold
// default: they assert the annotation is DECLARED by scaffolding, which is
// what makes the rest of the system run without a hand edit.

// frontendConfigMessageRE finds the frontend-bound config message and
// captures the frontend name the annotation binds it to. Matching the
// ANNOTATION rather than a message name is deliberate: the binding is by
// annotation everywhere else in forge, so renaming the message must not
// break this test and must not silently re-point the config.
var frontendConfigMessageRE = regexp.MustCompile(
	`option\s*\(forge\.v1\.frontend_config\)\s*=\s*\{\s*frontend:\s*"([^"]+)"\s*\}`)

// TestScaffold_FrontendGetsAnnotatedConfigMessage is the gate. A project
// scaffolded WITH a frontend must declare that frontend's config message,
// carrying the annotation that activates the three projections.
func TestScaffold_FrontendGetsAnnotatedConfigMessage(t *testing.T) {
	root := scaffoldWithFrontend(t, "cfgapp", "web")

	proto, path := readFrontendConfigProto(t, root, "web")

	m := frontendConfigMessageRE.FindStringSubmatch(proto)
	if m == nil {
		t.Fatalf("%s declares no (forge.v1.frontend_config) annotation — the scaffolded frontend "+
			"gets NO typed config module, NO KCL schema and NO runtime config.js, and its "+
			"templates keep reading build-time process.env.NEXT_PUBLIC_*", path)
	}
	if m[1] != "web" {
		t.Errorf("frontend config is bound to frontend %q, but the scaffolded frontend is %q — "+
			"generate refuses a config naming a frontend forge.yaml does not declare", m[1], "web")
	}
}

// TestScaffold_FrontendConfigDeclaresTheFieldsItsTemplatesRead is the
// substance behind the annotation. An annotated but empty message would
// satisfy the test above while leaving every read site undefined.
//
// The expected set is derived from what the scaffolded frontend templates
// actually read, not from a list someone remembered: each entry below is a
// NEXT_PUBLIC_* variable that appears in the shipped template tree and has a
// runtime meaning.
func TestScaffold_FrontendConfigDeclaresTheFieldsItsTemplatesRead(t *testing.T) {
	root := scaffoldWithFrontend(t, "fieldapp", "web")
	proto, path := readFrontendConfigProto(t, root, "web")

	// The env_var spelling is the ONE name shared by the proto, the KCL
	// projection, the runtime document and the generated TS module — so
	// asserting on it checks the whole chain agrees, not just that a
	// proto field exists.
	want := []string{
		"API_URL",
		"MOCK_API",
		"OTEL_ENDPOINT",
		"APP_VERSION",
		"OIDC_ISSUER",
		"OIDC_CLIENT_ID",
		"OIDC_REDIRECT_URI",
		"OIDC_SCOPES",
	}
	for _, env := range want {
		if !strings.Contains(proto, `env_var: "`+env+`"`) {
			t.Errorf("%s declares no field with env_var %q — the scaffolded frontend reads it, "+
				"so the generated config module will not carry it", path, env)
		}
	}
}

// TestScaffold_FrontendConfigOmitsBasePath pins a deliberate EXCLUSION, and
// pins it here because the omission looks like an oversight to anyone
// auditing the list above.
//
// NEXT_PUBLIC_BASE_PATH is genuinely build-time. next.config.ts reads it to
// place every emitted asset, so the prefix is baked into file paths a build
// has already written. A runtime document delivered to the browser cannot
// move files on disk — declaring it as runtime config would produce a value
// the app reads and cannot honor, which is worse than not having it.
func TestScaffold_FrontendConfigOmitsBasePath(t *testing.T) {
	root := scaffoldWithFrontend(t, "bpapp", "web")
	proto, path := readFrontendConfigProto(t, root, "web")

	if strings.Contains(proto, `env_var: "BASE_PATH"`) {
		t.Errorf("%s declares BASE_PATH as runtime config, but next.config.ts consumes it at BUILD "+
			"time to place emitted assets — a runtime document cannot relocate files a build "+
			"already wrote", path)
	}
}

// TestScaffold_FrontendReadsTypedModuleNotProcessEnv is the user-visible
// half. Declaring the config is pointless if the scaffolded frontend still
// reads process.env: the annotation would generate artifacts nothing loads.
//
// oidc-provider.ts is the file that matters most — auth is the one flow
// where a build-time-inlined value produces the confusing failure (sign-in
// succeeds against one issuer, every RPC 401s against another).
func TestScaffold_FrontendReadsTypedModuleNotProcessEnv(t *testing.T) {
	root := scaffoldWithFrontend(t, "typedapp", "web")

	rel := filepath.Join("frontends", "web", "src", "lib", "auth", "oidc-provider.ts")
	body := readFile(t, filepath.Join(root, rel))

	if !strings.Contains(body, `from "@/lib/config_gen"`) {
		t.Errorf("%s does not import the generated typed config module — it was scaffolded in its "+
			"build-time env-var form, so the config system's values never reach auth", rel)
	}
	if n := strings.Count(body, "process.env.NEXT_PUBLIC_OIDC"); n > 0 {
		t.Errorf("%s still reads process.env.NEXT_PUBLIC_OIDC* in %d place(s) — those are inlined "+
			"at build time, so the bundle cannot be promoted between environments", rel, n)
	}
}

// scaffoldWithFrontend generates a project that has a frontend, which is the
// configuration under test: the gap only exists on the frontend path.
func scaffoldWithFrontend(t *testing.T, name, frontend string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	g := NewProjectGenerator(name, root, "example.com/"+name)
	g.FrontendName = frontend
	if err := g.Generate(); err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	return root
}

// readFrontendConfigProto returns the proto file declaring the frontend's
// config, and its project-relative path for failure messages.
//
// It looks for the per-frontend file FIRST and falls back to the shared
// config.proto, so this test pins the ANNOTATION being scaffolded rather
// than the file layout carrying it — a later layout change stays free.
func readFrontendConfigProto(t *testing.T, root, frontend string) (body, rel string) {
	t.Helper()
	candidates := []string{
		filepath.Join("proto", "config", "v1", frontend+"_config.proto"),
		filepath.Join("proto", "config", "v1", "config.proto"),
	}
	for _, c := range candidates {
		data, err := os.ReadFile(filepath.Join(root, c))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "forge.v1.frontend_config") {
			return string(data), c
		}
		body, rel = string(data), c
	}
	if body == "" {
		t.Fatalf("no config proto found under proto/config/v1 (looked for %v)", candidates)
	}
	return body, rel
}
