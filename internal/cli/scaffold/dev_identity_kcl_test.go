package scaffold

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
	"github.com/reliant-labs/forge/internal/templates"
)

func renderFrontendEnv(t *testing.T, tmpl, env string) string {
	t.Helper()
	data := templates.EnvTemplateData{ProjectName: "acme", EnvName: env, IngressEnabled: true, PrimaryWorkload: "acme", HasFrontend: true, FrontendName: "web"}
	out, err := templates.DeployTemplates().Render(tmpl, data)
	if err != nil {
		t.Fatalf("render %s: %v", tmpl, err)
	}
	return string(out)
}

// The snippets spliced into a project's env files are copies of the
// HasFrontend sections of the env templates. This pins them byte for byte, so a
// template edit that is not mirrored fails here instead of drifting.
func TestIdentitySnippetsMatchTemplates(t *testing.T) {
	dev := renderFrontendEnv(t, "kcl/dev/main.k.tmpl", "dev")
	for name, snippet := range map[string]string{
		"idn import":       identityImportSnippet,
		"ports":            devIdentityPortsSnippet("acme"),
		"idp + mail infra": devIdentityInfraSnippet,
		"sign-in env":      devSignInEnvSnippet,
		"identity":         devIdentitySnippet,
	} {
		if !strings.Contains(dev, snippet) {
			t.Errorf("dev template no longer contains the %s snippet:\n%s", name, snippet)
		}
	}
	for _, env := range []string{"staging", "prod"} {
		hosted := renderFrontendEnv(t, "kcl/cloud/main.k.tmpl", env)
		for name, snippet := range map[string]string{"idn import": identityImportSnippet, "identity": hostedIdentitySnippet} {
			if !strings.Contains(hosted, snippet) {
				t.Errorf("%s cloud template no longer contains the %s snippet:\n%s", env, name, snippet)
			}
		}
	}
}

func TestSpliceDevIdentity_EveryAnchorIsRequired(t *testing.T) {
	in := renderNoFrontendEnv(t, "kcl/dev/main.k.tmpl", "dev")
	if classifyEnvIdentity(in) != identityFresh {
		t.Fatal("a no-frontend dev env must classify as fresh")
	}
	out, err := spliceDevIdentity(in, "acme")
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	if classifyEnvIdentity(out) != identityDeclared {
		t.Error("a spliced env must classify as declared, so a second run is a no-op")
	}
	for name, anchor := range map[string]string{
		"workloads import": "import ..workloads as wl\n",
		"pprof lambda":     "_pprof_port_of = lambda",
		"infra":            "    infra = [\n",
		"env expression":   "| w.env | _machine | _port\n",
		"secret provider":  "    secret_provider = ",
	} {
		broken := strings.Replace(in, anchor, "REMOVED ", 1)
		if broken == in {
			t.Fatalf("anchor %q is not in the template", anchor)
		}
		if _, err := spliceDevIdentity(broken, "acme"); err == nil {
			t.Errorf("a dev main.k without its %s must be refused, not guessed at", name)
		}
	}
}

func TestClassifyEnvIdentity(t *testing.T) {
	fresh := renderNoFrontendEnv(t, "kcl/dev/main.k.tmpl", "dev")
	if got := classifyEnvIdentity(fresh); got != identityFresh {
		t.Errorf("no-frontend dev = %d", got)
	}
	if got := classifyEnvIdentity(renderFrontendEnv(t, "kcl/dev/main.k.tmpl", "dev")); got != identityHasFrontend {
		t.Errorf("project new --frontend dev = %d, want has-frontend", got)
	}
	for _, env := range []string{"staging", "prod"} {
		if got := classifyEnvIdentity(renderNoFrontendEnv(t, "kcl/cloud/main.k.tmpl", env)); got != identityFresh {
			t.Errorf("no-frontend %s = %d: the binder lambda's `-> forge.Frontend {` is not a frontend declaration", env, got)
		}
		if got := classifyEnvIdentity(renderFrontendEnv(t, "kcl/cloud/main.k.tmpl", env)); got != identityHasFrontend {
			t.Errorf("project new --frontend %s = %d, want has-frontend", env, got)
		}
	}
	if got := classifyEnvIdentity(fresh + "\n    identity = idn.IDENTITY\n"); got != identityDeclared {
		t.Errorf("a declared identity = %d", got)
	}
	if got := classifyEnvIdentity(fresh + "\n# identity = idn.IDENTITY\n"); got != identityFresh {
		t.Errorf("a commented identity must not count = %d", got)
	}
}

