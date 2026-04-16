package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateWebhookFiles generates all files for a webhook endpoint within a service:
//   - handlers/<service>/webhook_<name>.go       (handler + signature verification)
//   - handlers/<service>/webhook_<name>_test.go   (tests)
//   - handlers/<service>/webhook_store.go         (idempotency store, only if not present)
func GenerateWebhookFiles(root, modulePath, serviceName, webhookName string) error {
	svcDir := filepath.Join(root, "handlers", serviceName)

	// Ensure the service directory exists.
	if _, err := os.Stat(svcDir); os.IsNotExist(err) {
		return fmt.Errorf("service directory %s does not exist", svcDir)
	}

	data := templates.WebhookTemplateData{
		Name:        webhookName,
		ServiceName: serviceName,
		Module:      modulePath,
	}

	// -- webhook handler --
	handlerContent, err := templates.RenderWebhookTemplate("webhook/webhooks.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render webhook handler: %w", err)
	}
	handlerPath := filepath.Join(svcDir, fmt.Sprintf("webhook_%s.go", webhookName))
	if err := os.WriteFile(handlerPath, handlerContent, 0644); err != nil {
		return fmt.Errorf("write webhook handler: %w", err)
	}

	// -- webhook test --
	testContent, err := templates.RenderWebhookTemplate("webhook/webhooks_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render webhook test: %w", err)
	}
	testPath := filepath.Join(svcDir, fmt.Sprintf("webhook_%s_test.go", webhookName))
	if err := os.WriteFile(testPath, testContent, 0644); err != nil {
		return fmt.Errorf("write webhook test: %w", err)
	}

	// -- webhook store (only create if not already present) --
	storePath := filepath.Join(svcDir, "webhook_store.go")
	if _, err := os.Stat(storePath); os.IsNotExist(err) {
		storeContent, err := templates.RenderWebhookTemplate("webhook/webhooks_store.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render webhook store: %w", err)
		}
		if err := os.WriteFile(storePath, storeContent, 0644); err != nil {
			return fmt.Errorf("write webhook store: %w", err)
		}
	}

	return nil
}
