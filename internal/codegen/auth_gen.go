package codegen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/templates"
)

// AuthSetupTemplateData is the data shape for the owned internal/app/auth.go
// scaffold (app-auth.go.tmpl).
type AuthSetupTemplateData struct {
	Module string
	// Graph renders the composition-graph form: APIKeyValidator takes the
	// serving pool (*DB), not the legacy *Infra.
	Graph       bool
	HasDatabase bool
	// AuthConfigType is the pkg/config type SetupAuth reads its jwt_*
	// settings from — the config that declares them (ConfigDeclaring): ""
	// is the root config, Config.
	AuthConfigType string
	// NoJWTConfig is true when no config the binary loads declares them:
	// SetupAuth then builds no validator, and a root that reaches the user
	// authenticator refuses to start until one is written.
	NoJWTConfig bool
	// UserInfoLocalHTTPField is true when the config SetupAuth reads declares
	// jwt_userinfo_allow_local_http, the dev-only opt-in for a plain-HTTP
	// issuer at this machine's LAN address. A config that predates it gets a
	// SetupAuth that does not read it, and still compiles.
	UserInfoLocalHTTPField bool
}

// userInfoLocalHTTPConfigField is the Go name of that opt-in.
const userInfoLocalHTTPConfigField = "JwtUserinfoAllowLocalHttp"

// authDeclaresUserInfoLocalHTTP reports whether the config SetupAuth reads
// (configType) also declares the opt-in.
func authDeclaresUserInfoLocalHTTP(projectDir, configType string) bool {
	in, ok := configDeclaring(projectDir, append(append([]string{}, jwtConfigFields...), userInfoLocalHTTPConfigField))
	return ok && in == configType
}

// GenerateAuthSetup scaffolds internal/app/auth.go ONCE — the OWNED,
// user-editable SetupAuth() that picks the request authenticator in CODE
// (default: a JWT validator built from the typed config's jwt_* fields). It
// replaces the retired forge-owned auth_gen.go validator codegen:
// authentication is a code-wiring choice (which validator) reading
// per-deployment values that are ordinary config fields, not a static
// forge.yaml block.
//
// Scaffold-once, never overwritten after first emit (os.Stat guard) — the
// user owns the bytes. The generated cmd serve wiring calls app.SetupAuth
// and threads the returned validator into the auth interceptor.
func GenerateAuthSetup(modulePath, projectDir string) error {
	return GenerateAuthSetupData(projectDir, AuthSetupTemplateData{Module: modulePath, UserInfoLocalHTTPField: authDeclaresUserInfoLocalHTTP(projectDir, "")})
}

// GenerateAuthSetupData is GenerateAuthSetup with the whole template input:
// a project in graph mode passes Graph and HasDatabase.
func GenerateAuthSetupData(projectDir string, data AuthSetupTemplateData) error {
	appDir := filepath.Join(projectDir, "internal", "app")
	path := filepath.Join(appDir, "auth.go")
	rel := filepath.Join("internal", "app", "auth.go")
	if !checksums.ScaffoldOnceDecision(projectDir, rel) {
		// Scaffold-once, user-owned — never clobber an existing copy, and
		// never resurrect a deleted one. A project that wires auth its own
		// way (or has no auth surface at all) deletes this file and it
		// stays gone.
		return nil
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return fmt.Errorf("create internal/app dir: %w", err)
	}
	content, err := templates.ProjectTemplates().Render("app-auth.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render app-auth.go.tmpl: %w", err)
	}
	if err := writeUserScaffold(path, content); err != nil {
		return err
	}
	checksums.RecordScaffold(projectDir, rel)
	return nil
}
