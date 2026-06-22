package cli

import (
	"strings"
	"testing"
)

const sampleKCLJSON = `{
  "services": [
    {
      "name": "admin-server",
      "image": "cp-forge:dev",
      "deploy": {
        "type": "host",
        "runner": "air",
        "air_config": ".air.toml",
        "env_vars": [
          {"name": "LOG_LEVEL", "value": "debug"},
          {"name": "DATABASE_URL", "value": "postgres://localhost:5432/x?sslmode=disable"}
        ],
        "secrets_file": ".env.dev.secrets",
        "delve_port": 2345
      },
      "env_vars": [{"name": "FOO", "value": "bar"}],
      "command": ["server", "admin-server"]
    },
    {
      "name": "workspace-proxy",
      "image": "cp-forge:dev",
      "deploy": {
        "type": "cluster",
        "replicas": 1,
        "ingress": {"host": "proxy.example.com", "path": "/"},
        "platform": "amd64",
        "ports": [8080]
      }
    },
    {
      "name": "reliant-daemon",
      "deploy": {
        "type": "build-only",
        "build_variants": [
          {"name": "dev", "ldflags": ["-X", "main.foo=dev"]},
          {"name": "prod", "ldflags": ["-X", "main.foo=prod"]}
        ]
      }
    }
  ],
  "operators": [
    {
      "name": "workspace-controller",
      "image": "cp-forge:dev",
      "crds": ["Workspace"],
      "leader_election": true,
      "replicas": 1
    }
  ],
  "frontends": [
    {
      "name": "admin-web",
      "type": "nextjs",
      "path": "frontends/admin-web",
      "dev_runner": "npm",
      "port": 3000
    }
  ],
  "cronjobs": [
    {
      "name": "billing-sweeper",
      "schedule": "@hourly",
      "image": "cp-forge:dev",
      "command": ["billing", "sweep"]
    }
  ]
}`

func TestParseKCLEntities_DispatchByDeployType(t *testing.T) {
	entities, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}

	if got := len(entities.Services); got != 3 {
		t.Fatalf("services: got %d, want 3", got)
	}
	if got := len(entities.Operators); got != 1 {
		t.Errorf("operators: got %d, want 1", got)
	}
	if got := len(entities.Frontends); got != 1 {
		t.Errorf("frontends: got %d, want 1", got)
	}
	if got := len(entities.CronJobs); got != 1 {
		t.Errorf("cronjobs: got %d, want 1", got)
	}

	// admin-server: host
	admin := entities.FindService("admin-server")
	if admin == nil {
		t.Fatal("admin-server not found")
	}
	if admin.Deploy.Type != "host" {
		t.Errorf("admin-server type: got %q, want host", admin.Deploy.Type)
	}
	if admin.Deploy.Host == nil {
		t.Fatal("admin-server.Deploy.Host is nil")
	}
	if admin.Deploy.Cluster != nil || admin.Deploy.BuildOnly != nil {
		t.Error("admin-server has stray Cluster/BuildOnly populated")
	}
	if admin.Deploy.Host.Runner != "air" {
		t.Errorf("admin-server runner: got %q, want air", admin.Deploy.Host.Runner)
	}
	if admin.Deploy.Host.AirConfig != ".air.toml" {
		t.Errorf("admin-server air_config: got %q", admin.Deploy.Host.AirConfig)
	}
	if admin.Deploy.Host.SecretsFile != ".env.dev.secrets" {
		t.Errorf("admin-server secrets_file: got %q", admin.Deploy.Host.SecretsFile)
	}
	if got := len(admin.Deploy.Host.EnvVars); got != 2 {
		t.Errorf("admin-server env_vars count: got %d, want 2", got)
	}
	if admin.Deploy.Host.DelvePort != 2345 {
		t.Errorf("admin-server delve_port: got %d, want 2345", admin.Deploy.Host.DelvePort)
	}

	// workspace-proxy: cluster
	proxy := entities.FindService("workspace-proxy")
	if proxy == nil {
		t.Fatal("workspace-proxy not found")
	}
	if proxy.Deploy.Type != "cluster" {
		t.Errorf("workspace-proxy type: got %q, want cluster", proxy.Deploy.Type)
	}
	if proxy.Deploy.Cluster == nil {
		t.Fatal("workspace-proxy.Deploy.Cluster is nil")
	}
	if proxy.Deploy.Cluster.Platform != "amd64" {
		t.Errorf("workspace-proxy platform: got %q, want amd64", proxy.Deploy.Cluster.Platform)
	}
	if len(proxy.Deploy.Cluster.Ports) != 1 || proxy.Deploy.Cluster.Ports[0] != 8080 {
		t.Errorf("workspace-proxy ports: got %v", proxy.Deploy.Cluster.Ports)
	}

	// reliant-daemon: build-only
	daemon := entities.FindService("reliant-daemon")
	if daemon == nil {
		t.Fatal("reliant-daemon not found")
	}
	if daemon.Deploy.Type != "build-only" {
		t.Errorf("reliant-daemon type: got %q, want build-only", daemon.Deploy.Type)
	}
	if daemon.Deploy.BuildOnly == nil {
		t.Fatal("reliant-daemon.Deploy.BuildOnly is nil")
	}
	if got := len(daemon.Deploy.BuildOnly.BuildVariants); got != 2 {
		t.Errorf("reliant-daemon variants: got %d, want 2", got)
	}
}

