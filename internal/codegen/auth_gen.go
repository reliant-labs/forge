package codegen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// AuthTemplateData holds the data shape expected by the auth middleware templates.
type AuthTemplateData struct {
	Provider    string           // "jwt", "api_key", "both"
	JWT         config.JWTConfig
	APIKey      config.APIKeyConfig
	Module      string
	SkipMethods []string // procedure names that don't require auth
}

// GenerateAuthMiddleware renders the auth middleware templates and writes
// the generated files into pkg/middleware/ of the target project.
// It generates:
//   - pkg/middleware/auth_gen.go (always regenerated — DO NOT EDIT)
//   - pkg/middleware/auth_validator.go (only if API key auth is configured and file doesn't exist)
func GenerateAuthMiddleware(cfg *config.AuthConfig, modulePath string, skipMethods []string, targetDir string) error {
	middlewareDir := filepath.Join(targetDir, "pkg", "middleware")
	if err := os.MkdirAll(middlewareDir, 0755); err != nil {
		return fmt.Errorf("create middleware dir: %w", err)
	}

	data := AuthTemplateData{
		Provider:    cfg.Provider,
		JWT:         cfg.JWT,
		APIKey:      cfg.APIKey,
		Module:      modulePath,
		SkipMethods: skipMethods,
	}

	// Always regenerate auth_gen.go
	content, err := templates.MiddlewareTemplates.Render("auth_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render auth_gen.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(middlewareDir, "auth_gen.go"), content, 0644); err != nil {
		return fmt.Errorf("write auth_gen.go: %w", err)
	}

	// Generate auth_validator.go only if API key auth is used and the file doesn't exist.
	// This is a user-editable file — never overwrite it.
	if cfg.Provider == "api_key" || cfg.Provider == "both" {
		validatorPath := filepath.Join(middlewareDir, "auth_validator.go")
		if _, err := os.Stat(validatorPath); os.IsNotExist(err) {
			validatorContent, err := templates.MiddlewareTemplates.Render("auth_validator.go.tmpl", data)
			if err != nil {
				return fmt.Errorf("render auth_validator.go.tmpl: %w", err)
			}
			if err := os.WriteFile(validatorPath, validatorContent, 0644); err != nil {
				return fmt.Errorf("write auth_validator.go: %w", err)
			}
		}
	}

	return nil
}
