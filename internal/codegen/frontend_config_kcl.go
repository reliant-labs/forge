package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
)

// FrontendConfigModule is the generated KCL module name carrying every
// frontend's typed schema plus its runtime-config projection.
//
// It is a SEPARATE module from config_gen.k (the backend's) because the two
// have different audiences and different rules: config_gen.k models secret
// references and projects to a workload's env, while this one models a
// strictly public surface and projects to a JSON document served to a
// browser. Keeping them apart makes the boundary legible in the file tree —
// anything in here is public by construction.
const FrontendConfigModule = "frontend_config_gen"

// GenerateFrontendConfigNative emits the project-level, forge-owned
// frontend config module at <kclDirAbs>/frontend_config_gen.k.
//
// It mirrors GenerateConfigNativeShared's ownership handling so both KCL
// config modules land under the same tier and the same checksum ledger.
// When no frontend is annotated, any previously-emitted module is removed
// rather than left behind stale.
func GenerateFrontendConfigNative(configs []FrontendConfig, projectName, projectDir, kclDirAbs string, cs *checksums.FileChecksums) error {
	outPath := filepath.Join(kclDirAbs, FrontendConfigModule+".k")

	if len(configs) == 0 {
		// Nothing annotated: withdraw a module from a previous run so a
		// removed annotation cannot leave a schema nothing projects into.
		if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", outPath, err)
		}
		return nil
	}

	body, err := GenerateFrontendConfigKCL(configs, projectName)
	if err != nil {
		return fmt.Errorf("generate frontend config module: %w", err)
	}

	if cs != nil && projectDir != "" {
		if rel, rerr := filepath.Rel(projectDir, outPath); rerr == nil {
			if werr := writeForgeOwned(projectDir, rel, []byte(body), cs); werr != nil {
				return fmt.Errorf("write %s: %w", outPath, werr)
			}
			return nil
		}
	}
	if werr := writeUserScaffold(outPath, []byte(body)); werr != nil {
		return fmt.Errorf("write %s: %w", outPath, werr)
	}
	return nil
}

// EnsureIDPIdentityStub write-if-absent seeds
// <kclDirAbs>/<env>/idp_identity_gen.k with four empty values, so a fresh
// clone's config.k (which imports it unconditionally once devIdentity is
// on) renders a complete configuration BEFORE the idp-provision job has
// ever run.
//
// It is the SAME file the job itself overwrites on every successful
// convergence (see pkg/devidp.KCLFilePublisher) — forge seeds it once,
// at generate time, and ownership passes to the job from there. That
// split (forge seeds the shape, the job owns the values) is what lets
// this ship as a committed file: `forge generate` on a machine that has
// never run the dev stack still produces a file that exists and parses,
// and the job's later run only ever changes its contents, never its
// presence.
func EnsureIDPIdentityStub(kclDirAbs, envName string) error {
	path := filepath.Join(kclDirAbs, envName, IDPIdentityModule+".k")
	body := "# Seeded by forge; OVERWRITTEN by this project's `auth idp-provision` job on\n" +
		"# every successful convergence (deploy/kcl/workloads.k's " + IDPProvisionWorkloadName + " workload).\n" +
		"#\n" +
		"# Empty until that job has run at least once — see EnsureIDPIdentityStub.\n" +
		"# Empty is a valid, working state: it selects the no-auth posture rather\n" +
		"# than a half-configured one.\n" +
		"idp_identity = {\n" +
		"    \"client_id\" = \"\"\n" +
		"    \"audience\" = \"\"\n" +
		"    \"issuer\" = \"\"\n" +
		"    \"jwks_url\" = \"\"\n" +
		"}\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	_, err := writeUserScaffoldIfAbsent(path, []byte(body))
	return err
}

