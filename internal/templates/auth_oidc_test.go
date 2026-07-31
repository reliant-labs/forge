package templates

// Guards for the local OIDC login loop: the opt-in Logto dev service, the
// browser-client config fields, and the property that must survive all of it
// — whether a generated server authenticates stays determinable FROM SOURCE.
//
// Every assertion below derives from a set some PRODUCER computed (the parsed
// compose graph, the parsed proto field table, the parsed Go AST), never from
// a substring of rendered text or a quoted sentence. Each derived set is
// checked for emptiness first: a discovery that silently matches nothing
// would make every assertion downstream pass vacuously, which is worse than
// having no test.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	yaml "gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/pkg/auth"
	"github.com/reliant-labs/forge/pkg/authn"
)

// ─── the compose graph, as compose itself would read it ──────────────────

// composeFile is the subset of the dev compose file these guards reason
// about, decoded from YAML rather than grepped.
type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
	Networks map[string]any            `yaml:"networks"`
}

type composeService struct {
	Image       string            `yaml:"image"`
	Profiles    []string          `yaml:"profiles"`
	Environment map[string]string `yaml:"environment"`
	DependsOn   map[string]struct {
		Condition string `yaml:"condition"`
		Required  *bool  `yaml:"required"`
	} `yaml:"depends_on"`
	Healthcheck struct {
		Test []string `yaml:"test"`
	} `yaml:"healthcheck"`
	Ports []string `yaml:"ports"`
}

// renderCompose renders the dev compose template and decodes it.
func renderCompose(t *testing.T) composeFile {
	t.Helper()
	out, err := ProjectTemplates().Render("docker-compose.yml.tmpl", struct{ ProjectName string }{ProjectName: "shopdemo"})
	if err != nil {
		t.Fatalf("render docker-compose.yml.tmpl: %v", err)
	}
	var cf composeFile
	if err := yaml.Unmarshal(out, &cf); err != nil {
		t.Fatalf("rendered docker-compose.yml is not valid YAML: %v\n%s", err, out)
	}
	if len(cf.Services) == 0 {
		t.Fatalf("the rendered compose file declares NO services — the decode below matched nothing, "+
			"so every assertion in this file would pass vacuously:\n%s", out)
	}
	return cf
}

// activeServices returns the services compose would start for a given set of
// active profiles, applying compose's own rule: a service with no profiles is
// always active; a service with profiles is active only if one of them is
// selected.
func activeServices(cf composeFile, profiles ...string) map[string]bool {
	sel := map[string]bool{}
	for _, p := range profiles {
		sel[p] = true
	}
	out := map[string]bool{}
	for name, svc := range cf.Services {
		if len(svc.Profiles) == 0 {
			out[name] = true
			continue
		}
		for _, p := range svc.Profiles {
			if sel[p] {
				out[name] = true
				break
			}
		}
	}
	return out
}

// TestDevCompose_IdentityProviderIsOptIn is the cost guard. A project that
// wants no browser login — api_key auth, a worker, a service with no frontend
// — must not pay for a ~300 MB IdP container it never calls. The check is
// structural: the IdP is absent from the DEFAULT service set and present once
// its profile is selected, computed with compose's activation rule rather
// than by looking for the word "profiles".
func TestDevCompose_IdentityProviderIsOptIn(t *testing.T) {
	cf := renderCompose(t)

	// Derive which services are profile-gated at all, and fail loudly if the
	// file has stopped gating anything: the `debug` profile has been there
	// since before this change, so an empty set means the decode is wrong.
	gated := map[string][]string{}
	for name, svc := range cf.Services {
		if len(svc.Profiles) > 0 {
			gated[name] = svc.Profiles
		}
	}
	if len(gated) == 0 {
		t.Fatal("no service in the dev compose file is profile-gated — either the decode is broken " +
			"or the gating idiom this change follows has been removed")
	}

	idp, ok := cf.Services["logto"]
	if !ok {
		t.Fatal("the dev compose file declares no `logto` service — a developer cannot complete a real browser sign-in locally")
	}
	if len(idp.Profiles) == 0 {
		// Fatal, not Error: with no profile there is nothing to select
		// below, and continuing would panic rather than report.
		t.Fatal("the logto service is NOT profile-gated, so `docker compose up` pulls a ~300 MB IdP " +
			"into every project — including ones with no browser at all")
	}

	def := activeServices(cf)
	if def["logto"] {
		t.Error("logto is active with NO profile selected — the IdP must be opt-in")
	}
	// The rest of the dev stack must still come up by default; a gating
	// mistake that took postgres or app with it would be caught here.
	for _, required := range []string{"postgres", "app"} {
		if !def[required] {
			t.Errorf("%s is NOT in the default service set — the ordinary dev loop is broken", required)
		}
	}

	with := activeServices(cf, idp.Profiles[0])
	if !with["logto"] {
		t.Errorf("selecting profile %q does not activate logto", idp.Profiles[0])
	}
}

