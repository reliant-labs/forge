package templates

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// One IdP, two readers. The browser sends the user to the issuer's
// /authorize; the server validates what comes back against the issuer's keys.
// If those two names can drift apart, a project logs in successfully and then
// has every RPC rejected — and the failure surfaces as "invalid token", which
// says nothing about the two spellings disagreeing.
//
// The browser cannot read the server's typed config: it is a different
// process, on a different machine, and its values are baked in at build time.
// So there are necessarily two spellings. What must not exist is two
// spellings with nothing holding them together.
//
// These guards derive both sides — the env names the frontend actually reads,
// and the config fields the server actually declares — and fail when a name on
// one side has no counterpart on the other. They read the producers, not a
// list written here: a renamed field breaks them, and so does a browser env
// var that names an issuer the server never learns about.

// browserIssuerEnvRE matches the framework-prefixed env names the frontend
// reads for OIDC configuration. Both prefixes appear because the two browser
// kinds inline env differently (Vite's import.meta.env, Next's process.env).
var browserIssuerEnvRE = regexp.MustCompile(`\b(?:VITE|NEXT_PUBLIC)_(OIDC_[A-Z_]+)\b`)

// browserOIDCEnv returns the OIDC env names the frontend reads, stripped of
// their framework prefix, derived by scanning the rendered provider for both
// browser kinds.
func browserOIDCEnv(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, kind := range []string{"nextjs", "vite-spa"} {
		src := renderFrontend(t, "shared-web/src/lib/auth/oidc-provider.ts.tmpl", kind)
		for _, m := range browserIssuerEnvRE.FindAllStringSubmatch(src, -1) {
			found[m[1]] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("derived NO OIDC env names from the rendered auth provider — the regex matched " +
			"nothing, so every comparison below would pass vacuously")
	}
	return found
}

// serverConfigFields returns the config field names the server declares,
// derived from the config proto template rather than from a list here.
func serverConfigFields(t *testing.T) map[string]bool {
	t.Helper()
	src, err := ProjectTemplates().Render("config.proto.tmpl", struct{ Module string }{Module: "example.com/demo"})
	if err != nil {
		t.Fatalf("render config.proto.tmpl: %v", err)
	}
	fieldRE := regexp.MustCompile(`(?m)^\s*(?:optional\s+)?[a-z0-9_.]+\s+([a-z][a-z0-9_]*)\s*=\s*\d+`)
	found := map[string]bool{}
	for _, m := range fieldRE.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("derived NO fields from config.proto.tmpl — the field regex matched nothing, so " +
			"every comparison below would pass vacuously")
	}
	return found
}

// TestBrowserOIDCEnvHasAServerCounterpart pins that every OIDC value the
// browser reads is one the server also declares, so the two halves of a login
// cannot be configured independently.
//
// The pairing is by name: the browser's OIDC_CLIENT_ID is the server's
// oidc_client_id. The one deliberate exception is the issuer, which the server
// spells jwt_issuer because its validator is provider-agnostic — a project on
// a static JWKS has an issuer and no OIDC flow at all. That exception is named
// here, with its reason, so it is a recorded decision rather than a gap.
func TestBrowserOIDCEnvHasAServerCounterpart(t *testing.T) {
	browser := browserOIDCEnv(t)
	server := serverConfigFields(t)

	// Two values whose spellings differ on purpose, each for the same reason:
	// the server's validator is provider-agnostic and names the JWT property
	// it enforces, while the browser names the OAuth parameter it sends.
	aliases := map[string]string{
		// A project on a static JWKS has an issuer and no OIDC flow at all.
		"OIDC_ISSUER": "jwt_issuer",
		// RFC 8707 calls it `resource` on the wire; what the server checks is
		// the resulting `aud` claim, so it declares jwt_audience. They must
		// carry the SAME string — the browser asks for a token FOR this API
		// and the server admits only tokens minted for it.
		"OIDC_RESOURCE": "jwt_audience",
	}

	var orphans []string
	for env := range browser {
		want, aliased := aliases[env]
		if !aliased {
			want = strings.ToLower(env)
		}
		if !server[want] {
			orphans = append(orphans, env+" (looked for config field "+want+")")
		}
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("the browser reads %d OIDC value(s) the server never declares: %s\n"+
			"A value only the browser knows cannot be validated: the user signs in against it "+
			"and the server rejects every resulting token. Declare the counterpart in "+
			"config.proto.tmpl, or record the pairing in this test's aliases with its reason.",
			len(orphans), strings.Join(orphans, ", "))
	}
}

// TestIssuerAliasIsRealOnBothSides pins that the deliberate issuer aliasing
// above describes something that actually exists. An alias entry naming a
// browser env var nobody reads, or a config field nobody declares, would
// silence the orphan check above for a value that had genuinely gone missing —
// the exception would outlive the thing it excepts.
func TestIssuerAliasIsRealOnBothSides(t *testing.T) {
	browser := browserOIDCEnv(t)
	if !browser["OIDC_ISSUER"] {
		t.Error("no browser kind reads an *_OIDC_ISSUER env var, yet this file carries an alias " +
			"mapping it to jwt_issuer. Either the browser stopped reading an issuer (in which " +
			"case the flow cannot start) or it was renamed and the alias is now dead.")
	}
	if server := serverConfigFields(t); !server["jwt_issuer"] {
		t.Error("config.proto.tmpl declares no jwt_issuer, yet this file aliases the browser's " +
			"OIDC_ISSUER onto it. The server would have no issuer to validate against.")
	}
}