// EnsureFrontendConfigInstances makes sure an env's user-owned config.k
// declares one typed VALUES instance per frontend config — the half the
// runtime projection reads.
//
// It appends rather than scaffolding a whole file, because config.k is
// shared with the backend's app_config and is written once
// (write-if-absent). A frontend annotated AFTER the env was created — the
// normal order, since a project scaffolds its envs long before it declares
// a frontend config — would otherwise never get an instance, leaving the
// projection nothing to project and every environment silently on proto
// defaults.
//
// Only MISSING instances are appended, matched by schema name, so this is
// idempotent and never touches values an author has edited. Returns the
// frontends it added an instance for.
// devIdentity asks for the dev-IdP wiring: the frontend's oidc_issuer,
// oidc_redirect_uri and — the two values that cannot be declared —
// oidc_client_id and the audience, read from the committed identity file
// the idp-provision job publishes into. See renderDevIdentityFields.
// backendFields is the backend AppConfig's field set, used only to resolve
// the JWT field NAMES for the dev-identity binding (see
// backendDevIdentityPatch). Nil is fine: it simply skips that binding.
func EnsureFrontendConfigInstances(configs []FrontendConfig, kclDirAbs, envName, projectName string, devIdentity bool, backendFields []ConfigField) ([]string, error) {
	if len(configs) == 0 {
		return nil, nil
	}
	path := filepath.Join(kclDirAbs, envName, "config.k")
	src, err := os.ReadFile(path)
	if err != nil {
		// No config.k yet: the backend scaffolder owns creating it, and it
		// runs in the same pass. Nothing to append to is not an error.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	body := string(src)
	var (
		added   []string
		appends strings.Builder
	)
	for _, fc := range configs {
		schemaName, _ := KCLFrontendConfigName(fc.MessageName)
		if frontendConfigInstanceDeclared(body, schemaName) {
			continue
		}
		appends.WriteString(renderFrontendConfigInstance(fc, schemaName, envName, projectName, devIdentity))
		added = append(added, fc.Frontend)
	}
	// The import rides on whether an instance ACTUALLY reads the published
	// identity, not on the caller's intent: a project whose frontend config
	// declares no OIDC fields gets no identity block, and an import with no
	// read site is a dependency the render does not need — which costs a
	// `kcl run` of this file, before the idp-provision job has ever run, the
	// ability to evaluate it at all.
	//
	// An ALREADY-PRESENT frontend instance counts as much as one being
	// appended now. That is what lets an existing project — whose config.k
	// was written before the backend half was wired, and which therefore
	// appends nothing on this pass — still be backfilled below.
	readsIdentity := strings.Contains(appends.String(), idpIdentityLookup) ||
		(devIdentity && strings.Contains(body, idpIdentityLookup))

	// Bind the BACKEND's half of the same identity, INSIDE the existing
	// app_config block. The frontend instance tells the browser where to sign
	// in; without this the server it then calls has no key material, so
	// sign-in succeeds and every RPC 401s — the failure `forge doctor`'s
	// auth-parity check exists to name, and the default state of every
	// scaffolded project until now.
	//
	// Deliberately BEFORE the len(added) == 0 return: an existing project has
	// nothing to append and is exactly the one that needs this backfill.
	patched := insertBackendDevIdentity(body, readsIdentity, backendFields)
	if len(added) == 0 {
		if patched == body {
			return nil, nil
		}
		if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		return nil, nil
	}
	body = patched

	var out strings.Builder
	out.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		out.WriteString("\n")
	}
	if !strings.Contains(body, "import "+FrontendConfigModule) {
		out.WriteString("\n# ── Frontend runtime config (PUBLIC — served to the browser) ──────────\n")
		out.WriteString("# Projected into each frontend's config.js. These values reach the browser\n")
		out.WriteString("# at RUNTIME, not baked into the bundle, so one built bundle can be\n")
		out.WriteString("# promoted between environments.\n")
		fmt.Fprintf(&out, "import %s\n", FrontendConfigModule)
	}
	if readsIdentity && !strings.Contains(body, idpIdentityImport) {
		out.WriteString(devIdentityPreamble)
	}
	out.WriteString(appends.String())

	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return added, nil
}

