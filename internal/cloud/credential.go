// Package cloud resolves the two things forge needs to talk to a hosted
// control plane, and keeps them apart on purpose:
//
//   - the ENDPOINT, which is a per-environment fact declared in that
//     environment's KCL (forge.ControlPlane) and lives in git.
//   - the CREDENTIAL, which is an opaque bearer string that must never
//     live in git, resolved at call time from a flag, an environment
//     variable, or the shared credentials file `forge login` writes
//     (forge/pkg/credentials), keyed by THAT endpoint.
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
//forge:lint-disable-next-line forge-exclude-contract-outbound-io: Client (client.go) is an outbound HTTP adapter owned by the CLI command layer; converting it to a contract.go adapter is tracked as CONTRACTS follow-up F2
//forge:exclude-contract: CLI-internal credential/endpoint resolution plus the control-plane HTTP client the forge CLI builds per command
package cloud

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/credentials"
)

// DefaultTokenEnv is the environment variable forge reads a control-plane
// credential from when an environment's ControlPlane declaration does not
// name a different one.
const DefaultTokenEnv = "FORGE_CONTROL_PLANE_TOKEN"

// CredentialSource says where a resolved credential came from. Commands
// print it so "which credential did that use" never requires guessing —
// the single most common confusion when a CI run and a laptop disagree.
type CredentialSource string

// The three places a credential can come from, in the precedence order
// resolution tries them: an explicit flag beats the environment, and the
// environment beats the credentials file on disk. That order is what makes a
// CI run overridable and a laptop's stored login the fallback rather than a
// thing that silently wins.
const (
	SourceFlag  CredentialSource = "flag"             // --token
	SourceEnv   CredentialSource = "env"              // the declared token_env
	SourceLogin CredentialSource = "credentials file" // written by `forge login`
)

// Credential is a resolved bearer token plus where it came from.
type Credential struct {
	Token  string
	Source CredentialSource
	// From is the human-readable origin: the flag name, the env var name,
	// or the credentials file path. Safe to print; never the token itself.
	From string
}

// ErrNoCredential is returned when no credential could be resolved. Use
// errors.Is to detect it; the message is already actionable (it names
// `forge login` and the environment variable), so callers should surface
// it as-is rather than wrapping it in a generic auth failure.
var ErrNoCredential = errors.New("no control-plane credential")

// ResolveCredential returns the bearer credential for ep, in this
// precedence order:
//
//  1. flagToken             — an explicit --token on the command line
//  2. os.Getenv(ep.TokenEnv) — the env var the environment DECLARED
//  3. the credentials file  — what `forge login` stored FOR ep.URL
//
// WHY THIS ORDER. It runs most-explicit to most-ambient. A flag is typed
// for one command and can mean nothing else, so it must win — that is
// what makes a one-off override possible at all, and what makes a bug
// report reproducible ("run it with --token X"). The env var comes next
// because CI is where it is used: a pipeline has no browser and no
// persistent home directory, so the variable IS its credential, and it
// must beat any file that happens to exist in a cached runner image. The
// file is last because it is the most ambient of the three — it was
// written days ago by a human — so it is the right DEFAULT and the wrong
// override.
//
// The file lookup is keyed by the endpoint. A login to staging can never be
// presented to prod: a prod command finds no entry and says so, instead of
// sending staging's token to prod's server and failing with an
// indistinguishable 401.
func ResolveCredential(flagToken string, ep Endpoint) (Credential, error) {
	if t := strings.TrimSpace(flagToken); t != "" {
		return Credential{Token: t, Source: SourceFlag, From: "--token"}, nil
	}
	tokenEnv := ep.TokenEnv
	if tokenEnv == "" {
		tokenEnv = DefaultTokenEnv
	}
	if t := strings.TrimSpace(os.Getenv(tokenEnv)); t != "" {
		return Credential{Token: t, Source: SourceEnv, From: tokenEnv}, nil
	}

	path, err := CredentialsPath()
	if err != nil {
		return Credential{}, err
	}
	stored, err := credentials.Lookup(path, ep.URL, ClientID)
	switch {
	case err == nil:
		if stored.Expired(time.Now()) {
			return Credential{}, fmt.Errorf("%w\nthe login for %s stored in %s expired at %s\nfix: forge login %s",
				ErrNoCredential, ep.URL, path, stored.ExpiresAt.Format(time.RFC3339), loginHint(ep))
		}
		return Credential{Token: stored.Token, Source: SourceLogin, From: path}, nil
	case !errors.Is(err, credentials.ErrNotFound):
		// A file that exists but cannot be read is an ERROR, not "not
		// logged in": the user did log in, and telling them otherwise sends
		// them round a loop that cannot fix it.
		return Credential{}, err
	}

	return Credential{}, fmt.Errorf(
		"%w for %s\nforge needs a bearer credential to reach the hosted control plane.\n"+
			"fix, in the order forge checks them:\n"+
			"    --token <token>        explicit, wins over everything (one-off / debugging)\n"+
			"    export %s=<token>      for CI — a pipeline has no browser\n"+
			"    forge login %s        for a human — opens a browser and stores the credential",
		ErrNoCredential, ep.URL, tokenEnv, loginHint(ep))
}

// loginHint is the `forge login` argument that targets ep: the env name when
// the endpoint came from one, else --endpoint.
func loginHint(ep Endpoint) string {
	if ep.Env != "" {
		return ep.Env
	}
	return "--endpoint " + ep.URL
}

// CredentialsPath is where the shared credentials file lives, from
// $FORGE_HOME, $XDG_CONFIG_HOME and the home directory. forge/pkg/credentials
// owns the precedence; the environment is read here, at the application edge.
func CredentialsPath() (string, error) {
	home, _ := os.UserHomeDir()
	return credentials.Dirs{
		ForgeHome:     os.Getenv("FORGE_HOME"),
		XDGConfigHome: os.Getenv("XDG_CONFIG_HOME"),
		Home:          home,
	}.Path()
}
