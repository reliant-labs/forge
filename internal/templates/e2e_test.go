package templates

import (
	"strings"
	"testing"
)

func TestE2EWorkflowTemplate_DockerCompose(t *testing.T) {
	data := E2EWorkflowData{
		ProjectName:  "myapp",
		GoVersion:    "1.25",
		Runtime:      "docker-compose",
		HasFrontends: false,
	}
	content, err := RenderCITemplate("github", "e2e.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render e2e.yml.tmpl: %v", err)
	}
	s := string(content)

	for _, want := range []string{
		"name: E2E Tests",
		"docker compose -f docker-compose.yml up -d --wait",
		"go test -v -count=1 -timeout=20m ./e2e/...",
		"docker compose -f docker-compose.yml down -v",
		"go build -o ./bin/myapp ./cmd/...",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing in output: %q", want)
		}
	}
	if strings.Contains(s, "k3d") {
		t.Error("docker-compose runtime should not mention k3d")
	}
}

func TestE2EWorkflowTemplate_K3d(t *testing.T) {
	data := E2EWorkflowData{
		ProjectName:  "myapp",
		GoVersion:    "1.25",
		Runtime:      "k3d",
		HasFrontends: true,
	}
	content, err := RenderCITemplate("github", "e2e.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render e2e.yml.tmpl: %v", err)
	}
	s := string(content)

	for _, want := range []string{
		"k3d cluster delete e2e",
		"k3d image import",
		"kcl run deploy/kcl/dev/main.k",
		"frontends/*/Dockerfile",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing in output: %q", want)
		}
	}
	if strings.Contains(s, "docker compose") {
		t.Error("k3d runtime should not mention docker compose")
	}
}