// backendJWTOverrideMarker is the one spelling of the backend identity
// binding's opening line — used both to write it and to detect that it is
// already there, so the two cannot drift.
//
// It is a COMMENT rather than a KCL statement because the binding is
// inserted INTO the existing app_config block. KCL variables are immutable,
// so the obvious `app_config = app_config | {…}` append is a hard
// ImmutableError, not a style preference.
const backendJWTOverrideMarker = "# ── Backend identity: the OTHER half of dev sign-in ──"

// insertBackendDevIdentity returns body with the BACKEND's half of the dev
// identity added inside its app_config block — or body unchanged when that
// would be wrong or redundant.
//
// WHY THIS EXISTS. Sign-in and token VALIDATION are two halves of one fact,
// and until now only the browser's half was wired. The result was a dev stack
// that looked configured and worked for exactly one step: the IdP minted a
// real token, the server had no key material to verify it, and every
// authenticated RPC answered 401 with a signing-material complaint. The
// failure presents as a broken token — so the search starts at the validator,
// which is correct, and is simply being shown a token it was never told how
// to check. `forge doctor`'s auth-parity check already names this exact case
// ("the frontend names an issuer but the backend validates against none"),
// which is the tell that it was a wiring gap rather than a deliberate
// posture.
//
// It INSERTS INTO the existing block rather than appending an override after
// it, because KCL variables are immutable: the obvious
// `app_config = app_config | {…}` is a hard ImmutableError, not a style
// choice. Insertion also keeps every value the author already wrote — the
// fields are added, nothing is rewritten.
//
// Existing projects are reached because this runs on the same
// append-if-missing pass the frontend instance does, which is what lets a
// config.k written long before the identity fields existed pick them up.
func insertBackendDevIdentity(body string, readsIdentity bool, backend []ConfigField) string {
	// No identity block was emitted (no OIDC fields, or not the dev env), so
	// there is no published issuer to point the backend at.
	if !readsIdentity {
		return body
	}
	// Resolve the field names by ENV VAR rather than hardcoding them — the
	// same rule devIdentityFieldNames follows on the frontend side, so a
	// project that renames a proto field keeps working and one that removed
	// the field gets no binding instead of a reference to nothing.
	names := map[string]string{}
	for _, f := range backend {
		if f.MessageType == "" {
			names[f.EnvVar] = f.Name
		}
	}
	issuer, jwks := names[configJWTIssuerEnvVar], names[configJWTJWKSURLEnvVar]
	if issuer == "" || jwks == "" {
		return body
	}
	// Already patched, or the author has taken these fields over by hand.
	// Either way this is not forge's to write again: a split-issuer dev setup
	// (a token-exchange gateway, an IdP migration) is legitimate, and doctor
	// reports the parity it cannot judge.
	if strings.Contains(body, backendJWTOverrideMarker) || strings.Contains(body, issuer) {
		return body
	}

	open := appConfigOpenLine(body)
	if open < 0 {
		// No single shared instance to insert into. A per-binary project has
		// no `app_config`, and guessing which binary validates tokens would
		// be wrong as often as right.
		return body
	}

	var ins strings.Builder
	ins.WriteString("\n")
	ins.WriteString("    " + backendJWTOverrideMarker + "\n")
	ins.WriteString("    # The oidc_* fields on the frontend instance further down say where the\n")
	ins.WriteString("    # BROWSER signs in. These say what the SERVER will accept, and both read\n")
	ins.WriteString("    # the ONE identity the idp-provision job published — so they cannot drift\n")
	ins.WriteString("    # into the failure where sign-in succeeds and every RPC still 401s.\n")
	ins.WriteString("    #\n")
	ins.WriteString("    # jwks_url is how the server fetches the issuer's public keys: read at\n")
	ins.WriteString("    # boot and refreshed in the background, so key rotation needs no\n")
	ins.WriteString("    # redeploy. Empty (before the job has ever converged) leaves the server\n")
	ins.WriteString("    # closed rather than half-open, which is the correct posture.\n")
	ins.WriteString("    #\n")
	ins.WriteString("    # Pointing dev at a DIFFERENT issuer means replacing these with your own\n")
	ins.WriteString("    # values; forge never rewrites them once they name something else.\n")
	fmt.Fprintf(&ins, "    %s = idp.idp_identity[%q]\n", issuer, "issuer")
	fmt.Fprintf(&ins, "    %s = idp.idp_identity[%q]\n", jwks, "jwks_url")
	// The audience is the project id the token is minted for. Bound only when
	// the project declares the field — it is the one of the three a validator
	// can legitimately do without.
	if aud := names[configJWTAudienceEnvVar]; aud != "" {
		fmt.Fprintf(&ins, "    %s = idp.idp_identity[%q]\n", aud, "audience")
	}

	return body[:open] + ins.String() + body[open:]
}

