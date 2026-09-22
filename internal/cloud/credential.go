// Package cloud resolves the two things forge needs to talk to a hosted
// control plane, and keeps them apart on purpose:
//
//   - the ENDPOINT, which is a per-environment fact declared in that
//     environment's KCL (forge.ControlPlane) and lives in git.
//   - the CREDENTIAL, which is an opaque bearer string that must never
//     live in git, resolved at call time from a flag, an environment
//     variable, or the login file `forge login` writes.
//
// This split mirrors internal/secrets: KCL emits the DECLARATION, Go
// resolves the VALUE. It is also why there is no `forge context use`.
// A stateful "current endpoint" would make the answer to "which server
// did that command hit" depend on machine-local state that never appears
// in a diff; deriving it from the env argument makes the answer a
// property of the repository instead.
//
// Nothing here imports control-plane or reliant, and nothing may. The
// endpoint is an arbitrary URL and the credential an opaque string — the
// same relationship forge's External deploy target has with flyctl, which
// needs FLY_API_TOKEN without forge linking Fly's SDK.
//
// forge:exclude-contract
// cloud is a credential/endpoint resolution utility, not a
// contract-shaped service. Opt out of the require-contract rule.
package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTokenEnv is the environment variable forge reads a control-plane
// credential from when an environment's ControlPlane declaration does not
// name a different one.
const DefaultTokenEnv = "FORGE_CONTROL_PLANE_TOKEN"

// CredentialSource says where a resolved credential came from. Commands
// print it so "which credential did that use" never requires guessing —
// the single most common confusion when a CI run and a laptop disagree.
type CredentialSource string

const (
	SourceFlag  CredentialSource = "flag"       // --token
	SourceEnv   CredentialSource = "env"        // the declared token_env
	SourceLogin CredentialSource = "login file" // written by `forge login`
)

// Credential is a resolved bearer token plus where it came from.
type Credential struct {
	Token  string
	Source CredentialSource
	// From is the human-readable origin: the flag name, the env var name,
	// or the login file path. Safe to print; never the token itself.
	From string
}

// ErrNoCredential is returned when no credential could be resolved. Use
// errors.Is to detect it; the message is already actionable (it names
// `forge login` and the environment variable), so callers should surface
// it as-is rather than wrapping it in a generic auth failure.
var ErrNoCredential = errors.New("no control-plane credential")

// ResolveCredential returns the bearer credential for an endpoint, in
// this precedence order:
//
//  1. flagToken      — an explicit --token on the command line
//  2. os.Getenv(tokenEnv) — the env var the environment DECLARED
//  3. the login file — what `forge login` stored
//
// WHY THIS ORDER. It runs most-explicit to most-ambient. A flag is typed
// for one command and can mean nothing else, so it must win — that is
// what makes a one-off override possible at all, and what makes a bug
// report reproducible ("run it with --token X"). The env var comes next
// because CI is where it is used: a pipeline has no browser and no
// persistent home directory, so the variable IS its credential, and it
// must beat any file that happens to exist in a cached runner image. The
// login file is last because it is the most ambient of the three — it
// was written days ago by a human and is shared by every project on the
// machine, so it is the right DEFAULT and the wrong override.
//
// The inverse order fails in a specific and nasty way: a stale login file
// would silently shadow the credential CI just injected, and the run
// would authenticate as the wrong principal rather than fail.
//
// tokenEnv empty falls back to DefaultTokenEnv, so a caller that has no
// declaration in hand still resolves the conventional variable.
func ResolveCredential(flagToken, tokenEnv string) (Credential, error) {
	if t := strings.TrimSpace(flagToken); t != "" {
		return Credential{Token: t, Source: SourceFlag, From: "--token"}, nil
	}
	if tokenEnv == "" {
		tokenEnv = DefaultTokenEnv
	}
	if t := strings.TrimSpace(os.Getenv(tokenEnv)); t != "" {
		return Credential{Token: t, Source: SourceEnv, From: tokenEnv}, nil
	}

	path, err := LoginFilePath()
	if err == nil {
		stored, readErr := ReadLogin(path)
		// A login file that exists but is unreadable or corrupt is an
		// ERROR, not a silent fall-through to "not logged in": the user
		// did log in, and telling them they did not would send them
		// round a loop that cannot fix it.
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return Credential{}, fmt.Errorf("read login file %s: %w", path, readErr)
		}
		if readErr == nil && strings.TrimSpace(stored.Token) != "" {
			return Credential{
				Token:  strings.TrimSpace(stored.Token),
				Source: SourceLogin,
				From:   path,
			}, nil
		}
	}

	return Credential{}, fmt.Errorf(
		"%w\nforge needs a bearer credential to reach the hosted control plane.\n"+
			"fix, in the order forge checks them:\n"+
			"    --token <token>        explicit, wins over everything (one-off / debugging)\n"+
			"    export %s=<token>      for CI — a pipeline has no browser\n"+
			"    forge login            for a human — opens a browser and stores the credential",
		ErrNoCredential, tokenEnv)
}

// StoredLogin is what `forge login` writes: an opaque bearer token and
// non-sensitive context about who and where it is for.
//
// The token is stored opaquely and deliberately unparsed. forge does not
// know or care whether it is a JWT, an API key, or a machine token with a
// vendor prefix — decoding it would couple forge to one issuer's format
// and break the moment that format changed.
type StoredLogin struct {
	Token    string `json:"token"`
	Endpoint string `json:"endpoint,omitempty"`
	Account  string `json:"account,omitempty"`
	// ExpiresAt is RFC3339 when the issuer supplied one. Informational:
	// forge does not refuse to send an apparently-expired credential,
	// because only the server can actually decide, and a clock-skewed
	// local refusal is worse than a clean 401.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// LoginFilePath is where `forge login` stores the credential:
// $FORGE_HOME/login.json, else ~/.forge/login.json.
//
// FORGE_HOME is honoured so a test (and a sandboxed CI job) can redirect
// the file without touching a real developer's credentials.
func LoginFilePath() (string, error) {
	if home := strings.TrimSpace(os.Getenv("FORGE_HOME")); home != "" {
		return filepath.Join(home, "login.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".forge", "login.json"), nil
}

// ReadLogin loads the stored login. A missing file returns an error
// satisfying errors.Is(err, os.ErrNotExist) so callers can distinguish
// "never logged in" from "the file is broken".
func ReadLogin(path string) (StoredLogin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return StoredLogin{}, err
	}
	var s StoredLogin
	if err := json.Unmarshal(raw, &s); err != nil {
		return StoredLogin{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// WriteLogin stores the credential at path, 0600, with the containing
// directory 0700. Both modes matter: this file is a bearer credential,
// and a world-readable one is a credential leak on any shared machine.
func WriteLogin(path string, s StoredLogin) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
