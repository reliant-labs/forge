package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/generator"
)

// A project born WITHOUT a frontend carries no identity: `forge project new`
// renders the dev IdP, the mail catcher and the `identity` declaration only in
// the HasFrontend sections of the env templates. Adding the project's first
// frontend later used to bind just the frontend, so the app stayed on its mock
// auth provider with no IdP to sign in against (and no native OIDC client, which
// is derived from the declared identity).
//
// reconcileFirstFrontendIdentity closes that gap: when the FIRST frontend is
// scaffolded into a project whose env KCL carries none of the identity wiring,
// it adds exactly what `forge project new --frontend` renders, as anchored
// insertions into the user's own files, never a rewrite:
//
//   - deploy/kcl/identity.k, write-if-absent (generator.ScaffoldIdentityKCL);
//   - dev/main.k: the `idn` import, the IdP and mail ports, the idp and mail
//     HostInfra entries, the CORS-credentials env piece, and the `identity`
//     declaration on the host IdP;
//   - each hosted env's main.k: the `idn` import and `identity = idn.IDENTITY`.
//
// It never fights the user. A project that already declares an identity, that
// already has a frontend, or that has identity.k but no wiring in its env (the
// user removed it) is left alone with a one-line note. Each file is edited
// atomically: if any anchor is missing the file is untouched and the note says
// so. It is idempotent: the second run finds the wiring and does nothing.
//
// The snippets below must stay byte-identical to the HasFrontend sections of
// deploy/kcl/dev/main.k.tmpl and deploy/kcl/cloud/main.k.tmpl;
// TestIdentitySnippetsMatchTemplates pins that.

const (
	devIdentityMainRel = "deploy/kcl/dev/main.k"

	identityImportSnippet = "\n# The users — declared once, shared by every env; only where they run differs.\n" +
		"import ..identity as idn\n"

	hostedIdentitySnippet = "    # THE USERS (deploy/kcl/identity.k), hosted: the control plane gives this\n" +
		"    # env its own identity instance and delivers its issuer, audience, client\n" +
		"    # id and broker token to the API (forge.IdentityRef): nothing is copied by\n" +
		"    # hand. Bringing your own issuer instead? Delete this line and set\n" +
		"    # jwt_issuer / oidc_client_id in config.k.\n" +
		"    identity = idn.IDENTITY\n"

	devIdentitySnippet = "    # THE USERS, declared once in deploy/kcl/identity.k and run here on the dev\n" +
		"    # IdP above. `forge env up` converges them with the same library the\n" +
		"    # control plane runs for a hosted env, and fills the API's issuer,\n" +
		"    # audience, client id, redirect and login URIs, IdP base and broker token\n" +
		"    # from the result (forge.IdentityRef): nothing is copied by hand.\n" +
		"    identity = idn.IDENTITY | {runtime = forge.IdentityOnHost {infra = \"idp\"}}\n"

	devIdentityInfraSnippet = "        # The dev identity provider — a single static binary, supervised the\n" +
		"        # same way. `idp-steps.yaml` is its declarative bootstrap; its state\n" +
		"        # lives in its OWN database on the postgres above.\n" +
		"        #\n" +
		"        # NO SIGN-IN LOOP AT ALL? Drop this entry and `identity` below; the\n" +
		"        # frontend then runs against its mock auth provider.\n" +
		"        forge.HostInfra {\n" +
		"            name = \"idp\"\n" +
		"            engine = \"zitadel\"\n" +
		"            port = _idp_port\n" +
		"            idp_database_port = _postgres_port\n" +
		"        }\n" +
		"        # The dev mail catcher: the IdP's invitations, verification codes,\n" +
		"        # password resets and magic links land here instead of in an inbox.\n" +
		"        # Open the web UI `forge env up` prints (port + 7000).\n" +
		"        forge.HostInfra {\n" +
		"            name = \"mail\"\n" +
		"            engine = \"mailpit\"\n" +
		"            port = _mail_port\n" +
		"        }\n"

	devSignInEnvSnippet = "    # The session cookie /auth/login sets only survives a cross-origin call\n" +
		"    # when the browser is told credentials are allowed. In dev the API and the\n" +
		"    # frontend are on different ports, so without this sign-in \"does nothing\".\n" +
		"    _sign_in = {CORS_ALLOW_CREDENTIALS = \"true\"} if w.kind == \"service\" else {}\n"
)