// TestDevCompose_DefaultStackParsesWithoutTheIdP encodes a compose rule that
// is easy to get wrong and fails LOUDLY rather than subtly: a service that
// depends on a profile-gated service makes the WHOLE project invalid when
// that profile is off ("service app depends on undefined service logto"), so
// the default dev stack would stop parsing entirely.
//
// Every dependency edge pointing at a profile-gated service must therefore be
// declared `required: false`.
func TestDevCompose_DefaultStackParsesWithoutTheIdP(t *testing.T) {
	cf := renderCompose(t)

	gatedSvc := map[string]bool{}
	for name, svc := range cf.Services {
		if len(svc.Profiles) > 0 {
			gatedSvc[name] = true
		}
	}
	if len(gatedSvc) == 0 {
		t.Fatal("no profile-gated services found — the discovery is broken, so the edges below are unchecked")
	}

	edges := 0
	for name, svc := range cf.Services {
		for dep, spec := range svc.DependsOn {
			if !gatedSvc[dep] {
				continue
			}
			edges++
			if spec.Required == nil || *spec.Required {
				t.Errorf("service %q depends on profile-gated service %q without `required: false` — "+
					"with that profile off, compose rejects the ENTIRE file as invalid and the default dev stack will not start",
					name, dep)
			}
		}
	}
	if edges == 0 {
		t.Fatal("no dependency edge points at a profile-gated service — either nothing depends on the IdP " +
			"(so the app never waits for it) or this discovery matched nothing")
	}
}

// TestDevCompose_PinsIdentityProviderVersion — a dev stack whose IdP changes
// version underneath it turns "login broke today" into archaeology.
func TestDevCompose_PinsIdentityProviderVersion(t *testing.T) {
	cf := renderCompose(t)
	idp, ok := cf.Services["logto"]
	if !ok {
		t.Fatal("no logto service to check")
	}
	if idp.Image == "" {
		t.Fatal("the logto service declares no image")
	}
	_, tag, found := strings.Cut(idp.Image, ":")
	if !found {
		t.Fatalf("logto image %q carries no tag, so it resolves to :latest", idp.Image)
	}
	if tag == "latest" || tag == "edge" {
		t.Errorf("logto image is pinned to the moving tag %q — pin a MINOR.PATCH version", tag)
	}
	// A bare major ("1") still moves across feature releases.
	if !strings.Contains(tag, ".") {
		t.Errorf("logto image tag %q is not specific enough to reproduce a dev stack", tag)
	}
}

// TestDevCompose_IdentityProviderSharesPostgresWithItsOwnDatabase pins the
// database decision AND the collision that would break it: Logto shares the
// existing postgres service, in a database that is NOT the app's.
func TestDevCompose_IdentityProviderSharesPostgresWithItsOwnDatabase(t *testing.T) {
	cf := renderCompose(t)
	idp, ok := cf.Services["logto"]
	if !ok {
		t.Fatal("no logto service to check")
	}

	dbURL := idp.Environment["DB_URL"]
	if dbURL == "" {
		t.Fatal("the logto service has no DB_URL — it cannot reach a database")
	}
	// It must point at the SHARED postgres service, not a second container.
	if _, dup := cf.Services["logto-postgres"]; dup {
		t.Error("a second postgres container was added for the IdP; dev shares the existing one")
	}
	if !strings.Contains(dbURL, "@postgres:") {
		t.Errorf("logto DB_URL %q does not point at the shared `postgres` service", dbURL)
	}
	if _, ok := idp.DependsOn["postgres"]; !ok {
		t.Error("logto does not depend on postgres, so it can start before the database is ready")
	}

	// The isolation that makes sharing safe: a DIFFERENT database name than
	// the app's, so the app's AUTO_MIGRATE cannot see the IdP's tables.
	// Derive both names from the same rendered file.
	idpDB := dbURL[strings.LastIndex(dbURL, "/")+1:]
	idpDB, _, _ = strings.Cut(idpDB, "?")
	if idpDB == "" {
		t.Fatalf("could not derive the IdP database name from DB_URL %q", dbURL)
	}
	appDB := cf.Services["app"].Environment["DATABASE_URL"]
	if appDB == "" {
		t.Fatal("the app service has no DATABASE_URL to compare against")
	}
	appDBName := appDB[strings.LastIndex(appDB, "/")+1:]
	appDBName, _, _ = strings.Cut(appDBName, "?")
	if idpDB == appDBName {
		t.Errorf("the IdP and the app would share database %q — the app's migrations would run over the IdP's schema", idpDB)
	}
}