func TestParseKCLEntities_SkipSetHelpers(t *testing.T) {
	entities, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if got := entities.HostServiceNames(); len(got) != 1 || got[0] != "admin-server" {
		t.Errorf("HostServiceNames: got %v, want [admin-server]", got)
	}
	if got := entities.ClusterServiceNames(); len(got) != 1 || got[0] != "workspace-proxy" {
		t.Errorf("ClusterServiceNames: got %v, want [workspace-proxy]", got)
	}
	if got := entities.BuildOnlyServiceNames(); len(got) != 1 || got[0] != "reliant-daemon" {
		t.Errorf("BuildOnlyServiceNames: got %v, want [reliant-daemon]", got)
	}
}

func TestDispatchServiceDeploy_Errors(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"missing type", `{"replicas": 1}`, "deploy.type missing"},
		{"unknown type", `{"type": "lambda"}`, "unrecognised deploy.type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := dispatchServiceDeploy("svc", []byte(c.raw))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestDispatchServiceBuild(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantType string
		check    func(t *testing.T, b BuildConfigEntity)
	}{
		{
			name:     "go",
			raw:      `{"type":"go","cmd":"./cmd/trader","goarch":"arm64","flags":["-cover"],"ldflags":["-s"],"env":{"CGO_ENABLED":"0"}}`,
			wantType: "go",
			check: func(t *testing.T, b BuildConfigEntity) {
				if b.Go == nil || b.Go.Cmd != "./cmd/trader" || b.Go.GOARCH != "arm64" {
					t.Errorf("go build = %+v", b.Go)
				}
				if len(b.Go.Flags) != 1 || b.Go.Flags[0] != "-cover" {
					t.Errorf("flags = %v", b.Go.Flags)
				}
			},
		},
		{
			name:     "docker",
			raw:      `{"type":"docker","dockerfile":"Dockerfile.x","platform":"linux/arm64","target":"runtime"}`,
			wantType: "docker",
			check: func(t *testing.T, b BuildConfigEntity) {
				if b.Docker == nil || b.Docker.Dockerfile != "Dockerfile.x" || b.Docker.Platform != "linux/arm64" {
					t.Errorf("docker build = %+v", b.Docker)
				}
			},
		},
		{
			name:     "shell",
			raw:      `{"type":"shell","cmd":"make build"}`,
			wantType: "shell",
			check: func(t *testing.T, b BuildConfigEntity) {
				if b.Shell == nil || b.Shell.Cmd != "make build" {
					t.Errorf("shell build = %+v", b.Shell)
				}
			},
		},
		{
			name:     "null is absent",
			raw:      `null`,
			wantType: "",
			check:    func(t *testing.T, b BuildConfigEntity) {},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := dispatchServiceBuild("svc", []byte(c.raw))
			if err != nil {
				t.Fatalf("dispatchServiceBuild: %v", err)
			}
			if b.Type != c.wantType {
				t.Fatalf("type = %q, want %q", b.Type, c.wantType)
			}
			c.check(t, b)
		})
	}
}

