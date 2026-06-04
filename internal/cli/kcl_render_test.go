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
        "env_file": ".env.dev"
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
	if admin.Deploy.Host.EnvFile != ".env.dev" {
		t.Errorf("admin-server env_file: got %q", admin.Deploy.Host.EnvFile)
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

func TestParseKCLEntities_EmptyJSON(t *testing.T) {
	entities, err := parseKCLEntities([]byte("  "))
	if err != nil {
		t.Fatalf("parseKCLEntities on empty: %v", err)
	}
	if len(entities.Services) != 0 || len(entities.Operators) != 0 {
		t.Errorf("expected empty entities, got %+v", entities)
	}
}