// TestDevCompose_IdentityProviderIssuerIsBrowserReachable pins the subtlety
// that breaks a real login: the issuer a provider MINTS TOKENS UNDER must be
// the URL the BROWSER uses, because that string becomes the `iss` claim the
// server enforces. Setting it to the docker-network name produces tokens
// naming a host no browser can resolve.
func TestDevCompose_IdentityProviderIssuerIsBrowserReachable(t *testing.T) {
	cf := renderCompose(t)
	idp, ok := cf.Services["logto"]
	if !ok {
		t.Fatal("no logto service to check")
	}
	endpoint := idp.Environment["ENDPOINT"]
	if endpoint == "" {
		t.Fatal("the logto service sets no ENDPOINT, so it mints tokens under a default issuer nobody configured")
	}
	// The service NAME is exactly what must not appear here.
	for svcName := range cf.Services {
		if strings.Contains(endpoint, "//"+svcName+":") {
			t.Errorf("ENDPOINT %q names the docker service %q, so minted tokens would carry an `iss` "+
				"no browser can resolve", endpoint, svcName)
		}
	}
	if !strings.Contains(endpoint, "localhost") && !strings.Contains(endpoint, "127.0.0.1") {
		t.Errorf("ENDPOINT %q is not browser-reachable", endpoint)
	}

	// Its host ports must be FIXED, not kernel-assigned: a registered OIDC
	// redirect URI is compared literally by the issuer, so a moving port is
	// a login that fails every restart.
	if len(idp.Ports) == 0 {
		t.Fatal("the logto service publishes no ports, so a browser cannot reach it")
	}
	for _, p := range idp.Ports {
		hostPort := p
		if i := strings.LastIndex(p, ":"); i >= 0 {
			hostPort = p[:i]
		}
		if strings.HasSuffix(hostPort, ":0") || hostPort == "0" {
			t.Errorf("logto port mapping %q uses a kernel-assigned host port; a registered redirect URI cannot follow it", p)
		}
	}
}

// ─── the config proto's field table ─────────────────────────────────────

// protoConfigField is one (forge.v1.config)-annotated field, parsed out of
// the rendered config.proto.
type protoConfigField struct {
	Name      string
	Type      string
	EnvVar    string
	Sensitive bool
	Options   string
}

var (
	protoFieldRE = regexp.MustCompile(`(?m)^\s*(?:optional\s+)?([A-Za-z_][\w.]*)\s+([a-z][a-z0-9_]*)\s*=\s*\d+\s*\[\(forge\.v1\.config\)\s*=\s*\{`)
	envVarRE     = regexp.MustCompile(`env_var:\s*"([^"]*)"`)
)

