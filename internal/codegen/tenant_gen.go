package codegen

import (
	"fmt"
	"os"
	"path/filepath"

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
func GenerateTenantMiddleware(mt *config.MultiTenantConfig, targetDir string) error {
	middlewareDir := filepath.Join(targetDir, "pkg", "middleware")
	if err := os.MkdirAll(middlewareDir, 0755); err != nil {
		return fmt.Errorf("create middleware dir: %w", err)
	}

	data := TenantTemplateData{
		ClaimField: mt.EffectiveClaimField(),
		ColumnName: mt.EffectiveColumnName(),
	}

	content, err := templates.RenderMiddlewareTemplate("middleware/tenant_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render tenant_gen.go.tmpl: %w", err)
	}

	if err := os.WriteFile(filepath.Join(middlewareDir, "tenant_gen.go"), content, 0644); err != nil {
		return fmt.Errorf("write tenant_gen.go: %w", err)
	}

	return nil
}
