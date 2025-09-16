package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateFrontendFiles generates the frontend directory and files for a
// Next.js frontend. Both the "new" project flow and the "add frontend" flow
// delegate here so the output is always identical.
func GenerateFrontendFiles(root, modulePath, projectName, frontendName string, apiPort int) error {
	frontendDir := filepath.Join(root, "frontends", frontendName)
	if err := os.MkdirAll(frontendDir, 0755); err != nil {
		return fmt.Errorf("create frontend directory: %w", err)
	}

	frontendFiles, err := templates.ListFrontendTemplates("nextjs")
	if err != nil {
		return fmt.Errorf("list frontend templates: %w", err)
	}

	data := templates.FrontendTemplateData{
		FrontendName: frontendName,
		ProjectName:  projectName,
		ApiUrl:       fmt.Sprintf("http://localhost:%d", apiPort),
		ApiPort:      fmt.Sprintf("%d", apiPort),
		Module:       modulePath,
	}

	for _, file := range frontendFiles {
		content, err := templates.RenderFrontendTemplate(filepath.Join("nextjs", file), data)
		if err != nil {
			return fmt.Errorf("render frontend template %s: %w", file, err)
		}

		destFile := file
		if strings.HasSuffix(destFile, ".tmpl") {
			destFile = strings.TrimSuffix(destFile, ".tmpl")
		}

		destPath := filepath.Join(frontendDir, destFile)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write frontend file %s: %w", destFile, err)
		}
	}

	return nil
}
