package codegen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// TenantTemplateData holds the data shape expected by the tenant middleware template.
type TenantTemplateData struct {
	ClaimField string // JWT claim to extract tenant ID from (e.g. "org_id")
	ColumnName string // DB column name for tenant scoping (e.g. "org_id")
}

// GenerateTenantMiddleware renders the tenant middleware template and writes
// the generated file into pkg/middleware/ of the target project.
// It generates:
//   - pkg/middleware/tenant_gen.go (always regenerated — DO NOT EDIT)
//
// cs is the project's checksum tracker; passing it keeps the generated
// file out of `forge audit`'s orphan list. A nil cs is tolerated.
func GenerateTenantMiddleware(mt *config.MultiTenantConfig, targetDir string, cs *checksums.FileChecksums) error {
	middlewareDir := filepath.Join(targetDir, "pkg", "middleware")
	if err := os.MkdirAll(middlewareDir, 0755); err != nil {
		return fmt.Errorf("create middleware dir: %w", err)
	}

	// Tolerate a nil MultiTenantConfig — tenant_gen.go is generated
	// unconditionally so that pkg/app/testing.go's middleware.ContextWithTenantID
	// reference resolves even when multi-tenant is disabled. Defaults
	// ("org_id"/"org_id") match what EffectiveClaimField/Column produce.
	cfg := mt
	if cfg == nil {
		cfg = &config.MultiTenantConfig{}
	}
	data := TenantTemplateData{
		ClaimField: cfg.EffectiveClaimField(),
		ColumnName: cfg.EffectiveColumnName(),
	}

	content, err := templates.MiddlewareTemplates().Render("tenant_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render tenant_gen.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(targetDir, filepath.Join("pkg", "middleware", "tenant_gen.go"), content, cs, true); err != nil {
		return fmt.Errorf("write tenant_gen.go: %w", err)
	}

	return nil
}
