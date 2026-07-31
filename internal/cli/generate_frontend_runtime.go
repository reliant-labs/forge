package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/templates"
)

// ensureViteQueryResourceHook writes src/hooks/use-query-resource.ts into a
// Vite SPA frontend when it is absent. The hook (the tristate adapter +
// useDebouncedValue) is the app-side adapter for the runtime's <Resource>
// container: it maps a React Query result onto the { status, data, error }
// shape <Resource> consumes. It ships in the shared static scaffold tree for
// new projects, so this only backfills a frontend that predates it.
// Emit-if-missing (never overwrite) so a hand-edited copy survives.
func ensureViteQueryResourceHook(projectDir, feDir string) error {
	destRel := filepath.Join(feDir, "src", "hooks", "use-query-resource.ts")
	dest := filepath.Join(projectDir, destRel)
	if _, err := os.Stat(dest); err == nil {
		return nil // present — leave it alone
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", destRel, err)
	}
	content, err := templates.FrontendTemplates().Get("shared/src/hooks/use-query-resource.ts")
	if err != nil {
		return fmt.Errorf("read use-query-resource: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", destRel, err)
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", destRel, err)
	}
	return nil
}
