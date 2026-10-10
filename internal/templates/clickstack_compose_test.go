package templates

// Guards for the generated ClickStack compose service, derived from the parsed
// compose graph. Each one is a failure the first ClickStack attempt shipped.

import (
	"regexp"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

type clickstackCompose struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Init        bool              `yaml:"init"`
		Ports       []string          `yaml:"ports"`
		Environment map[string]string `yaml:"environment"`
		Volumes     []string          `yaml:"volumes"`
		Entrypoint  []string          `yaml:"entrypoint"`
		Healthcheck struct {
			Test []string `yaml:"test"`
		} `yaml:"healthcheck"`
		Networks yaml.Node `yaml:"networks"`
	} `yaml:"services"`
	Volumes map[string]any `yaml:"volumes"`
}

func renderClickstackCompose(t *testing.T) (clickstackCompose, string) {
	t.Helper()
	out, err := ProjectTemplates().Render("docker-compose.yml.tmpl", struct {
		ProjectName string
		HasFrontend bool
	}{ProjectName: "shopdemo"})
	if err != nil {
		t.Fatalf("render docker-compose.yml.tmpl: %v", err)
	}
	var cf clickstackCompose
	if err := yaml.Unmarshal(out, &cf); err != nil {
		t.Fatalf("rendered compose is not valid YAML: %v\n%s", err, out)
	}
	if _, ok := cf.Services["clickstack"]; !ok {
		t.Fatalf("no clickstack service in the rendered compose (the assertions below would be vacuous): %v", cf.Services)
	}
	return cf, string(out)
}

func TestClickstackComposeIsOneLoopbackOnlyService(t *testing.T) {
	cf, raw := renderClickstackCompose(t)
	svc := cf.Services["clickstack"]

	// Every published port is bound to loopback; ClickHouse is not published.
	if len(svc.Ports) != 3 {
		t.Fatalf("clickstack publishes %v, want exactly the UI and the two OTLP ports", svc.Ports)
	}
	for _, p := range svc.Ports {
		if !strings.HasPrefix(p, "127.0.0.1:") {
			t.Errorf("port %q is not bound to 127.0.0.1", p)
		}
		for _, db := range []string{":8123", ":9000", ":27017"} {
			if strings.HasSuffix(p, db) {
				t.Errorf("port %q publishes a database", p)
			}
		}
	}
	for name, s := range cf.Services {
		for _, p := range s.Ports {
			if strings.HasPrefix(p, "0:") || strings.HasPrefix(p, "0.0.0.0") {
				if name == "clickstack" {
					t.Errorf("clickstack publishes on all interfaces: %q", p)
				}
			}
		}
	}

	// Non-default credentials; the passwordless default user is not the one in use.
	if svc.Environment["CLICKHOUSE_USER"] == "" || svc.Environment["CLICKHOUSE_USER"] == "default" {
		t.Errorf("CLICKHOUSE_USER = %q, want a non-default user", svc.Environment["CLICKHOUSE_USER"])
	}
	if svc.Environment["CLICKHOUSE_PASSWORD"] == "" {
		t.Error("ClickHouse has no password")
	}

	// A healthcheck that proves ingest, not just the UI.
	hc := strings.Join(svc.Healthcheck.Test, " ")
	for _, want := range []string{"/api/health", "4318/v1/traces"} {
		if !strings.Contains(hc, want) {
			t.Errorf("healthcheck %q does not probe %s", hc, want)
		}
	}

	// Persistent state, a dashboard provisioner dir, and the file log mount.
	mounts := strings.Join(svc.Volumes, "\n")
	for _, want := range []string{
		"clickstack_clickhouse:/var/lib/clickhouse",
		"clickstack_mongo:/data/db",
		"./deploy/observability/dashboards:/etc/forge/dashboards:ro",
		"./deploy/observability/otel-collector.yaml:",
		"./.forge/logs:/var/log/forge:ro",
	} {
		if !strings.Contains(mounts, want) {
			t.Errorf("clickstack does not mount %s; mounts:\n%s", want, mounts)
		}
	}
	for _, v := range []string{"clickstack_clickhouse", "clickstack_mongo"} {
		if _, ok := cf.Volumes[v]; !ok {
			t.Errorf("top-level volume %s is not declared", v)
		}
	}
	if svc.Environment["DASHBOARD_PROVISIONER_DIR"] != "/etc/forge/dashboards" {
		t.Errorf("DASHBOARD_PROVISIONER_DIR = %q", svc.Environment["DASHBOARD_PROVISIONER_DIR"])
	}

	// Compose services reach the collector by its network name.
	var nets map[string]struct {
		Aliases []string `yaml:"aliases"`
	}
	if err := svc.Networks.Decode(&nets); err != nil {
		t.Fatalf("clickstack networks must use the map form so it can carry an alias: %v", err)
	}
	aliased := false
	for _, n := range nets {
		for _, a := range n.Aliases {
			aliased = aliased || a == "otel-collector"
		}
	}
	if !aliased {
		t.Error("clickstack has no `otel-collector` network alias; compose services cannot reach it")
	}
	if got := cf.Services["app"].Environment["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "http://otel-collector:4318" {
		t.Errorf("the compose app exports to %q, want http://otel-collector:4318", got)
	}

	// Pinned by digest, and the LOCAL-mode image: the separate collector image
	// only opens OTLP after a UI registration and then demands a key.
	if !regexp.MustCompile(`^clickhouse/clickstack-local:\d+\.\d+\.\d+@sha256:[0-9a-f]{64}$`).MatchString(svc.Image) {
		t.Errorf("image %q is not the clickstack-local image pinned by version and digest", svc.Image)
	}
	for _, banned := range []string{"clickstack-otel-collector", "OPAMP_SERVER_URL", "grafana/", "otel-lgtm", "image: mongo", "pyroscope", "alloy", "INGESTION_API_KEY"} {
		if strings.Contains(raw, banned) {
			t.Errorf("the compose file mentions %q: the single-container local mode has none of that", banned)
		}
	}
}

