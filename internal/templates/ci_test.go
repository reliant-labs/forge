package templates

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCIWorkflowTemplate_AllFeatures(t *testing.T) {
	data := CIWorkflowData{
		ProjectName:  "myapp",
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
		HasDocker:           true,
		VerifyGenerated:     true,
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

	for _, expected := range []string{"lint", "test", "build", "verify-generated", "deployability", "vuln-scan", "docker-build", "e2e"} {
		if _, ok := jobs[expected]; !ok {
			t.Errorf("missing job %q", expected)
		}
	}

	// Every job that runs `forge …` must install it first. A job that
	// shells a binary it never installed fails on "command not found" —
	// which is how the KCL validation and vuln-scan jobs shipped.
	for name, raw := range jobs {
		job, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		steps, _ := job["steps"].([]interface{})
		var body strings.Builder
		for _, s := range steps {
			step, isMap := s.(map[string]interface{})
			if !isMap {
				continue
			}
			run, _ := step["run"].(string)
			body.WriteString(run)
			body.WriteString("\n")
		}
		text := body.String()
		if strings.Contains(text, "\nforge ") || strings.HasPrefix(text, "forge ") {
			if !strings.Contains(text, installForgeScript) {
				t.Errorf("job %q runs forge but never installs it", name)
			}
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
		ProjectName:     "minimal",
		LintGolangci:    true,
		TestRace:        true,
		PermContents:    "read",
		HasDocker:       true,
		VerifyGenerated: true,
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
	for _, absent := range []string{"deployability", "vuln-scan", "e2e"} {
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
		ProjectName:     "myapp",
		LintGolangci:    true,
		TestRace:        true,
		PermContents:    "read",
		E2EEnabled:      true,
		E2ERuntime:      "k3d",
		HasDocker:       true,
		VerifyGenerated: true,
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
	e2e, ok := jobs["e2e"]
	if !ok {
		t.Fatal("missing e2e job for k3d runtime")
	}
	// The k3d lane goes THROUGH forge (cluster, build, deploy + rollout
	// wait), exactly as e2e.yml does. A raw image build, a hand-named k3d
	// cluster or a label-selected kubectl wait each re-state a fact the
	// e2e env declares, and the old `-l app=<project>` wait matched no pod.
	job, _ := yaml.Marshal(e2e)
	for _, want := range []string{"forge cluster up e2e", "forge env build e2e --push", "forge env deploy e2e"} {
		if !strings.Contains(string(job), want) {
			t.Errorf("k3d e2e job does not run %q:\n%s", want, job)
		}
	}
	for _, bypass := range []string{"docker build", "k3d image import", "kubectl wait", "kubectl apply", "k3d-action"} {
		if strings.Contains(string(job), bypass) {
			t.Errorf("k3d e2e job still routes around forge with %q:\n%s", bypass, job)
		}
	}
}