func TestDispatchServiceBuild_Errors(t *testing.T) {
	for _, c := range []struct{ name, raw, wantErr string }{
		{"missing type", `{"cmd":"x"}`, "build.type missing"},
		{"unknown type", `{"type":"rust"}`, "unrecognised build.type"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := dispatchServiceBuild("svc", []byte(c.raw))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

func TestEffectiveBuild_SynthesizesGoDefault(t *testing.T) {
	// A service with no build block falls back to GoBuild { cmd =
	// ./cmd/<name> } — the ONE place the default lives.
	s := ServiceEntity{Name: "api"}
	b := s.EffectiveBuild()
	if b.Type != "go" || b.Go == nil || b.Go.Cmd != "./cmd/api" || b.Go.OutputName != "api" {
		t.Fatalf("EffectiveBuild default = %+v / %+v", b, b.Go)
	}
	// An explicit build wins over the synthesized default.
	s2 := ServiceEntity{Name: "api", Build: BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cmd: "x"}}}
	if s2.EffectiveBuild().Type != "shell" {
		t.Errorf("explicit build dropped: %+v", s2.EffectiveBuild())
	}
	// A compose service has no Go artifact — no synthesized GoBuild.
	sc := ServiceEntity{Name: "dev-infra", Deploy: DeployConfigEntity{Type: "compose"}}
	if sc.EffectiveBuild().Type != "" {
		t.Errorf("compose service synthesized a build: %+v", sc.EffectiveBuild())
	}
	// An external service owns its own build via build_cmd / deploy_cmd —
	// no synthesized GoBuild against a package that may not exist locally.
	se := ServiceEntity{Name: "sibling", Deploy: DeployConfigEntity{Type: "external"}}
	if se.EffectiveBuild().Type != "" {
		t.Errorf("external service synthesized a build: %+v", se.EffectiveBuild())
	}
	// A top-level build_cmd (generic shell escape hatch for a non-external
	// deploy type) also suppresses the synthesized GoBuild.
	sb := ServiceEntity{Name: "sib2", Deploy: DeployConfigEntity{Type: "host"}, BuildCmd: "make foo"}
	if sb.EffectiveBuild().Type != "" {
		t.Errorf("build_cmd service synthesized a build: %+v", sb.EffectiveBuild())
	}
	// An EXPLICIT build still wins even for compose (defensive — a user can
	// force a go build onto any deploy type).
	scb := ServiceEntity{Name: "x", Deploy: DeployConfigEntity{Type: "compose"}, Build: BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/x"}}}
	if scb.EffectiveBuild().Type != "go" {
		t.Errorf("explicit go build dropped for compose: %+v", scb.EffectiveBuild())
	}
}

func TestGoBuildTargetsFromKCL_DedupsSharedBinary(t *testing.T) {
	// Two server services that map to the same shared ./cmd/proj binary
	// collapse to ONE go-build target; a distinct binary stays separate.
	e := &KCLEntities{Services: []ServiceEntity{
		{Name: "users", Build: BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/proj", OutputName: "proj"}}},
		{Name: "orders", Build: BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/proj", OutputName: "proj"}}},
		{Name: "gateway", Build: BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/gateway", OutputName: "gateway"}}},
		{Name: "image", Build: BuildConfigEntity{Type: "docker", Docker: &DockerBuild{}}},
	}}
	targets := goBuildTargetsFromKCL(e)
	if len(targets) != 2 {
		t.Fatalf("want 2 deduped go targets, got %d: %+v", len(targets), targets)
	}
	got := map[string]bool{}
	for _, tt := range targets {
		got[tt.cmd] = true
	}
	if !got["./cmd/proj"] || !got["./cmd/gateway"] {
		t.Errorf("targets = %+v, want ./cmd/proj + ./cmd/gateway (docker excluded)", targets)
	}
}

func TestParseKCLEntities_EmptyJSON(t *testing.T) {
	entities, err := parseKCLEntities([]byte("  "))
	if err != nil {
		t.Fatalf("parseKCLEntities on empty: %v", err)
	}
	if len(entities.Services) != 0 || len(entities.Operators) != 0 {
		t.Errorf("expected empty entities, got %+v", entities)
	}
}

// TestParseKCLEntities_OutputWrapper pins the wrapper-aware behavior:
// the canonical generated main.k declares
//
//	output    = forge.render(_bundle)
//	manifests = forge.render_manifests(_bundle, _env)
//
// so `kcl run --format json` emits `{"output": {...services, ...}, "manifests": [...]}`
// at the top level. parseKCLEntities must unwrap `output` and then
// parse the inner entity set. Without this, every consumer
// (forge build/run/up/deploy) silently degrades to zero entities and
// no error — see the bug fixed in commit 8ceef73.
func TestParseKCLEntities_OutputWrapper(t *testing.T) {
	wrapped := `{
  "output": ` + sampleKCLJSON + `,
  "manifests": [
    {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "x"}}
  ]
}`
	entities, err := parseKCLEntities([]byte(wrapped))
	if err != nil {
		t.Fatalf("parseKCLEntities on wrapped JSON: %v", err)
	}
	if got := len(entities.Services); got != 3 {
		t.Errorf("wrapped services: got %d, want 3", got)
	}
	if got := len(entities.Operators); got != 1 {
		t.Errorf("wrapped operators: got %d, want 1", got)
	}
	// Dispatch must still work through the wrapper.
	if admin := entities.FindService("admin-server"); admin == nil || admin.Deploy.Type != "host" {
		t.Errorf("admin-server lost dispatch through wrapper: %+v", admin)
	}
}

// TestParseKCLEntities_ManifestNamespaceFallback covers the
// manifests-only render shape: a project's main.k emits ONLY
// `manifests = forge.render_manifests(...)` (no `output = forge.render`
// entity echo), so the entity contract — and every cluster-shaped
// service's K8sCluster.namespace — is absent. The namespace must still
// be recovered from the rendered objects' metadata.namespace so
// k8sClusterNamespaceForEnv (forge deploy/smoke/secrets) keeps resolving
// without --namespace. The cluster-scoped objects (Namespace, CRD,
// ClusterRole) carry no namespace and are ignored; the dominant
// namespaced value wins.
func TestParseKCLEntities_ManifestNamespaceFallback(t *testing.T) {
	manifestsOnly := `{
  "GATEWAYS": [{"name": "public", "host": "preprod.example.com"}],
  "HTTP_ROUTES": [{"name": "api", "gateway": "public", "service": "admin", "port": 8090, "host": "preprod.example.com"}],
  "manifests": [
    {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "control-plane-preprod"}},
    {"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": {"name": "workspaces.x"}},
    {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "admin-server", "namespace": "control-plane-preprod"}},
    {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "admin-server", "namespace": "control-plane-preprod"}}
  ]
}`
	entities, err := parseKCLEntities([]byte(manifestsOnly))
	if err != nil {
		t.Fatalf("parseKCLEntities manifests-only: %v", err)
	}
	// No entity contract -> no service entities.
	if got := len(entities.Services); got != 0 {
		t.Errorf("manifests-only services: got %d, want 0", got)
	}
	// Gateways/routes still come through (case-insensitive flat keys).
	if got := len(entities.Gateways); got != 1 {
		t.Errorf("manifests-only gateways: got %d, want 1", got)
	}
	// Namespace recovered from manifest metadata.
	if got := entities.ManifestNamespace; got != "control-plane-preprod" {
		t.Errorf("ManifestNamespace = %q, want control-plane-preprod", got)
	}
}

// TestManifestNamespaceFromOuter_DominantWins confirms the namespace
// tally ignores cluster-scoped (namespace-less) objects and picks the
// dominant namespace deterministically when more than one appears.
func TestManifestNamespaceFromOuter_DominantWins(t *testing.T) {
	outer := `{"manifests": [
	  {"kind": "ClusterRole", "metadata": {"name": "x"}},
	  {"kind": "Deployment", "metadata": {"namespace": "main-ns"}},
	  {"kind": "Service", "metadata": {"namespace": "main-ns"}},
	  {"kind": "Secret", "metadata": {"namespace": "other-ns"}}
	]}`
	if got := manifestNamespaceFromOuter([]byte(outer)); got != "main-ns" {
		t.Errorf("manifestNamespaceFromOuter = %q, want main-ns", got)
	}
	if got := manifestNamespaceFromOuter([]byte(`{"manifests":[]}`)); got != "" {
		t.Errorf("empty manifests namespace = %q, want empty", got)
	}
}

// TestParseKCLEntities_FlatShapeStillWorks confirms backward compat:
// callers that pass the raw `{services: [...], operators: [...], ...}`
// shape (e.g. tests using FORGE_KCL_RENDER_FIXTURE files written in
// the unwrapped form, or future main.k templates that drop the
// `output` wrapper) parse identically.
func TestParseKCLEntities_FlatShapeStillWorks(t *testing.T) {
	entities, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities flat: %v", err)
	}
	if got := len(entities.Services); got != 3 {
		t.Errorf("flat services: got %d, want 3", got)
	}
}

// NOTE: TestKCLRunArgs_* removed with the exec("kcl") path. The env →
// `-D env=<env>` plumbing they pinned now lives in renderKCLViaKpm's
// WithArguments and is exercised end-to-end by the kcl-go parity smoke
// test. TODO: restore per-env (dev-host/staging/prod) propagation
// coverage with a fixture KCL package rendered through renderKCLViaKpm.
