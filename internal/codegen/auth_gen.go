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
	content, err := templates.ProjectTemplates().Render("app-auth.go.tmpl", AuthSetupTemplateData{Module: modulePath})
	if err != nil {
		return fmt.Errorf("render app-auth.go.tmpl: %w", err)
	}
	if err := writeUserScaffold(path, content); err != nil {
		return err
	}
	checksums.RecordScaffold(projectDir, rel)
	return nil
}