// devIdentityPortsSnippet is the IdP and mail ports, keyed by project name.
func devIdentityPortsSnippet(projectName string) string {
	return "# The dev IdP's origin must stay stable BETWEEN runs. resolve_port chooses\n" +
		"# a free port once, then reuses its reservation even while the IdP answers.\n" +
		"# Unlike a fixed 100-port offset, it cannot land on another stack's HyperDX.\n" +
		fmt.Sprintf("_idp_port = plugin.resolve_port(%q, 8080)\n", projectName+"-dev-idp") +
		"_idp_origin = \"http://localhost:${_idp_port}\"\n" +
		"\n" +
		"# The dev mail catcher's SMTP port. Its web UI is on this port + 7000 (the\n" +
		"# default 1025 puts it on http://localhost:8025, which `forge env up` prints).\n" +
		fmt.Sprintf("_mail_port = plugin.resolve_port(%q, 1025)\n", projectName+"-dev-mail")
}

var (
	// A frontend declaration is an assignment (`_web_frontend = forge.Frontend {`);
	// the binder lambdas also spell `-> forge.Frontend {`, which is not one.
	frontendLiteralRE = regexp.MustCompile(`=\s*forge\.Frontend\s*\{`)
	// identityWiringRE is any sign the env already declares (or has chosen
	// about) its identity: the shared import, the bundle field, the dev IdP,
	// or the IdP port the dev wiring is built on.
	identityWiringRE = regexp.MustCompile(`(?m)import\s+\.\.identity\b|^\s*identity\s*=|\b_idp_port\b|\b_mail_port\b`)

	hostedControlPlaneRE = regexp.MustCompile(`(?m)^\s*control_plane\s*=\s*forge\.ControlPlane`)
	workloadsImportRE    = regexp.MustCompile(`(?m)^import \.\.workloads as wl\n`)
	pprofPortLambdaRE    = regexp.MustCompile(`(?ms)^_pprof_port_of = lambda[^\n]*\n.*?^}\n`)
	postgresPortRE       = regexp.MustCompile(`(?m)^_postgres_port\s*=`)
	infraOpenRE          = regexp.MustCompile(`(?m)^    infra = \[\n`)
	infraCloseRE         = regexp.MustCompile(`(?m)^    \]\n`)
	envFinalExprRE       = regexp.MustCompile(`(?m)^(    config_gen\.[^\n]*\| w\.env \| _machine \| _port)\n`)
	envLambdaRE          = regexp.MustCompile(`(?m)^_env = lambda`)
	secretProviderRE     = regexp.MustCompile(`(?m)^    secret_provider = `)
)

type identityState int

const (
	// identityFresh: no frontend and no identity anywhere in the env file.
	identityFresh identityState = iota
	// identityHasFrontend: the env already declares a frontend, so this is not
	// the project's first.
	identityHasFrontend
	// identityDeclared: the env already declares identity wiring.
	identityDeclared
)

func classifyEnvIdentity(content string) identityState {
	stripped := codegen.StripKCLProse(content)
	switch {
	case frontendLiteralRE.MatchString(stripped):
		return identityHasFrontend
	case identityWiringRE.MatchString(stripped) || devIdentityProviderRE.MatchString(stripped):
		return identityDeclared
	}
	return identityFresh
}

