package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

func renderDeploy(t *testing.T, data DeployWorkflowData) string {
	t.Helper()
	out, err := CITemplates("github").Render("deploy.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render deploy.yml: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("deploy.yml is not valid YAML: %v\n%s", err, out)
	}
	return string(out)
}

func deployFixture() DeployWorkflowData {
	return DeployWorkflowData{
		ProjectName: "myapp",
		Environments: []DeployEnv{
			{Name: "staging", Auto: true},
			{Name: "preprod", Protection: true},
			{Name: "prod", Protection: true},
		},
		Registry:         "ghcr",
		HasFrontends:     true,
		FrontendPath:     "frontends/web",
		Concurrency:      true,
		CancelInProgress: false,
	}
}

// Deploys go THROUGH forge — never `kcl run | kubectl apply`.
//
// The raw pipe cannot resolve kcl_plugin.forge, which every env render
// imports, so on a clean clone (no committed .forge-kcl/) it fails before it
// renders anything. Where it did run it skipped the declared-context binding
// (it applied to whatever context the kubeconfig had current), the
// secret/image preflight, digest pinning, the per-env config.js render, and
// the hosted-env dispatch entirely.
func TestDeployTemplate_DeploysThroughForge(t *testing.T) {
	s := renderDeploy(t, deployFixture())

	for _, bypass := range []string{"kcl run", "kubectl apply", "install-cli.sh"} {
		if strings.Contains(s, bypass) {
			t.Errorf("deploy.yml bypasses forge: it contains %q", bypass)
		}
	}
	for _, want := range []string{
		`forge build "${{ matrix.env }}" --push "$REGISTRY"`,
		`forge env deploy "${{ matrix.env }}"`,
		// The same run-time install every CI job uses.
		`CGO_ENABLED=1 go install "github.com/reliant-labs/forge/cmd/forge@${v}"`,
		// Both credential kinds: forge picks one from the env's own KCL.
		"secrets.KUBECONFIG",
		"FORGE_CONTROL_PLANE_TOKEN: ${{ secrets.FORGE_CONTROL_PLANE_TOKEN }}",
		// The frontend build needs its deps installed.
		"working-directory: frontends/web",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("deploy.yml is missing %q", want)
		}
	}
	// The workflow_run deploy ships the commit whose images were built.
	if !strings.Contains(s, "github.event.workflow_run.head_sha") {
		t.Error("an auto deploy must check out the commit whose image build triggered it")
	}
	if strings.Contains(s, "tags:") {
		t.Error("a tag push must not deploy anything — a tag records a release, it does not ship one")
	}
	if !strings.HasPrefix(s, "# yours: scaffolded once, never touched again") {
		t.Error("missing scaffold header")
	}
}

// The workflow targets exactly the envs it was given — one matrix entry and
// one dispatch option each — with auto/protection carried per env.
func TestDeployTemplate_TargetsOnlyTheGivenEnvs(t *testing.T) {
	s := renderDeploy(t, DeployWorkflowData{
		ProjectName:  "hounders",
		Environments: []DeployEnv{{Name: "prod", Protection: true}},
		Registry:     "ghcr",
	})
	var wf struct {
		On struct {
			WorkflowDispatch struct {
				Inputs struct {
					Environment struct {
						Options []string `yaml:"options"`
					} `yaml:"environment"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []map[string]any `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(s), &wf); err != nil {
		t.Fatal(err)
	}
	if got := wf.On.WorkflowDispatch.Inputs.Environment.Options; len(got) != 1 || got[0] != "prod" {
		t.Errorf("dispatch options = %v, want [prod]", got)
	}
	include := wf.Jobs["deploy"].Strategy.Matrix.Include
	if len(include) != 1 || include[0]["env"] != "prod" {
		t.Fatalf("matrix = %v, want exactly prod", include)
	}
	if include[0]["auto"] != false {
		t.Errorf("prod auto = %v: a lone env must not deploy on every merge to main", include[0]["auto"])
	}
	if include[0]["protected"] != true {
		t.Errorf("prod protected = %v, want true", include[0]["protected"])
	}
	if strings.Contains(s, "staging") {
		t.Error("deploy.yml names staging, an env this project does not declare")
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
	if strings.Contains(s, "jobs:") {
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
	_ = renderDeploy(t, data)

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
		ProjectName: "myapp",
		Registry:    "ghcr",
		VulnDocker:  true,
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

	// No frontend images here: they are per-env (config.js is rendered
	// into the build), built by `forge build <env> --push` in deploy.yml.
	// The raw `docker buildx build frontends/*` baked the on-disk (dev, or
	// absent) config.js into every image.
	if strings.Contains(s, "build-push-frontends:") || strings.Contains(s, "frontends/*/Dockerfile") {
		t.Error("build-images.yml builds frontend images outside forge, without the env's config.js")
	}

	// workflow_dispatch
	if !strings.Contains(s, "workflow_dispatch:") {
		t.Error("missing workflow_dispatch trigger")
	}

	// SLSA provenance goes to GitHub's attestation store, which refuses
	// private repositories outside Enterprise Cloud ("Feature not available
	// for user-owned private repositories") — AFTER the image is pushed, so
	// every push to main went red (houndersclub). The image stays
	// cosign-signed with an SBOM either way.
	i := strings.Index(s, "uses: actions/attest-build-provenance@")
	if i < 0 {
		t.Fatal("missing the provenance attestation step")
	}
	step := s[strings.LastIndex(s[:i], "- name:"):i]
	if !strings.Contains(step, "if: ${{ !github.event.repository.private }}") {
		t.Errorf("provenance attestation is not gated off for private repositories:\n%s", step)
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
		ProjectName: "myapp",
		Registry:    "gar",
		VulnDocker:  false,
	}

	out, err := CITemplates("github").Render("build-images.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	s := string(out)

	// Should NOT have trivy
	if strings.Contains(s, "trivy-scan:") {
		t.Error("should not have trivy-scan when VulnDocker=false")
	}

	// Should use GAR registry
	if !strings.Contains(s, "vars.GAR_REGISTRY") {
		t.Error("should use GAR_REGISTRY for gar registry")
	}
}
