package codegen

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/reliant-labs/forge/internal/config"
)

// devEnvMentionsIDP matches a dev env main.k that runs an identity provider
// some way: the scaffolded HostInfra (`engine = "zitadel"`), or a workload
// or binding naming the compose `idp` service.
var devEnvMentionsIDP = regexp.MustCompile(`zitadel|"idp"`)

// ProjectDeclaresIdentity answers "does this project have users to sign
// in?", read off the tree: deploy/kcl/identity.k, which `forge project new`
// writes for a project with a frontend and a backend unless --auth-mode none,
// is the declaration. Nothing else records the choice.
//
// It errs toward YES, because every consumer of a NO takes something away
// (the dev IdP from docker-compose.yml, the auth requirement from a born
// rpc): a project scaffolded before identity.k existed has a frontend and no
// identity.k, yet its dev env may still run the IdP, so a dev main.k that
// mentions one (or that cannot be read) also counts. An empty projectDir (no
// tree to read) counts too.
func ProjectDeclaresIdentity(projectDir string) bool {
	if projectDir == "" {
		return true
	}
	if _, err := os.Stat(filepath.Join(projectDir, filepath.FromSlash(IdentityKCLRelPath))); err == nil {
		return true
	}
	b, err := os.ReadFile(filepath.Join(projectDir, "deploy", "kcl", DevEnvName, "main.k"))
	if err != nil {
		return true
	}
	return devEnvMentionsIDP.Match(b)
}

// ProjectHasNoSignIn reports whether the project at projectDir is one nobody
// signs in to: people reach it through a browser (it has a frontend) and it
// declares no identity (ProjectDeclaresIdentity is false). That is the
// project `forge project new --auth-mode none` writes.
//
// In such a project nothing issues the credential a browser would present,
// so an rpc that requires one answers 401 to every caller its frontend has.
// `forge scaffold` therefore births its CRUD rpcs `auth_required: false`
// there, and `forge lint` warns about an rpc that still requires a caller.
//
// A project with no frontend is NOT one: its rpcs take a bearer token from
// an issuer it trusts, or an API key, so requiring a caller is still
// satisfiable. It errs toward NO, inheriting ProjectDeclaresIdentity's lean
// toward yes, because a NO keeps the fail-closed default.
func ProjectHasNoSignIn(projectDir string) bool {
	if ProjectDeclaresIdentity(projectDir) {
		return false
	}
	if len(config.DiscoverInRepoFrontends(projectDir)) > 0 {
		return true
	}
	_, ok := config.DiscoverRootFrontend(projectDir)
	return ok
}