// parseConfigProtoFields parses the rendered config.proto into the annotated
// field table. This is the producer-derived truth source the assertions below
// read; it fails loudly when it finds nothing.
func parseConfigProtoFields(t *testing.T) map[string]protoConfigField {
	t.Helper()
	out, err := ProjectTemplates().Render("config.proto.tmpl", struct{ Module string }{Module: "example.com/proj"})
	if err != nil {
		t.Fatalf("render config.proto.tmpl: %v", err)
	}
	s := string(out)

	fields := map[string]protoConfigField{}
	for _, m := range protoFieldRE.FindAllStringSubmatchIndex(s, -1) {
		typ := s[m[2]:m[3]]
		name := s[m[4]:m[5]]
		// The option block runs from the opening brace to the matching
		// "}];" that closes this field.
		rest := s[m[1]:]
		end := strings.Index(rest, "}]")
		if end < 0 {
			t.Fatalf("config.proto field %q has an unterminated option block", name)
		}
		opts := rest[:end]
		f := protoConfigField{Name: name, Type: typ, Options: opts}
		if mm := envVarRE.FindStringSubmatch(opts); mm != nil {
			f.EnvVar = mm[1]
		}
		f.Sensitive = regexp.MustCompile(`sensitive:\s*true`).MatchString(opts)
		fields[name] = f
	}
	if len(fields) == 0 {
		t.Fatalf("parsed NO (forge.v1.config) fields out of the rendered config.proto — the parser above "+
			"matched nothing, so every assertion below would pass vacuously:\n%s", s)
	}
	// Sanity-check the parser against fields that predate this change.
	for _, known := range []string{"port", "database_url", "jwt_issuer"} {
		if _, ok := fields[known]; !ok {
			t.Fatalf("the config.proto parser did not find the long-standing %q field — it has drifted", known)
		}
	}
	if !fields["database_url"].Sensitive {
		t.Fatal("the parser reports database_url as non-sensitive — its `sensitive` detection is broken, " +
			"so the secret assertions below cannot be trusted")
	}
	return fields
}

// TestConfigProto_OIDCBrowserClientFieldsAreNotSecrets is the security
// property of the new fields. A PUBLIC PKCE client has no client secret, and
// the values it does carry are public by construction: the client ID ships in
// the JS bundle and rides the query string of every authorization request.
// Marking any of them `sensitive` would bind it to a secretKeyRef, claiming a
// confidentiality it does not have and demanding a Secret entry to boot.
func TestConfigProto_OIDCBrowserClientFieldsAreNotSecrets(t *testing.T) {
	fields := parseConfigProtoFields(t)

	// Derive the OIDC browser-client fields from the table rather than
	// listing them, so a field added later is covered automatically.
	var oidc []string
	for name := range fields {
		if strings.HasPrefix(name, "oidc_") {
			oidc = append(oidc, name)
		}
	}
	sort.Strings(oidc)
	if len(oidc) == 0 {
		t.Fatal("config.proto declares no oidc_* fields — the browser flow has no client ID, redirect URI or scopes")
	}

	for _, name := range oidc {
		f := fields[name]
		if f.Sensitive {
			t.Errorf("%s is marked `sensitive: true`. A public PKCE client holds no confidential value; "+
				"binding this to a secretKeyRef implies a confidentiality it does not have", name)
		}
		if f.EnvVar == "" {
			t.Errorf("%s declares no env_var, so nothing can supply it", name)
		}
	}

	// A client SECRET must not have been added as an ordinary field: if a
	// confidential-client variant is ever introduced, it is sensitive.
	for name, f := range fields {
		if strings.Contains(name, "client_secret") && !f.Sensitive {
			t.Errorf("%s looks like a client secret but is not `sensitive: true`", name)
		}
	}
}

// TestConfigProto_NoEnvVarCanDisableAuthentication is the property that must
// survive this change. Whether a generated server authenticates is answered
// in SOURCE — AuthDeps{AnonymousOK: false} plus the protos' auth_required
// allow-list — and no configuration field may be able to flip it. An opt-out
// settable from any shell cannot be reviewed and makes "this server
// authenticates" unprovable from the code.
func TestConfigProto_NoEnvVarCanDisableAuthentication(t *testing.T) {
	fields := parseConfigProtoFields(t)

	// Derive every env var the config system binds, then reject any whose
	// name claims authority over whether auth runs.
	var envVars []string
	for _, f := range fields {
		if f.EnvVar != "" {
			envVars = append(envVars, f.EnvVar)
		}
	}
	if len(envVars) == 0 {
		t.Fatal("no config field declares an env_var — the scan below would be vacuous")
	}
	sort.Strings(envVars)

	// A toggle is a name that pairs an auth/anonymous concept with an
	// enable/disable/bypass verb. This is deliberately a shape, not a
	// blocklist of one spelling: AUTH_ENABLED, DISABLE_AUTH,
	// AUTH_BYPASS and ANONYMOUS_OK all have to fail.
	toggle := regexp.MustCompile(`(?i)^(.*_)?(AUTH|AUTHN|AUTHENTICATION|ANONYMOUS)(_.*)?$`)
	verb := regexp.MustCompile(`(?i)(ENABLE|DISABLED?|BYPASS|SKIP|OFF|OPTIONAL|REQUIRED|OK|MODE|NONE)`)
	for _, ev := range envVars {
		if toggle.MatchString(ev) && verb.MatchString(ev) {
			t.Errorf("config declares env var %q, which reads as a switch over whether authentication runs. "+
				"That decision lives in source (AuthDeps{AnonymousOK: false} + the protos' auth_required "+
				"declarations) and must not be settable from a shell", ev)
		}
	}
}

