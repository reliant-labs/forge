package templates

import (
	"strings"
	"testing"
)

func TestRenderServiceTemplate_StripsBuildIgnoreFromRenderedOutput(t *testing.T) {
	content, err := RenderServiceTemplate("webhook/webhook_routes_gen.go.tmpl", map[string]any{
		"Package": "tasks",
		"Webhooks": []map[string]any{{"Name": "github", "PascalName": "Github"}},
	})
	if err != nil {
		t.Fatalf("RenderServiceTemplate() error = %v", err)
	}

	rendered := string(content)
	if strings.HasPrefix(rendered, "//go:build ignore") {
		t.Fatal("rendered template should not retain //go:build ignore header")
	}
	if !strings.Contains(rendered, "func (s *Service) RegisterWebhookRoutes") {
		t.Fatal("rendered template should include webhook route registration")
	}
}
