package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKCLModuleExposesClusterRBAC asserts the upstream KCL module
// surface still carries the cluster-RBAC opt-in: Operator declares a
// `cluster_rbac: ClusterRBAC` field and the rbac lib emits both
// ClusterRole + ClusterRoleBinding (operator path) and namespaced
// Role + RoleBinding (service / cronjob path).
//
// FRICTION 2026-06-02: cp-forge layer 8 had to hand-port a cluster-RBAC
// renderer because the legacy Application only emitted namespaced
// Role + RoleBinding. The workspace-controller (cross-namespace
// operator) needed cluster-scope perms. The new Operator type makes
// the intent typed.
func TestKCLModuleExposesClusterRBAC(t *testing.T) {
	root := kclModuleRoot(t)

	schemaBytes, err := os.ReadFile(filepath.Join(root, "schema.k"))
	if err != nil {
		t.Fatalf("read kcl/schema.k: %v", err)
	}
	schema := string(schemaBytes)
	for _, want := range []string{
		"schema ClusterRBAC:",
		"cluster_rbac: ClusterRBAC = ClusterRBAC {}",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema.k missing %q (cluster_rbac opt-in absent)", want)
		}
	}

	rbacBytes, err := os.ReadFile(filepath.Join(root, "lib", "rbac.k"))
	if err != nil {
		t.Fatalf("read kcl/lib/rbac.k: %v", err)
	}
	rbac := string(rbacBytes)
	for _, want := range []string{
		"render_cluster_rbac",
		"render_namespaced_rbac",
		`kind = "ClusterRole"`,
		`kind = "ClusterRoleBinding"`,
		`kind = "Role"`,
		`kind = "RoleBinding"`,
	} {
		if !strings.Contains(rbac, want) {
			t.Errorf("lib/rbac.k missing %q (cluster_rbac branch absent)", want)
		}
	}
}

func TestDeployTemplate_AllEnvironments(t *testing.T) {
	data := DeployWorkflowData{
		ProjectName: "myapp",
		Environments: []DeployEnv{
			{Name: "staging", Auto: true, Protection: false},
			{Name: "preprod", Auto: false, Protection: true, URL: "https://preprod.example.com"},
			{Name: "prod", Auto: false, Protection: true, URL: "https://example.com"},
		},
		Registry:         "ghcr",
		HasFrontends:     true,
		FrontendDeploy:   "none",
		MigrationTest:    true,
		Concurrency:      true,
		CancelInProgress: false,
	}

	out, err := CITemplates("github").Render("deploy.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	s := string(out)

	// Must have all three deploy jobs
	for _, env := range []string{"deploy-staging", "deploy-preprod", "deploy-prod"} {
		if !strings.Contains(s, env+":") {
			t.Errorf("missing job %q", env)
		}
	}

	// Must have retag-release and deploy-release
	if !strings.Contains(s, "retag-release:") {
		t.Error("missing retag-release job")
	}
	if !strings.Contains(s, "deploy-release:") {
		t.Error("missing deploy-release job")
	}

	// Must have migration test for preprod and prod
	if strings.Count(s, "Test migrations") < 2 {
		t.Error("expected migration test steps for preprod and prod")
	}

	// Must have concurrency groups
	if !strings.Contains(s, "group: deploy-staging") {
		t.Error("missing concurrency group for staging")
	}

	// Must have environment protection
	if !strings.Contains(s, "name: prod") {
		t.Error("missing environment protection for prod")
	}

	// Sequential promotion: preprod needs staging
	if !strings.Contains(s, "needs: [deploy-staging]") {
		t.Error("missing sequential dependency: preprod -> staging")
	}

	// Frontend retagging
	if !strings.Contains(s, "frontends/*/") {
		t.Error("missing frontend retagging in retag-release")
	}

	// Header — CI workflows are write-once scaffolds ("yours"), not
	// Tier-1 regenerated files, so they carry the scaffold banner.
	if !strings.HasPrefix(s, "# yours: scaffolded once, never touched again") {
		t.Error("missing scaffold header")
	}
}

func TestDeployTemplate_EmptyEnvironments(t *testing.T) {
	data := DeployWorkflowData{
		ProjectName:  "myapp",
		Environments: nil,
		Registry:     "ghcr",
	}

	out, err := CITemplates("github").Render("deploy.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	s := string(out)
	// With no environments, should just have the header and name
	if !strings.Contains(s, "# yours: scaffolded once, never touched again") {
		t.Error("missing header for empty envs")
	}
	// Should NOT have deploy jobs
	if strings.Contains(s, "deploy-staging") {
		t.Error("should not have deploy jobs with empty environments")
	}
}

func TestDeployTemplate_GARRegistry(t *testing.T) {
	data := DeployWorkflowData{
		ProjectName: "myapp",
		Environments: []DeployEnv{
			{Name: "staging", Auto: true},
		},
		Registry: "gar",
	}

	out, err := CITemplates("github").Render("deploy.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	s := string(out)
	if !strings.Contains(s, "vars.GAR_REGISTRY") {
		t.Error("should use GAR_REGISTRY variable for gar registry")
	}
}

func TestBuildImagesTemplate_Full(t *testing.T) {
	data := BuildImagesWorkflowData{
		ProjectName:  "myapp",
		Registry:     "ghcr",
		HasFrontends: true,
		VulnDocker:   true,
	}

	out, err := CITemplates("github").Render("build-images.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	s := string(out)

	// Header — write-once scaffold banner, not the Tier-1 generated one.
	if !strings.HasPrefix(s, "# yours: scaffolded once, never touched again") {
		t.Error("missing scaffold header")
	}

	// Concurrency group
	if !strings.Contains(s, "group: build-images-") {
		t.Error("missing concurrency group")
	}

	// Trivy scan
	if !strings.Contains(s, "trivy-scan:") {
		t.Error("missing trivy-scan job")
	}
	if !strings.Contains(s, "exit-code: \"1\"") {
		t.Error("trivy should fail on vulnerabilities")
	}

	// Frontend builds
	if !strings.Contains(s, "build-push-frontends:") {
		t.Error("missing build-push-frontends job")
	}

	// workflow_dispatch
	if !strings.Contains(s, "workflow_dispatch:") {
		t.Error("missing workflow_dispatch trigger")
	}

	// Proper tagging
	if !strings.Contains(s, "type=sha,prefix=sha-") {
		t.Error("missing sha tag")
	}
	if !strings.Contains(s, "type=semver") {
		t.Error("missing semver tag")
	}

	// Summary includes all jobs
	if !strings.Contains(s, "trivy scan") {
		t.Error("summary should include trivy scan")
	}
}

func TestBuildImagesTemplate_Minimal(t *testing.T) {
	data := BuildImagesWorkflowData{
		ProjectName:  "myapp",
		Registry:     "gar",
		HasFrontends: false,
		VulnDocker:   false,
	}

	out, err := CITemplates("github").Render("build-images.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	s := string(out)

	// Should NOT have trivy or frontends
	if strings.Contains(s, "trivy-scan:") {
		t.Error("should not have trivy-scan when VulnDocker=false")
	}
	if strings.Contains(s, "build-push-frontends:") {
		t.Error("should not have build-push-frontends when HasFrontends=false")
	}

	// Should use GAR registry
	if !strings.Contains(s, "vars.GAR_REGISTRY") {
		t.Error("should use GAR_REGISTRY for gar registry")
	}
}