// ─── the owned SetupAuth scaffold ───────────────────────────────────────

// parseSetupAuth renders and parses internal/app/auth.go, returning the AST
// of the SetupAuth function.
func parseSetupAuth(t *testing.T) (*ast.FuncDecl, string) {
	t.Helper()
	out, err := ProjectTemplates().Render("app-auth.go.tmpl", struct{ Module string }{Module: "example.com/proj"})
	if err != nil {
		t.Fatalf("render app-auth.go.tmpl: %v", err)
	}
	src := string(out)
	fset := token.NewFileSet()
	file, perr := parser.ParseFile(fset, "auth.go", src, parser.AllErrors|parser.ParseComments)
	if perr != nil {
		t.Fatalf("rendered auth.go does not parse: %v\n%s", perr, src)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "SetupAuth" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatalf("the rendered scaffold declares no SetupAuth:\n%s", src)
	}
	return fn, src
}

// TestSetupAuth_HardcodesNoIdentityProvider is the swappability property.
// SetupAuth is the ONE seam a user edits to change IdP, so a vendor name
// baked into it — a default URL, a provider branch, a special case — would
// make Logto the thing you fight rather than the thing you replace.
//
// The check derives every string literal in the function body from the AST
// and rejects any that names a provider or carries an issuer URL.
func TestSetupAuth_HardcodesNoIdentityProvider(t *testing.T) {
	fn, src := parseSetupAuth(t)

	var literals []string
	ast.Inspect(fn, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			v, err := strconv.Unquote(bl.Value)
			if err == nil {
				literals = append(literals, v)
			}
		}
		return true
	})
	if len(literals) == 0 {
		t.Fatal("no string literals found in SetupAuth's body — the AST walk matched nothing, so the " +
			"vendor-name scan below is vacuous")
	}

	// Any URL-ish literal is a hardcoded endpoint, whoever it belongs to.
	for _, lit := range literals {
		if strings.Contains(lit, "://") {
			t.Errorf("SetupAuth contains the endpoint literal %q — every issuer URL must arrive on the typed config", lit)
		}
	}

	// And no provider may be named ANYWHERE in the file, including comments:
	// shipped guidance that says "the Logto default" teaches that there is
	// one, and the whole point is that there is not.
	vendors := []string{"logto", "auth0", "clerk", "okta", "keycloak", "zitadel", "supabase", "firebase", "cognito"}
	lower := strings.ToLower(src)
	// The doc comment legitimately lists alternatives to swap TO. The
	// function BODY is what must be vendor-free.
	bodyStart := int(fn.Body.Pos()) - 1
	bodyLower := strings.ToLower(src[min(bodyStart, len(src)):])
	for _, v := range vendors {
		if strings.Contains(bodyLower, v) {
			t.Errorf("SetupAuth's BODY mentions %q — the validator it builds must be provider-agnostic", v)
		}
	}
	_ = lower
}

// TestSetupAuth_DocCommentStaysAttached guards a mistake this change actually
// made and the e2e caught 180 seconds later: inserting a declaration between
// SetupAuth's doc comment and its signature detaches the comment, and the
// scaffold then emits an exported symbol with no doc — which makes the
// scaffolded .golangci.yml's promise ("re-enabling revive: exported only
// reports code the USER wrote") false.
//
// Derived from the AST: SetupAuth's Doc must be non-nil and must start with
// its own name, which is exactly what revive's `exported` rule checks.
func TestSetupAuth_DocCommentStaysAttached(t *testing.T) {
	fn, _ := parseSetupAuth(t)
	if fn.Doc == nil || len(fn.Doc.List) == 0 {
		t.Fatal("SetupAuth has no doc comment attached. If the prose is still in the file, something was " +
			"inserted BETWEEN the comment and the func — move it above the comment. revive's `exported` " +
			"rule fails on this, and the scaffold promises it never fires on forge-authored code")
	}
	first := strings.TrimSpace(strings.TrimPrefix(fn.Doc.List[0].Text, "//"))
	if !strings.HasPrefix(first, "SetupAuth") {
		t.Errorf("SetupAuth's doc comment must start with the symbol name (revive `exported`); it starts %q", first)
	}
}