// The collector pipeline file is what makes logs flow: it must read the files
// forge writes and parse the fields the runtime emits.
func TestClickstackCollectorConfigTailsForgeLogs(t *testing.T) {
	out, err := ProjectTemplates().Render("otel-collector.yaml.tmpl", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Receivers map[string]struct {
			Include   []string `yaml:"include"`
			Operators []struct {
				Type string `yaml:"type"`
			} `yaml:"operators"`
		} `yaml:"receivers"`
		Service struct {
			Pipelines map[string]struct {
				Receivers  []string `yaml:"receivers"`
				Exporters  []string `yaml:"exporters"`
				Processors []string `yaml:"processors"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	if err := yaml.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("collector config is not YAML: %v\n%s", err, out)
	}
	fl, ok := cfg.Receivers["filelog/forge"]
	if !ok || len(fl.Include) != 1 || fl.Include[0] != "/var/log/forge/*/*.log" {
		t.Fatalf("filelog/forge = %+v, want it to include /var/log/forge/*/*.log", fl)
	}
	var types []string
	for _, op := range fl.Operators {
		types = append(types, op.Type)
	}
	for _, want := range []string{"regex_parser", "json_parser", "trace_parser"} {
		if !strings.Contains(strings.Join(types, " "), want) {
			t.Errorf("filelog operators %v lack %s", types, want)
		}
	}
	p, ok := cfg.Service.Pipelines["logs/forge"]
	if !ok || len(p.Receivers) != 1 || p.Receivers[0] != "filelog/forge" || len(p.Exporters) != 1 || p.Exporters[0] != "clickhouse" {
		t.Errorf("logs/forge pipeline = %+v, want filelog/forge -> clickhouse", p)
	}
	// Exactly one local log path: the runtime must not also export logs over OTLP.
	for name, pl := range cfg.Service.Pipelines {
		for _, r := range pl.Receivers {
			if strings.HasPrefix(r, "otlp") {
				t.Errorf("pipeline %s also receives logs over OTLP; one shipping path only", name)
			}
		}
	}
}