// writeNoFrontendEnvs writes the env files of a project born without a frontend.
func writeNoFrontendEnvs(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, env := range []string{"dev", "staging", "prod"} {
		dir := filepath.Join(root, "deploy", "kcl", env)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := renderNoFrontendEnv(t, scaffoldedEnvTemplate(env), env)
		if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		files[env] = content
	}
	return files
}

func readEnv(t *testing.T, root, env string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "deploy", "kcl", env, "main.k"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReconcileFirstFrontendIdentity_EditsOnlyDevAndPrintsTheHostedEdit(t *testing.T) {
	root := t.TempDir()
	envs := writeNoFrontendEnvs(t, root)
	out, _ := captureStdout(t, func() error {
		reconcileFirstFrontendIdentity(root, "acme", true)
		return nil
	})

	got := readEnv(t, root, "dev")
	for _, want := range []string{identityImportSnippet, devIdentityInfraSnippet, devIdentitySnippet, devSignInEnvSnippet, devIdentityPortsSnippet("acme")} {
		if !strings.Contains(got, want) {
			t.Errorf("dev/main.k is missing:\n%s", want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, codegen.IdentityKCLRelPath)); err != nil {
		t.Errorf("identity.k not written: %v", err)
	}
	// A hosted identity is managed and billable: the first frontend, even a web
	// one, must not opt a deployed env into it.
	for _, env := range []string{"staging", "prod"} {
		if now := readEnv(t, root, env); now != envs[env] {
			t.Errorf("%s/main.k was edited; hosted envs are the author's call:\n%s", env, now)
		}
	}
	// ...and the output names the exact edit to add it.
	for _, want := range []string{"prod, staging", "billable", "import ..identity as idn", "identity = idn.IDENTITY", "before `secret_provider =`", "forge env deploy <env>"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}

	before := map[string]string{}
	for _, env := range []string{"dev", "staging", "prod"} {
		before[env] = readEnv(t, root, env)
	}
	reconcileFirstFrontendIdentity(root, "acme", true)
	for env, want := range before {
		if got := readEnv(t, root, env); got != want {
			t.Errorf("%s/main.k changed on the second run (not idempotent)", env)
		}
	}
}

// The hint is for hosted envs that declare none: one that already has an
// identity, or is not hosted at all, is not mentioned.
func TestHostedIdentityHint(t *testing.T) {
	root := t.TempDir()
	envs := writeNoFrontendEnvs(t, root)
	if got := hostedIdentityHint(root); !strings.Contains(got, "prod, staging") {
		t.Errorf("hint = %q, want both hosted envs", got)
	}
	declared := strings.Replace(envs["prod"], "    secret_provider = ", "    identity = my_own_identity\n    secret_provider = ", 1)
	if err := os.WriteFile(filepath.Join(root, "deploy", "kcl", "prod", "main.k"), []byte(declared), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := hostedIdentityHint(root); !strings.Contains(got, "Hosted env(s) staging declare none") {
		t.Errorf("a prod that declares an identity must drop out of the hint, got %q", got)
	}
	if got := hostedIdentityHint(t.TempDir()); got != "" {
		t.Errorf("no hosted envs: hint = %q", got)
	}
}

func TestReconcileFirstFrontendIdentity_LeavesTheUsersChoiceAlone(t *testing.T) {
	snapshot := func(root string) map[string]string {
		m := map[string]string{}
		_ = filepath.Walk(filepath.Join(root, "deploy"), func(p string, i os.FileInfo, err error) error {
			if err == nil && !i.IsDir() {
				b, _ := os.ReadFile(p)
				m[p] = string(b)
			}
			return nil
		})
		return m
	}
	for name, setup := range map[string]func(t *testing.T, root string){
		"not the first frontend": func(t *testing.T, root string) {},
		"identity removed on purpose (identity.k present)": func(t *testing.T, root string) {
			if err := generator.ScaffoldIdentityKCL(root, "acme"); err != nil {
				t.Fatal(err)
			}
		},
		"identity already declared by hand": func(t *testing.T, root string) {
			p := filepath.Join(root, "deploy", "kcl", "dev", "main.k")
			b, _ := os.ReadFile(p)
			edited := strings.Replace(string(b), "    secret_provider = ", "    identity = my_own_identity\n    secret_provider = ", 1)
			if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"restructured past the anchors": func(t *testing.T, root string) {
			p := filepath.Join(root, "deploy", "kcl", "dev", "main.k")
			b, _ := os.ReadFile(p)
			if err := os.WriteFile(p, []byte(strings.Replace(string(b), "    infra = [\n", "    infra = [  # moved\n", 1)), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeNoFrontendEnvs(t, root)
			setup(t, root)
			before := snapshot(root)
			reconcileFirstFrontendIdentity(root, "acme", name != "not the first frontend")
			after := snapshot(root)
			if len(after) != len(before) {
				t.Fatalf("files appeared or vanished: %d -> %d", len(before), len(after))
			}
			for p, want := range before {
				if after[p] != want {
					t.Errorf("%s was edited", p)
				}
			}
		})
	}
}

func TestDevIssuerConfigured(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if devIssuerConfigured(root) {
		t.Error("an empty project has no issuer")
	}
	write("secrets/dev.yaml", "# comment\nDATABASE_URL: \"\"\nJWT_ISSUER: \"\"\n")
	if devIssuerConfigured(root) {
		t.Error("the scaffolded store lists JWT_ISSUER EMPTY, which is not configured")
	}
	write("secrets/dev.yaml", "JWT_ISSUER: \"https://idp.example\"\n")
	if !devIssuerConfigured(root) {
		t.Error("a non-empty JWT_ISSUER in secrets/dev.yaml is a configured issuer")
	}
	write("secrets/dev.yaml", "JWT_JWKS_URL: https://idp.example/keys\n")
	if !devIssuerConfigured(root) {
		t.Error("a non-empty JWT_JWKS_URL in secrets/dev.yaml is a configured issuer")
	}
	write("secrets/dev.yaml", "JWT_ISSUER: \"\"\n")
	write("deploy/kcl/dev/config.k", "# jwt_issuer = \"commented\"\napp_config = {\n    jwt_issuer = \"https://idp.example\"\n}\n")
	if !devIssuerConfigured(root) {
		t.Error("a jwt_issuer set in config.k is a configured issuer")
	}
}

var (
	devFrontendBlocks = []*regexp.Regexp{
		regexp.MustCompile(`(?ms)^# The (?:\w+ )?frontend's dev-server port\..*?^_\w*frontend_origin = [^\n]*\n\n`),
		regexp.MustCompile(`(?ms)^# The \w+ frontend's dev-server port\..*?^_\w+_frontend_port = [^\n]*\n\n`),
		regexp.MustCompile(`(?ms)^# Masked session recording for this environment\..*?^_session_replay = [^\n]*\n`),
		regexp.MustCompile(`(?ms)^_\w+_frontend = forge\.Frontend \{.*?^}\n`),
		regexp.MustCompile(`(?ms)^_on_host_frontend = lambda.*?^}\n`),
		regexp.MustCompile(`(?ms)^    # The frontend's dev server .*?^    frontends = \[\n.*?^    \]\n`),
		regexp.MustCompile(`(?ms)^    frontends \+= \[\n.*?^    \]\n`),
	}
	blankRuns = regexp.MustCompile(`\n{3,}`)
)

// withoutFrontendDeclaration removes what is specific to the frontend itself
// (its port, declaration and binding), leaving the identity and infra a
// project gets for having one.
func withoutFrontendDeclaration(s, project string) string {
	s = strings.ReplaceAll(s, project, "X")
	for _, re := range devFrontendBlocks {
		s = re.ReplaceAllString(s, "")
	}
	return blankRuns.ReplaceAllString(s, "\n\n")
}

// TestFirstFrontendMatchesProjectNewWithFrontend is the end-to-end guard for
// the defect: a project created WITHOUT --frontend, then given its first
// frontend (a mobile app, whose native OIDC client is derived from the
// declared identity), must end up with the dev IdP, mail catcher and identity
// that `forge project new --frontend` produces. Compared on the generated
// files, then evaluated under KCL.
func TestFirstFrontendMatchesProjectNewWithFrontend(t *testing.T) {
	if testing.Short() {
		t.Skip("scaffolds two full projects and evaluates KCL; runs in task test")
	}
	skipNpmInstall(t)
	kclplugin.Register()

	newProject := func(frontend string) string {
		dir := t.TempDir()
		g := generator.NewProjectGenerator("acme", dir, "example.com/acme")
		g.Kind = config.ProjectKindService
		g.ApplyKindFeatureDefaults(config.ProjectKindService)
		g.ServiceName = "api"
		g.FrontendName = frontend
		if err := g.Generate(); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		// The per-env config module and instances `forge generate` projects:
		// the env files import them.
		var fields []codegen.ConfigField
		for _, m := range codegen.DefaultConfigMessages() {
			fields = append(fields, m.Fields...)
		}
		kclDir := filepath.Join(dir, "deploy", "kcl")
		if err := codegen.GenerateConfigNativeShared(fields, "acme", dir, kclDir, nil); err != nil {
			t.Fatalf("config module: %v", err)
		}
		for _, env := range []string{"dev", "staging", "prod"} {
			if _, err := codegen.GenerateConfigKScaffold(fields, "acme", kclDir, env); err != nil {
				t.Fatalf("config.k %s: %v", env, err)
			}
		}
		return dir
	}

	reference := newProject("web")
	root := newProject("")
	if _, err := os.Stat(filepath.Join(root, codegen.IdentityKCLRelPath)); err == nil {
		t.Fatal("precondition: a project born without a frontend has no identity.k")
	}

	hostedBefore := map[string]string{}
	for _, env := range []string{"staging", "prod"} {
		hostedBefore[env] = withoutFrontendDeclaration(readEnv(t, root, env), "acme")
	}

	t.Chdir(root)
	scaffoldOut, err := captureStdout(t, func() error {
		return runFrontend(context.Background(), testFactory(), frontendArgs{name: "app", kind: "mobile"})
	})
	if err != nil {
		t.Fatalf("runFrontend: %v", err)
	}
	if !strings.Contains(scaffoldOut, "identity = idn.IDENTITY") || !strings.Contains(scaffoldOut, "prod, staging") {
		t.Errorf("the scaffold must print the hosted-env edit:\n%s", scaffoldOut)
	}

	// Identity file: byte-identical.
	wantID, err := os.ReadFile(filepath.Join(reference, codegen.IdentityKCLRelPath))
	if err != nil {
		t.Fatal(err)
	}
	gotID, err := os.ReadFile(filepath.Join(root, codegen.IdentityKCLRelPath))
	if err != nil {
		t.Fatalf("identity.k not written by the first frontend: %v", err)
	}
	if string(gotID) != string(wantID) {
		t.Errorf("identity.k differs from project new --frontend:\n%s", gotID)
	}

	// Dev env: everything but the frontend's own declaration is identical.
	want := withoutFrontendDeclaration(readEnv(t, reference, "dev"), "acme")
	got := withoutFrontendDeclaration(readEnv(t, root, "dev"), "acme")
	if got != want {
		t.Errorf("dev/main.k differs from project new --frontend beyond the frontend declaration.\n--- want\n%s\n--- got\n%s", want, got)
	}
	// Hosted envs are untouched (a hosted identity is billable and changes the
	// env's shape, so it is never added as a side effect of a frontend): they
	// stay exactly what the no-frontend project had.
	for _, env := range []string{"staging", "prod"} {
		got := withoutFrontendDeclaration(readEnv(t, root, env), "acme")
		if got != hostedBefore[env] {
			t.Errorf("%s/main.k was edited by the first frontend (beyond its own frontend declaration):\n%s", env, got)
		}
		for _, snippet := range []string{identityImportSnippet, hostedIdentitySnippet} {
			if strings.Contains(got, snippet) {
				t.Errorf("%s/main.k carries %q: a hosted identity must be opted into by hand", env, snippet)
			}
		}
	}

	// Under KCL: dev runs the IdP and mail catcher, and derives a native OIDC
	// client for the mobile frontend; hosted envs declare none.
	render := func(env string) map[string]any {
		out, err := kclrender.Run(root, filepath.Join(root, "deploy", "kcl", env), []string{"env=" + env})
		if err != nil {
			t.Fatalf("render %s: %v", env, err)
		}
		return decodeRenderOutput(t, out)
	}
	dev := render("dev")
	// A native frontend declares no dev port, so Metro prefers 8081.
	if strings.Contains(readEnv(t, root, "dev"), "_app_frontend_port") {
		t.Error("the mobile frontend must not declare a resolve_port (a declared port beats Metro's 8081 preference)")
	}
	for _, f := range dev["frontends"].([]any) {
		if m := f.(map[string]any); m["name"] == "app" && m["port"] != nil {
			t.Errorf("the mobile frontend renders port %v, want none", m["port"])
		}
	}
	infra := map[string]string{}
	for _, i := range dev["infra"].([]any) {
		m := i.(map[string]any)
		infra[m["name"].(string)], _ = m["engine"].(string)
	}
	if infra["idp"] != "zitadel" || infra["mail"] != "mailpit" {
		t.Errorf("dev infra = %v, want idp (zitadel) and mail (mailpit)", infra)
	}
	idn, _ := dev["identity"].(map[string]any)
	if idn == nil || idn["runtime"] != "host" {
		t.Fatalf("dev identity = %v, want one on the host IdP", dev["identity"])
	}
	native := false
	for _, a := range idn["applications"].([]any) {
		m := a.(map[string]any)
		if m["kind"] == "native" && m["frontend"] == "app" {
			native = true
		}
	}
	if !native {
		t.Errorf("dev identity derives no native client for the mobile frontend: %v", idn["applications"])
	}
	for _, env := range []string{"staging", "prod"} {
		if id := render(env)["identity"]; id != nil {
			t.Errorf("%s identity = %v, want none: hosted envs are not opted in", env, id)
		}
	}

	// A second scaffold must not add the identity again.
	before := map[string]string{}
	for _, env := range []string{"dev", "staging", "prod"} {
		before[env] = readEnv(t, root, env)
	}
	reconcileFirstFrontendIdentity(root, "acme", true)
	if err := runFrontend(context.Background(), testFactory(), frontendArgs{name: "second", kind: "mobile"}); err != nil {
		t.Fatalf("second runFrontend: %v", err)
	}
	for env, was := range before {
		now := readEnv(t, root, env)
		for _, snippet := range []string{identityImportSnippet, devIdentitySnippet, hostedIdentitySnippet, devIdentityInfraSnippet} {
			if strings.Count(now, snippet) > 1 {
				t.Errorf("%s/main.k carries %q twice after a second scaffold", env, snippet)
			}
		}
		if !strings.Contains(now, "_second_frontend") {
			t.Errorf("%s/main.k lost the second frontend's declaration", env)
		}
		_ = was
	}
	if _, err := kclrender.Run(root, filepath.Join(root, "deploy", "kcl", "dev"), []string{"env=dev"}); err != nil {
		t.Errorf("dev no longer renders after a second frontend: %v", err)
	}
}

// decodeRenderOutput is the bundle a render's `output` key holds.
func decodeRenderOutput(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc struct {
		Output map[string]any `json:"output"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Output == nil {
		t.Fatalf("decode render: %v\n%s", err, raw)
	}
	return doc.Output
}

func captureBanner(t *testing.T, root string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	reportFrontendAuthNextStep(root)
	_ = w.Close()
	os.Stdout = prev
	b := make([]byte, 8192)
	n, _ := r.Read(b)
	return string(b[:n])
}

// The banner promises only what the project will do: `forge env up dev` starts
// the IdP the dev env declares, and a project with no IdP and no issuer is told
// so instead of being promised one.
func TestReportFrontendAuthNextStep_IsTrue(t *testing.T) {
	root := t.TempDir()
	writeNoFrontendEnvs(t, root)

	none := captureBanner(t, root)
	if strings.Contains(none, "forge run") || strings.Contains(none, "starts the identity provider") {
		t.Errorf("a project with no IdP must not be promised one:\n%s", none)
	}
	if !strings.Contains(none, "declares no identity provider") {
		t.Errorf("banner should say there is nothing to sign in against:\n%s", none)
	}

	reconcileFirstFrontendIdentity(root, "acme", true)
	with := captureBanner(t, root)
	if strings.Contains(with, "forge run") || !strings.Contains(with, "forge env up dev") {
		t.Errorf("a project with a dev IdP is told to `forge env up dev`:\n%s", with)
	}

	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets", "dev.yaml"), []byte("JWT_ISSUER: \"https://idp.example\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := captureBanner(t, root); got != "" {
		t.Errorf("an already-configured issuer needs no advice, got:\n%s", got)
	}
}
