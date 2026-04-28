package templates

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCIWorkflowTemplate_AllFeatures(t *testing.T) {
	data := CIWorkflowData{
		ProjectName:  "myapp",
		GoVersion:    "1.26",
		HasFrontends: true,
		Frontends: []FrontendCIConfig{
			{Name: "web", Path: "frontends/web"},
			{Name: "admin", Path: "frontends/admin"},
		},
		HasServices:         true,
		LintGolangci:        true,
		LintBuf:             true,
		LintBufBreaking:     true,
		LintFrontend:        true,
		LintFrontendStyles:  true,
		LintMigrationSafety: true,
		TestRace:            true,
		TestCoverage:        true,
		VulnGo:              true,
		VulnDocker:          true,
		VulnNPM:             true,
		E2EEnabled:          true,
		E2ERuntime:          "docker-compose",
		PermContents:        "read",
		HasKCL:              true,
		Environments:        []string{"dev", "staging", "prod"},
	}

	content, err := CITemplates("github").Render("ci.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("invalid YAML:\n%s\nerror: %v", string(content), err)
	}

	jobs, ok := parsed["jobs"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'jobs' key")
	}

	for _, expected := range []string{"lint", "test", "build", "verify-generated", "validate-kcl", "vuln-scan", "docker-build", "e2e"} {
		if _, ok := jobs[expected]; !ok {
			t.Errorf("missing job %q", expected)
		}
	}
}

func TestProtoBreakingWorkflowTemplate(t *testing.T) {
	data := CIWorkflowData{PermContents: "read"}
	content, err := CITemplates("github").Render("proto-breaking.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("invalid YAML:\n%s\nerror: %v", string(content), err)
	}
	if parsed["name"] != "Proto Breaking Change Detection" {
		t.Fatalf("unexpected workflow name: %#v", parsed["name"])
	}
}

func TestCIWorkflowTemplate_Minimal(t *testing.T) {
	data := CIWorkflowData{
		ProjectName:  "minimal",
		GoVersion:    "1.26",
		LintGolangci: true,
		TestRace:     true,
		PermContents: "read",
	}

	content, err := CITemplates("github").Render("ci.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("invalid YAML:\n%s\nerror: %v", string(content), err)
	}

	jobs, ok := parsed["jobs"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'jobs' key")
	}

	// Should NOT have optional jobs
	for _, absent := range []string{"validate-kcl", "vuln-scan", "e2e"} {
		if _, ok := jobs[absent]; ok {
			t.Errorf("job %q should not be present in minimal config", absent)
		}
	}

	// Should have core jobs
	for _, expected := range []string{"lint", "test", "build", "verify-generated", "docker-build"} {
		if _, ok := jobs[expected]; !ok {
			t.Errorf("missing job %q", expected)
		}
	}
}

func TestCIWorkflowTemplate_K3dE2E(t *testing.T) {
	data := CIWorkflowData{
		ProjectName:  "myapp",
		GoVersion:    "1.26",
		LintGolangci: true,
		TestRace:     true,
		PermContents: "read",
		E2EEnabled:   true,
		E2ERuntime:   "k3d",
	}

	content, err := CITemplates("github").Render("ci.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("invalid YAML:\n%s\nerror: %v", string(content), err)
	}

	jobs := parsed["jobs"].(map[string]interface{})
	if _, ok := jobs["e2e"]; !ok {
		t.Error("missing e2e job for k3d runtime")
	}
}