// appConfigOpenLine returns the byte offset just after the line that opens
// the shared `app_config` block, or -1 when there is none.
//
// Both the typed binding the scaffolder writes
// (`app_config: config_gen.AppConfig = {`) and a bare `app_config = {` count:
// the insertion is valid in either, and a project that dropped the annotation
// still has exactly one instance to point at.
func appConfigOpenLine(src string) int {
	offset := 0
	for _, line := range strings.SplitAfter(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") && strings.HasSuffix(trimmed, "{") {
			if name, _, ok := strings.Cut(trimmed, "="); ok {
				name, _, _ = strings.Cut(name, ":")
				if strings.TrimSpace(name) == "app_config" {
					return offset + len(line)
				}
			}
		}
		offset += len(line)
	}
	return -1
}

// frontendConfigInstanceDeclared reports whether config.k already binds a
// variable to the given schema. Matched on the SCHEMA (the qualified type
// reference), not the variable name, because the variable name is the
// author's choice — renaming it must not make forge append a duplicate.
func frontendConfigInstanceDeclared(src, schemaName string) bool {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		typeRef := strings.TrimSpace(strings.Split(strings.Split(strings.TrimSpace(rest), "=")[0], " ")[0])
		if dot := strings.LastIndex(typeRef, "."); dot >= 0 && typeRef[dot+1:] == schemaName {
			return true
		}
	}
	return false
}

