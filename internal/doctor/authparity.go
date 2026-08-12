package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// CheckAuthParity compares the issuer the BACKEND validates against with the
// issuer the FRONTEND signs in against.
//
// Both halves are now declared in ONE file — the environment's
// deploy/kcl/<env>/config.k, the backend's jwt_issuer on its AppConfig
// instance and the frontend's oidc_issuer on its frontend-config instance.
// That is a real narrowing (they used to live in a dotenv and a generated
// KCL fragment respectively), but they remain two declarations: nothing
// links them, and nothing notices when they disagree.
//
// The check retires when both halves compose ONE shared config message.
// Then there is a single declared fact projected to both, and there is
// nothing left to compare.
//
// The failure they produce is maximally confusing: sign-in SUCCEEDS (the
// browser talked to a real IdP and got a real token), and then every RPC
// answers 401 with a signature or issuer complaint. It reads like a broken
// token, so the search starts at the validator — which is correct, and is
// simply being shown a token from somewhere else.
//
// This check is deliberately a WARN and never a FAIL: split-issuer setups are
// legitimate (a token-exchange gateway, an IdP migration accepting two
// issuers via auth.Config.TokenValidators), and doctor cannot tell those from
// a typo. It reports what it read and lets a human judge.
func CheckAuthParity(_ context.Context, env *Environment) CheckResult {
	res := CheckResult{Name: "Auth issuer parity (backend ↔ frontend)"}

	backend := backendIssuer(env.ProjectDir, env.Env)
	frontend := frontendIssuers(env.ProjectDir, env.Env)

	// No identity configured at all is the scaffold's default state, and a
	// correct one: no key material means closed-and-bootable, not broken.
	if backend == "" && len(frontend) == 0 {
		res.Status = StatusSkip
		res.Message = "no OIDC issuer configured on either side — nothing to compare"
		return res
	}

	if backend == "" {
		res.Status = StatusWarn
		res.Message = "the frontend names an issuer but the backend validates against none — every authenticated RPC will reject the token the browser obtains"
		res.Evidence = "frontend: " + strings.Join(frontend, ", ")
		return res
	}
	if len(frontend) == 0 {
		res.Status = StatusWarn
		res.Message = "the backend validates against an issuer but no frontend names one — the browser has no way to obtain a token it will accept"
		res.Evidence = "backend: " + backend
		return res
	}

	var mismatched []string
	for _, f := range frontend {
		if !sameIssuer(f, backend) {
			mismatched = append(mismatched, f)
		}
	}
	if len(mismatched) > 0 {
		res.Status = StatusWarn
		res.Message = "backend and frontend name DIFFERENT issuers — sign-in will succeed and every RPC will still 401"
		res.Evidence = "backend: " + backend + "\nfrontend: " + strings.Join(mismatched, ", ") +
			"\n\nNote the two-hostname case: a containerised IdP is reached at localhost:<port> by the\n" +
			"browser and at <service>:<port> from inside the network. That split is EXPECTED for\n" +
			"jwt_jwks_url (in-network), but jwt_issuer must match the browser-facing URL the token\n" +
			"is minted under. See: forge skill load auth/dev-loop"
		return res
	}

	res.Status = StatusPass
	res.Message = "backend and frontend agree on the issuer"
	res.Evidence = backend
	return res
}

// backendIssuer reads the issuer the backend validates against from the
// environment's declared config.
//
// It is a line scan of deploy/kcl/<env>/config.k for `jwt_issuer`, matching
// the frontend half below rather than parsing KCL — doctor reports on
// projects that may not render cleanly, and a check that needs a working
// build to tell you what is broken goes quiet exactly when it is needed.
//
// An empty answer means "not declared here", not "not set": a deployed
// environment may legitimately bind its issuer out of band.
func backendIssuer(root, env string) string {
	if env == "" {
		env = "dev"
	}
	return readKCLStringValue(filepath.Join(root, "deploy", "kcl", env, "config.k"), "JWT_ISSUER")
}

// frontendIssuers reads the issuer each frontend signs in against from the
// environment's KCL config.
//
// It scans every .k file in deploy/kcl/<env>/ for OIDC_ISSUER rather than
// only config.k, because a project may split its declarations across files
// the env imports, and a check that reads one filename would go quiet on
// exactly the layouts most likely to have drifted.
//
// This check is NOT yet obsolete. Its whole purpose is catching drift
// between two independently-declared issuers, and although both now live in
// the same file, they are still two declarations that can disagree. It
// becomes structurally unnecessary only once both halves compose ONE shared
// config message, at which point there is a single fact and nothing to
// compare.
func frontendIssuers(root, env string) []string {
	if env == "" {
		env = "dev"
	}
	dir := filepath.Join(root, "deploy", "kcl", env)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".k") {
			continue
		}
		if v := readKCLStringValue(filepath.Join(dir, e.Name()), "OIDC_ISSUER"); v != "" {
			out = append(out, e.Name()+": "+v)
		}
	}
	return out
}

// readKCLStringValue returns the first string assigned to key in a KCL
// file, or "" when the file or key is absent.
//
// Deliberately a line scan rather than a KCL evaluation: doctor reports on
// a project that may not render cleanly, and a check that needs a working
// build to tell you what is broken is a check that goes quiet exactly when
// it is needed. It accepts both `"KEY" = "value"` (a dict entry) and
// `key = "value"` (a schema field) spellings.
func readKCLStringValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		if !strings.EqualFold(k, key) {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.TrimSuffix(v, ",")
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if v != "" {
			return v
		}
	}
	return ""
}

// sameIssuer compares two issuer URLs the way an OIDC implementation does
// modulo the one difference that is never meaningful: a trailing slash. The
// frontend entry carries a "<name>: " prefix, which is stripped first.
func sameIssuer(frontendEntry, backend string) bool {
	_, issuer, found := strings.Cut(frontendEntry, ": ")
	if !found {
		issuer = frontendEntry
	}
	return strings.TrimRight(issuer, "/") == strings.TrimRight(backend, "/")
}
