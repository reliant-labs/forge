package templates

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCIWorkflowTemplate_InstallForgeRef pins the three-branch install
// policy in ci.yml.tmpl: a resolvable ForgeVersion pins by version; an
// empty ForgeVersion (the value InstallableVersion() returns for a
// +dirty/dev build) falls back to the git SHA; neither falls back to
// @main. The load-bearing guarantee (fr-8c8a24ea97): the rendered
// `go install` ref is NEVER a `+dirty` pseudo-version.
func TestCIWorkflowTemplate_InstallForgeRef(t *testing.T) {
	cases := []struct {
		name        string
		version     string
		commit      string
		wantInstall string
	}{
		{
			name:        "resolvable version pins by version",
			version:     "v1.2.3",
			commit:      "abc123",
			wantInstall: "go install github.com/reliant-labs/forge/cmd/forge@v1.2.3",
		},
		{
			name: "empty version (the +dirty/dev case) falls back to SHA",
			// InstallableVersion() returns "" for a +dirty build; the
			// stamper passes that "" through, so the template must pin SHA.
			version:     "",
			commit:      "a3e3b883c97c",
			wantInstall: "go install github.com/reliant-labs/forge/cmd/forge@a3e3b883c97c",
		},
		{
			name:        "neither version nor commit falls back to @main",
			version:     "",
			commit:      "",
			wantInstall: "go install github.com/reliant-labs/forge/cmd/forge@main",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := CIWorkflowData{
				ProjectName:     "myapp",
				GoVersion:       "1.26",
				HasServices:     true,
				VerifyGenerated: true,
				PermContents:    "read",
				ForgeVersion:    c.version,
				ForgeGitCommit:  c.commit,
			}
			content, err := CITemplates("github").Render("ci.yml.tmpl", data)
			if err != nil {
				t.Fatalf("render error: %v", err)
			}
			out := string(content)
			if !strings.Contains(out, c.wantInstall) {
				t.Errorf("rendered ci.yml missing %q\n%s", c.wantInstall, out)
			}
			if strings.Contains(out, "+dirty") {
				t.Errorf("rendered ci.yml must never contain a +dirty install ref\n%s", out)
			}
		})
	}
}

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
		ProjectName:     "minimal",
		GoVersion:       "1.26",
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
		ProjectName:     "myapp",
		GoVersion:       "1.26",
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
	if _, ok := jobs["e2e"]; !ok {
		t.Error("missing e2e job for k3d runtime")
	}
}