// TestSetupAuth_ReadsConfigNotEnvironment keeps the one configuration
// channel. os.Getenv here would be a second one, invisible to the deploy
// projection and to review.
func TestSetupAuth_ReadsConfigNotEnvironment(t *testing.T) {
	_, src := parseSetupAuth(t)
	for _, banned := range []string{"os.Getenv", "os.LookupEnv", "os.Environ", "nolint:forbidigo", `"os"`} {
		if strings.Contains(src, banned) {
			t.Errorf("internal/app/auth.go must read configuration off the typed cfg, never %s", banned)
		}
	}
}

// TestSetupAuth_FailsClosedOnEveryErrorPath is the honesty property: an
// issuer it cannot reach must REFUSE TO START, not degrade to a validator
// that accepts nothing while the server reports healthy.
//
// Derived from the AST: every `if err != nil` (and every other error branch)
// inside SetupAuth must return a non-nil error and a nil validator. A branch
// that swallowed a discovery failure and continued would fail here.
func TestSetupAuth_FailsClosedOnEveryErrorPath(t *testing.T) {
	fn, src := parseSetupAuth(t)

	returns := 0
	bad := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 2 {
			return true
		}
		returns++
		validatorIsNil := isNilIdent(ret.Results[0])
		errIsNil := isNilIdent(ret.Results[1])
		// The only legal shapes are (validator, nil) and (nil, err).
		if validatorIsNil && errIsNil {
			bad++
			t.Errorf("SetupAuth has a `return nil, nil` — that hands the interceptor no validator AND no "+
				"error, which is the auth-off shape:\n%s", src)
		}
		return true
	})
	if returns == 0 {
		t.Fatal("no two-value return statements found in SetupAuth — the AST walk matched nothing")
	}
	if returns < 2 {
		t.Errorf("SetupAuth has only %d return path(s); a scaffold that both succeeds and refuses needs at least two", returns)
	}
	_ = bad
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// TestSetupAuth_DiscoveryFailureNamesTheIssuer pins the debuggability half of
// refusing to start: the message must carry the URL that could not be
// reached, which is the existing documented behaviour for JWKS. A bare
// "discovery failed" sends the reader to a packet capture.
func TestSetupAuth_DiscoveryFailureNamesTheIssuer(t *testing.T) {
	fn, _ := parseSetupAuth(t)

	// Find the error-wrapping calls and confirm at least one interpolates the
	// configured issuer into its message.
	var wrapped []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Errorf" || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		format, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		// Record the format only when one of its arguments is the issuer
		// field — that is what "names the URL" means.
		for _, arg := range call.Args[1:] {
			if s, ok := arg.(*ast.SelectorExpr); ok && s.Sel.Name == "JwtIssuer" {
				wrapped = append(wrapped, format)
			}
		}
		return true
	})
	if len(wrapped) == 0 {
		t.Fatal("no error in SetupAuth interpolates cfg.JwtIssuer — an unreachable or misconfigured issuer " +
			"would fail with a message that does not say which URL was tried")
	}
	for _, format := range wrapped {
		if !strings.Contains(format, "%q") && !strings.Contains(format, "%s") {
			t.Errorf("error format %q takes the issuer but does not render it", format)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ─── the shipped auth guidance ──────────────────────────────────────────

// authSkills returns the auth skill and its sub-skills, keyed by skill path.
func authSkills(t *testing.T) map[string]string {
	t.Helper()
	files, err := ProjectTemplates().List("skills/forge/auth")
	if err != nil {
		t.Fatalf("list auth skills: %v", err)
	}
	out := map[string]string{}
	for _, rel := range files {
		if !strings.HasSuffix(rel, "SKILL.md") {
			continue
		}
		// List returns paths relative to the directory it was given.
		full := "skills/forge/auth/" + strings.TrimPrefix(rel, "/")
		body, err := ProjectTemplates().Get(full)
		if err != nil {
			t.Fatalf("read %s: %v", full, err)
		}
		out["forge/auth/"+rel] = string(body)
	}
	if len(out) == 0 {
		t.Fatal("no auth SKILL.md files found — the discovery matched nothing, so the checks below are vacuous")
	}
	return out
}

// TestAuthSkill_ConfigFieldClaimsExist keeps the shipped guidance honest about
// the config surface. A skill that names a field the proto does not declare
// teaches an agent to write code that will not compile — and the failure lands
// on a user, not here.
//
// The truth source is the parsed proto field table; the claims are the
// backtick-quoted `snake_case` identifiers in the guidance that LOOK like
// config fields (the jwt_*/oidc_* families this change touches).
func TestAuthSkill_ConfigFieldClaimsExist(t *testing.T) {
	fields := parseConfigProtoFields(t)
	claimRE := regexp.MustCompile("`((?:jwt|oidc)_[a-z0-9_]+)`")

	checked := 0
	for rel, content := range authSkills(t) {
		for _, m := range claimRE.FindAllStringSubmatch(content, -1) {
			name := m[1]
			checked++
			if _, ok := fields[name]; !ok {
				t.Errorf("skills/%s names config field %q, which proto/config/v1/config.proto does not declare", rel, name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("the auth guidance names NO jwt_*/oidc_* config field — either the guidance stopped " +
			"documenting the config surface or this scan is broken")
	}
}

// TestAuthSkill_TeachesPublicRPCClaimsAbsence guards the correction this
// change makes. The interceptor checks the proto-declared allow-list BEFORE
// reading the Authorization header, so a public RPC receives NO claims even
// from a caller who presented a valid token — and a handler that assumes
// otherwise nil-panics on every anonymous call.
//
// This is asserted against forge/pkg/authn's REAL behaviour, not against the
// prose: the test drives the interceptor and only then requires the guidance
// to document what it observed. If authn's behaviour ever changes, this fails
// and the guidance must be rewritten rather than silently going stale.
func TestAuthSkill_TeachesPublicRPCClaimsAbsence(t *testing.T) {
	// Derived fact: does a public procedure see claims when a token is sent?
	claimsOnPublicRPC := publicRPCReceivesClaims(t)
	if claimsOnPublicRPC {
		t.Fatal("forge/pkg/authn now attaches claims to allow-listed procedures when a token is offered. " +
			"That is the OPPOSITE of what the shipped auth guidance teaches — update " +
			"skills/forge/auth/SKILL.md before this ships, or a public handler written from it will be wrong")
	}

	skill, ok := authSkills(t)["forge/auth/SKILL.md"]
	if !ok {
		t.Fatal("the auth skill is missing")
	}
	// The guidance must show the safe read (checking the second return) and
	// must not present GetUser as the way to read identity in a public
	// handler.
	if !strings.Contains(skill, "ClaimsFromContext") {
		t.Error("the auth skill never mentions ClaimsFromContext, so it cannot teach the two-case public handler; " +
			"a handler written from it will assume a principal and nil-panic on anonymous calls")
	}
	if !strings.Contains(skill, "nil-panic") && !strings.Contains(skill, "nil panic") {
		t.Error("the auth skill does not warn that a blind claims dereference nil-panics on an anonymous call")
	}
}

// publicRPCReceivesClaims drives the REAL interceptor: an allow-listed
// procedure, a caller presenting a valid token, and a handler that reports
// whether any principal reached it.
func publicRPCReceivesClaims(t *testing.T) bool {
	t.Helper()
	const public = "/shop.v1.Catalog/ListItems"

	ic, err := authn.NewInterceptor(authn.Policy{
		ValidatorConfigured: true,
		Validate: func(string) (*auth.Claims, error) {
			return &auth.Claims{UserID: "user-1"}, nil
		},
		AnonymousOK:       false,
		Unauthenticated:   map[string]struct{}{public: {}},
		ContextWithClaims: authn.ContextWithClaims,
	})
	if err != nil {
		t.Fatalf("build interceptor: %v", err)
	}

	saw := false
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		_, ok := authn.ClaimsFromContext(ctx)
		saw = ok
		return nil, nil
	})
	r := &allowListProbeRequest{procedure: public}
	r.Header().Set("Authorization", "Bearer a-valid-token")
	if _, err := ic.WrapUnary(next)(context.Background(), r); err != nil {
		t.Fatalf("an allow-listed procedure must not be rejected: %v", err)
	}
	return saw
}

// allowListProbeRequest is the minimum connect.AnyRequest the interceptor
// reads: a procedure and headers.
type allowListProbeRequest struct {
	connect.AnyRequest
	procedure string
	header    http.Header
}

func (r *allowListProbeRequest) Spec() connect.Spec {
	return connect.Spec{Procedure: r.procedure}
}

func (r *allowListProbeRequest) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}