// devIdentityWillBeAdded reports whether scaffolding the first frontend into
// root will add the dev identity: the same decision reconcileFirstFrontendIdentity
// acts on, asked before the frontend's files are rendered so the sign-in mode
// default can follow it.
func devIdentityWillBeAdded(root string, firstFrontend bool) bool {
	if !firstFrontend {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, codegen.IdentityKCLRelPath)); err == nil {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(root, devIdentityMainRel))
	if err != nil {
		return false
	}
	if classifyEnvIdentity(string(raw)) != identityFresh {
		return false
	}
	_, err = spliceDevIdentity(string(raw), "probe")
	return err == nil
}

// reconcileFirstFrontendIdentity adds the dev identity wiring to a project
// whose first frontend is being scaffolded. Advisory, like declareFrontendInKCL:
// the frontend's code is already written, so nothing here fails the scaffold.
func reconcileFirstFrontendIdentity(root, projectName string, firstFrontend bool) {
	if !firstFrontend {
		return
	}
	devPath := filepath.Join(root, devIdentityMainRel)
	raw, err := os.ReadFile(devPath)
	if err != nil {
		return // no dev env (e.g. --disable deploy): nothing to wire
	}
	switch classifyEnvIdentity(string(raw)) {
	case identityHasFrontend:
		return
	case identityDeclared:
		fmt.Printf("   ℹ %s already declares an identity; left as is\n", devIdentityMainRel)
		return
	}
	if _, err := os.Stat(filepath.Join(root, codegen.IdentityKCLRelPath)); err == nil {
		fmt.Printf("   ℹ %s exists but %s declares no identity — treating that as your choice; left as is\n",
			codegen.IdentityKCLRelPath, devIdentityMainRel)
		return
	}

	updated, err := spliceDevIdentity(string(raw), projectName)
	if err != nil {
		fmt.Printf("   ℹ dev identity not added to %s (%v); compare with `forge project new <name> --frontend web`, or see `forge skill load auth/dev-loop`\n",
			devIdentityMainRel, err)
		return
	}
	if err := atomicfile.WriteFile(devPath, []byte(updated), 0o644); err != nil {
		fmt.Printf("   ℹ dev identity not added: could not update %s: %v\n", devIdentityMainRel, err)
		return
	}
	if err := generator.ScaffoldIdentityKCL(root, projectName); err != nil {
		fmt.Printf("   ℹ could not write %s: %v\n", codegen.IdentityKCLRelPath, err)
		return
	}
	fmt.Printf("   - %s (dev identity provider, mail catcher and identity declared)\n   - %s\n",
		devIdentityMainRel, codegen.IdentityKCLRelPath)

	envs, err := os.ReadDir(filepath.Join(root, "deploy", "kcl"))
	if err != nil {
		return
	}
	for _, env := range envs {
		if !env.IsDir() || env.Name() == codegen.DevEnvName {
			continue
		}
		rel := filepath.Join("deploy", "kcl", env.Name(), "main.k")
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue // not an env directory
		}
		if !hostedControlPlaneRE.MatchString(codegen.StripKCLProse(string(content))) {
			continue // not a hosted env: where its identity runs is the author's call
		}
		if classifyEnvIdentity(string(content)) != identityFresh {
			continue
		}
		out, err := spliceHostedIdentity(string(content))
		if err != nil {
			fmt.Printf("   ℹ identity not added to %s (%v)\n", rel, err)
			continue
		}
		if err := atomicfile.WriteFile(filepath.Join(root, rel), []byte(out), 0o644); err != nil {
			fmt.Printf("   ℹ identity not added: could not update %s: %v\n", rel, err)
			continue
		}
		fmt.Printf("   - %s (identity declared)\n", rel)
	}
}

// beforeLeadingComments moves an insertion point at the start of a line up
// over the comment lines directly above it, so a field's own comment stays
// attached to the field.
func beforeLeadingComments(content string, at int) int {
	for at > 0 {
		prevStart := strings.LastIndex(content[:at-1], "\n") + 1
		if !strings.HasPrefix(content[prevStart:at], "    #") {
			break
		}
		at = prevStart
	}
	return at
}