// renderFrontendConfigInstance emits one frontend's typed VALUES instance.
//
// It is SPARSE on the same rule as the backend's per-env scaffold: only a
// field KCL would otherwise reject the instance for — required with no
// schema default — is pinned, and everything else inherits the schema
// default projected from the proto. An env that wants to differ says so by
// editing this block, which is the one place a per-env frontend value is
// authored.
func renderFrontendConfigInstance(fc FrontendConfig, schemaName, envName, projectName string, devIdentity bool) string {
	identity := map[string]bool{}
	if devIdentity {
		identity = devIdentityFieldNames(fc)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n%s: %s.%s = {\n", naming.ToSnakeCase(fc.MessageName), FrontendConfigModule, schemaName)
	for _, f := range fc.Fields {
		if f.MessageType != "" {
			continue
		}
		switch {
		case identity[f.Name]:
			// Pinned below, in one block, because the three identity values
			// are one fact: an issuer, a callback into it, and the id it
			// issued for that pair.
			continue
		case f.Role == configModeRole && envName == configDevEnvName:
			// The mode/environment knob is the one value a dev env must
			// not inherit from a proto default authored for production —
			// the exact divergence that made a dev bundle announce itself
			// as "production".
			fmt.Fprintf(&b, "    %s = %q\n", f.Name, configDevModeValue)
		case isKCLMandatory(f):
			fmt.Fprintf(&b, "    %s = %s\n", f.Name, kclConfigZeroLiteral(f))
		}
	}
	if len(identity) > 0 {
		b.WriteString(renderDevIdentityFields(fc))
	}
	b.WriteString("}\n")
	return b.String()
}

// The dev-identity fields, by their proto names. Selected by env_var — the
// same rule the projection keys on — so renaming a proto field does not
// silently drop it out of the identity block.
const (
	oidcIssuerEnvVar      = "OIDC_ISSUER"
	oidcClientIDEnvVar    = "OIDC_CLIENT_ID"
	oidcRedirectURIEnvVar = "OIDC_REDIRECT_URI"
)

// IDPIdentityModule is the committed KCL module the dev-IdP convergence job
// (`auth idp-provision`, deploy/kcl/workloads.k's idp-provision workload)
// publishes into on a compose/dev target — the compose analogue of the
// ConfigMap it writes on a cluster target. See pkg/devidp.KCLFilePublisher.
const IDPIdentityModule = "idp_identity_gen"

// idpIdentityImport is the KCL import that makes the published identity
// available. Imported in the USER's file (config.k), never inside a
// forge-owned module, so a plugin-less/job-less `kcl run` of the forge
// module tree still renders — the same rule resolve_port's plugin import
// followed.
const idpIdentityImport = "import ." + IDPIdentityModule + " as idp"

// idpIdentityLookup is the marker that decides whether the import above is
// needed. One spelling, so the two cannot drift into a file that reads the
// published identity without importing it.
const idpIdentityLookup = "idp.idp_identity["

// devIdentityPreamble introduces the import and says where these values
// come from, in the file a developer opens to change them.
const devIdentityPreamble = `
# ── Dev identity ──────────────────────────────────────────────────────
# The dev IdP is DECLARED infrastructure: docker-compose.yml runs it and
# idp-steps.yaml describes the instance, the org, the admin user and the
# service account it converges to on first boot.
#
# TWO values in that setup cannot be declared up front — an OIDC client_id
# and the project id (token audience) are GENERATED by the issuer when the
# application is registered. Nothing here resolves them at RENDER time
# (there is no plugin call): a deploy-time job, ` + IDPProvisionWorkloadName + `
# in deploy/kcl/workloads.k, converges the registration and PUBLISHES what
# it generated into this committed file. This config.k just reads it, the
# same way it reads any other declared fact.
` + idpIdentityImport + "\n"

// devIdentityFieldNames returns the identity fields this frontend's config
// message actually declares, keyed by proto field name. Empty when the
// message declares none — a hand-written config with no OIDC fields gets no
// identity block rather than a reference to fields it does not have.
func devIdentityFieldNames(fc FrontendConfig) map[string]bool {
	wanted := map[string]string{
		oidcIssuerEnvVar:      "",
		oidcClientIDEnvVar:    "",
		oidcRedirectURIEnvVar: "",
	}
	for _, f := range fc.Fields {
		if f.MessageType != "" {
			continue
		}
		if _, ok := wanted[f.EnvVar]; ok {
			wanted[f.EnvVar] = f.Name
		}
	}
	// All three or none. The client id is meaningless without the issuer it
	// was minted by, and the redirect URI is what the id is registered
	// AGAINST — a partial block would declare a sign-in that cannot complete.
	out := map[string]bool{}
	for _, name := range wanted {
		if name == "" {
			return map[string]bool{}
		}
		out[name] = true
	}
	return out
}

// renderDevIdentityFields emits the three identity values as ONE derivation
// against the dev IdP.
//
// They are not independent, which is why they are written as a block rather
// than three settings someone could move separately:
//
//   - the client id is the one THIS issuer generated for THIS application,
//     so it is resolved against the issuer named two lines above it;
//   - the redirect URI is what the issuer will accept the callback at, and
//     the value forge registers is a glob (devOIDCRedirectGlob) so the
//     frontend's dev port stays kernel-assigned.
//
// The redirect URI field is scaffolded EMPTY, and that is the whole
// un-pinning. Empty is not "unconfigured" here: oidc-provider.ts falls back
// to `${window.location.origin}/auth/callback`, so the browser names the port
// it is actually serving on at the moment it starts the flow — the one value
// nothing can know before launch. Pinning a literal instead would force the
// frontend onto a fixed port and stop two dev stacks from signing in at once.
// A deployed environment DOES pin one, because there the origin is a fact
// about the deployment rather than about whichever port was free.
func renderDevIdentityFields(fc FrontendConfig) string {
	names := map[string]string{}
	for _, f := range fc.Fields {
		if f.MessageType == "" {
			names[f.EnvVar] = f.Name
		}
	}

	var b strings.Builder
	b.WriteString("\n    # Published by the idp-provision job into ")
	b.WriteString(IDPIdentityModule)
	b.WriteString(".k (see deploy/kcl/workloads.k) —\n")
	b.WriteString("    # this config.k just READS it, the same way it reads any other declared\n")
	b.WriteString("    # fact. Before that job has ever run, the committed stub it write-if-\n")
	b.WriteString("    # absent scaffolds carries empty strings for all four keys, so a fresh\n")
	b.WriteString("    # clone renders a complete configuration offline.\n")
	fmt.Fprintf(&b, "    %s = idp.idp_identity[%q]\n", names[oidcIssuerEnvVar], "issuer")
	b.WriteString("\n")
	b.WriteString("    # EMPTY ON PURPOSE — the browser fills it in. The app falls back to\n")
	b.WriteString("    # `<its own origin>/auth/callback`, which is the only way to name a\n")
	b.WriteString("    # dev port that the kernel assigns at launch. The idp-provision job\n")
	b.WriteString("    # registers the matching PATTERN with the dev IdP (origin glob + this\n")
	b.WriteString("    # frontend's base_path), so any port is accepted and two dev stacks\n")
	b.WriteString("    # can sign in at the same time. Set a literal here only if this\n")
	b.WriteString("    # frontend serves somewhere fixed.\n")
	fmt.Fprintf(&b, "    %s = \"\"\n", names[oidcRedirectURIEnvVar])
	b.WriteString("\n")
	b.WriteString("    # With no IdP ever converged, this is EMPTY, and the render still\n")
	b.WriteString("    # succeeds. Empty selects the frontend's no-auth posture — closed, and\n")
	b.WriteString("    # working offline — whereas a wrong client id produces a sign-in that\n")
	b.WriteString("    # fails at the issuer.\n")
	fmt.Fprintf(&b, "    %s = idp.idp_identity[%q]\n", names[oidcClientIDEnvVar], "client_id")
	return b.String()
}

// KCLFrontendConfigName maps a frontend-bound config MESSAGE name to the
// KCL schema and projection lambda it generates: `WebConfig` -> schema
// `WebConfig`, lambda `webConfigRuntime`. The `Runtime` suffix (rather than
// the backend's `EnvMap`) says what the projection produces: the runtime
// config document the bundle reads, not a set of env vars.
func KCLFrontendConfigName(messageName string) (schema, lambda string) {
	if messageName == "" {
		return "FrontendConfig", "frontendConfigRuntime"
	}
	lower := strings.ToLower(messageName[:1]) + messageName[1:]
	return messageName, lower + "Runtime"
}

// GenerateFrontendConfigKCL emits the frontend config module: one typed
// schema + one runtime projection lambda per frontend.
//
// The projection returns a plain dict — the exact JSON object the browser
// receives as window.__FORGE_CONFIG__ — keyed by the field's env_var name
// where it declares one, falling back to the proto field name. Using the
// env_var as the key keeps ONE spelling of each fact across the proto, the
// KCL and the generated TypeScript, so a rename cannot half-land.
//
// Sensitive fields never appear here: ValidateFrontendConfigs refuses them
// before generation reaches this point, which is why (unlike the backend
// schema) there is no ConfigSecretRef in the emitted module.
func GenerateFrontendConfigKCL(configs []FrontendConfig, projectName string) (string, error) {
	var b strings.Builder

	b.WriteString("# Code generated by forge. DO NOT EDIT.\n")
	b.WriteString("# Typed frontend config schemas + runtime projections, projected from\n")
	b.WriteString("# proto/config/v1/config.proto — author per-env values in\n")
	b.WriteString("# deploy/kcl/<env>/config.k as a `<Frontend>Config { ... }` instance; add or\n")
	b.WriteString("# change fields in the PROTO, never here.\n")
	b.WriteString("#\n")
	b.WriteString("# EVERYTHING IN THIS FILE IS PUBLIC. These values are served to a browser\n")
	b.WriteString("# as /config.js and are readable by anyone who opens devtools. A field\n")
	b.WriteString("# marked `sensitive` in the proto is refused at generate time rather than\n")
	b.WriteString("# projected here — see ValidateFrontendConfigs.\n\n")

	for _, fc := range configs {
		schemaName, lambdaName := KCLFrontendConfigName(fc.MessageName)

		schema, err := renderFrontendConfigSchema(fc, projectName, schemaName)
		if err != nil {
			return "", fmt.Errorf("render frontend config schema for %s: %w", fc.Frontend, err)
		}
		b.WriteString(schema)
		b.WriteString("\n")
		b.WriteString(renderFrontendRuntimeProjection(fc, schemaName, lambdaName))
		b.WriteString("\n")
	}

	return b.String(), nil
}

// renderFrontendConfigSchema emits one frontend's typed KCL schema. Every
// field is a plain typed value with its proto default — there is no secret
// variant, by construction.
func renderFrontendConfigSchema(fc FrontendConfig, projectName, schemaName string) (string, error) {
	// Same refusal as the backend schema: a repeated field name in one KCL
	// suite is silently last-wins, and a frontend schema is no less exposed
	// to it than AppConfig is.
	if err := CheckDuplicateConfigFields(fc.Fields, FrontendConfigModule, schemaName); err != nil {
		return "", err
	}

	var b strings.Builder

	fmt.Fprintf(&b, "schema %s:\n", schemaName)
	fmt.Fprintf(&b, "    \"\"\"Public runtime config for frontend %q (project %s).\n\n", fc.Frontend, projectName)
	b.WriteString("    Served to the browser — every value here is public.\n")
	b.WriteString("    \"\"\"\n")

	emitted := 0
	var body strings.Builder
	for _, f := range fc.Fields {
		if f.MessageType != "" {
			continue // block references are flattened before this point
		}
		if d := strings.TrimSpace(f.Description); d != "" {
			d = strings.Join(strings.Fields(d), " ")
			fmt.Fprintf(&body, "    # %s\n", d)
		}
		kt := kclTypeForProtoConfig(f)
		// Defaults follow the SAME rules as the backend schema
		// (kclConfigDefaultLiteral): a proto default is emitted literally, a
		// required field with none is left undefaulted so KCL makes it
		// mandatory, and anything else takes its type-zero so an author can
		// instantiate the schema without spelling every field.
		if lit, ok := kclConfigDefaultLiteral(f); ok {
			fmt.Fprintf(&body, "    %s: %s = %s\n", f.Name, kt, lit)
		} else {
			fmt.Fprintf(&body, "    %s: %s\n", f.Name, kt)
		}
		emitted++
	}

	if emitted == 0 {
		b.WriteString("    # No (forge.v1.config)-annotated fields on this frontend's config message.\n")
		return b.String(), nil
	}
	b.WriteString(body.String())
	return b.String(), nil
}

// renderFrontendRuntimeProjection emits the lambda turning a schema
// instance into the runtime config dict the browser receives.
func renderFrontendRuntimeProjection(fc FrontendConfig, schemaName, lambdaName string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Runtime config document for frontend %q. The consumer serves this as\n", fc.Frontend)
	b.WriteString("# JSON inside /config.js, which the bundle reads at boot.\n")
	fmt.Fprintf(&b, "%s = lambda c: %s -> any {\n", lambdaName, schemaName)
	b.WriteString("    {\n")

	for _, f := range fc.Fields {
		if f.MessageType != "" {
			continue
		}
		key := f.EnvVar
		if key == "" {
			key = f.Name
		}
		fmt.Fprintf(&b, "        %q = c.%s\n", key, f.Name)
	}

	b.WriteString("    }\n")
	b.WriteString("}\n")
	return b.String()
}