// uniqueMatch is the submatch indexes of the single match of re in s.
func uniqueMatch(re *regexp.Regexp, s, what string) ([]int, error) {
	m := re.FindAllStringSubmatchIndex(s, -1)
	if len(m) != 1 {
		return nil, fmt.Errorf("found %d places matching %s, need exactly one", len(m), what)
	}
	return m[0], nil
}

// spliceDevIdentity is the pure core of the dev edit. Every anchor is checked
// before anything is changed, so a file it cannot place unambiguously comes
// back untouched with the error naming the anchor.
func spliceDevIdentity(content, projectName string) (string, error) {
	if !postgresPortRE.MatchString(content) {
		return "", fmt.Errorf("no `_postgres_port`: the dev IdP keeps its state in the project's postgres")
	}
	imp, err := uniqueMatch(workloadsImportRE, content, "`import ..workloads as wl`")
	if err != nil {
		return "", err
	}
	ports, err := uniqueMatch(pprofPortLambdaRE, content, "the `_pprof_port_of` lambda")
	if err != nil {
		return "", err
	}
	infraOpen, err := uniqueMatch(infraOpenRE, content, "`infra = [`")
	if err != nil {
		return "", err
	}
	infraClose := infraCloseRE.FindStringIndex(content[infraOpen[1]:])
	if infraClose == nil {
		return "", fmt.Errorf("no closing `]` for `infra = [`")
	}
	infraCloseAt := infraOpen[1] + infraClose[0]
	envStart, err := uniqueMatch(envLambdaRE, content, "the `_env` lambda")
	if err != nil {
		return "", err
	}
	finalExpr, err := uniqueMatch(envFinalExprRE, content, "the `_env` lambda's final expression")
	if err != nil {
		return "", err
	}
	sp, err := uniqueMatch(secretProviderRE, content, "`secret_provider =`")
	if err != nil {
		return "", err
	}
	if !(imp[1] <= ports[0] && ports[1] <= envStart[0] && envStart[0] < finalExpr[0] && finalExpr[1] <= infraOpen[0] && infraCloseAt < sp[0]) {
		return "", fmt.Errorf("the file's sections are not in the scaffolded order")
	}

	type edit struct {
		at   int
		text string
		// replace is the length of source text the edit replaces (0 = pure insert).
		replace int
	}
	exprEnd := finalExpr[3] // end of capture group 1: the line without its newline
	edits := []edit{
		{at: imp[1], text: identityImportSnippet},
		{at: ports[1], text: "\n" + devIdentityPortsSnippet(projectName)},
		{at: finalExpr[2], text: devSignInEnvSnippet},
		{at: exprEnd, text: " | _sign_in"},
		{at: infraCloseAt, text: devIdentityInfraSnippet},
		{at: beforeLeadingComments(content, sp[0]), text: devIdentitySnippet},
	}
	// Apply back to front so earlier offsets stay valid. The list is in file
	// order: the section order was checked above.
	out := content
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		out = out[:e.at] + e.text + out[e.at+e.replace:]
	}
	return out, nil
}

// spliceHostedIdentity adds the `idn` import and `identity = idn.IDENTITY` to a
// hosted env's main.k.
func spliceHostedIdentity(content string) (string, error) {
	imp, err := uniqueMatch(workloadsImportRE, content, "`import ..workloads as wl`")
	if err != nil {
		return "", err
	}
	sp, err := uniqueMatch(secretProviderRE, content, "`secret_provider =`")
	if err != nil {
		return "", err
	}
	if imp[1] > sp[0] {
		return "", fmt.Errorf("the file's sections are not in the scaffolded order")
	}
	at := beforeLeadingComments(content, sp[0])
	out := content[:at] + hostedIdentitySnippet + content[at:]
	return out[:imp[1]] + identityImportSnippet + out[imp[1]:], nil
}
