package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func clickstackEntities(env map[string]string) *KCLEntities {
	return &KCLEntities{Workloads: []WorkloadEntity{
		{Name: "api", Runtime: RuntimeEntity{Type: RuntimeHost}},
		{Name: "clickstack", Runtime: RuntimeEntity{Type: RuntimeCompose, Compose: &ComposeRuntime{Service: "clickstack", Env: env}}},
	}}
}

// The endpoint host processes export to is read from the port the env DECLARED
// for the compose service to publish, so the two cannot drift.
func TestClickstackHostOTLPEnvFollowsThePublishedPort(t *testing.T) {
	got := clickstackHostOTLPEnv(clickstackEntities(map[string]string{"CLICKSTACK_OTLP_HTTP_PORT": "4419"}))
	want := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4419",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("host OTLP env = %v, want %v", got, want)
	}
}

// No clickstack workload (observability off, or a custom backend) means no
// endpoint is invented: processes run exactly as before.
func TestClickstackHostOTLPEnvIsNilWithoutTheService(t *testing.T) {
	for name, e := range map[string]*KCLEntities{
		"nil entities":      nil,
		"no compose":        {Workloads: []WorkloadEntity{{Name: "api", Runtime: RuntimeEntity{Type: RuntimeHost}}}},
		"other compose":     {Workloads: []WorkloadEntity{{Name: "pg", Runtime: RuntimeEntity{Type: RuntimeCompose, Compose: &ComposeRuntime{Service: "postgres"}}}}},
		"port not declared": clickstackEntities(nil),
		"port zero":         clickstackEntities(map[string]string{"CLICKSTACK_OTLP_HTTP_PORT": "0"}),
	} {
		if got := clickstackHostOTLPEnv(e); got != nil {
			t.Errorf("%s: got %v, want nil", name, got)
		}
	}
}

func TestWithDefaultEnvNeverOverridesWhatTheUserSet(t *testing.T) {
	defaults := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
	}
	t.Run("unset gets the default", func(t *testing.T) {
		got := strings.Join(withDefaultEnv([]string{"PATH=/bin"}, defaults), "\n")
		for _, want := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318", "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf", "PATH=/bin"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in\n%s", want, got)
			}
		}
	})
	t.Run("a user value wins", func(t *testing.T) {
		got := withDefaultEnv([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example:4318"}, defaults)
		n := 0
		for _, kv := range got {
			if strings.HasPrefix(kv, "OTEL_EXPORTER_OTLP_ENDPOINT=") {
				n++
				if kv != "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example:4318" {
					t.Errorf("endpoint was overridden: %s", kv)
				}
			}
		}
		if n != 1 {
			t.Errorf("endpoint appears %d times, want 1", n)
		}
	})
	t.Run("an empty value is an absence, not a decision", func(t *testing.T) {
		// The project-config projection emits one entry per field, so an unset
		// otlp_endpoint arrives as an empty string and must not shadow the default.
		got := strings.Join(withDefaultEnv([]string{"OTEL_EXPORTER_OTLP_ENDPOINT="}, defaults), "\n")
		if !strings.Contains(got, "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318") || strings.Contains(got, "OTEL_EXPORTER_OTLP_ENDPOINT=\n") {
			t.Errorf("an empty endpoint shadowed the default:\n%s", got)
		}
	})
	t.Run("nil defaults change nothing", func(t *testing.T) {
		in := []string{"A=1"}
		if got := withDefaultEnv(in, nil); !reflect.DeepEqual(got, in) {
			t.Errorf("got %v", got)
		}
	})
}

// The endpoint and the protocol that speaks to it are a pair. A user who set one
// has made the transport decision; filling in the other half would pair their
// gRPC :4317 with our http/protobuf and export would fail with nothing on screen.
func TestWithDefaultEnvIsAllOrNothing(t *testing.T) {
	defaults := clickstackHostOTLPEnvForTest()
	for name, user := range map[string][]string{
		"endpoint only (a gRPC collector)": {"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4317"},
		"protocol only":                    {"OTEL_EXPORTER_OTLP_PROTOCOL=grpc"},
		"both":                             {"OTEL_EXPORTER_OTLP_ENDPOINT=http://c:4317", "OTEL_EXPORTER_OTLP_PROTOCOL=grpc"},
	} {
		t.Run(name, func(t *testing.T) {
			in := append([]string{"PATH=/bin"}, user...)
			if got := withDefaultEnv(in, defaults); !reflect.DeepEqual(got, in) {
				t.Errorf("a default was injected beside the user's setting:\n got %v\nwant %v", got, in)
			}
		})
	}
	t.Run("empty values are unset, so the pair is injected whole", func(t *testing.T) {
		got := strings.Join(withDefaultEnv([]string{"OTEL_EXPORTER_OTLP_ENDPOINT=", "OTEL_EXPORTER_OTLP_PROTOCOL="}, defaults), "\n")
		for _, want := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318", "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in\n%s", want, got)
			}
		}
	})
}

// Setting the endpoint or protocol for ONE signal is a transport decision too.
func TestWithDefaultOTLPEnvRespectsPerSignalSettings(t *testing.T) {
	defaults := clickstackHostOTLPEnvForTest()
	for _, kv := range []string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://t:4317",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=grpc",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://l:4318",
	} {
		in := []string{"PATH=/bin", kv}
		if got := withDefaultOTLPEnv(in, defaults); !reflect.DeepEqual(got, in) {
			t.Errorf("%s: defaults injected beside it: %v", kv, got)
		}
	}
	got := withDefaultOTLPEnv([]string{"PATH=/bin", "OTEL_EXPORTER_OTLP_HEADERS=authorization=x"}, defaults)
	if !strings.Contains(strings.Join(got, "\n"), "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf") {
		t.Errorf("headers alone must not suppress the defaults: %v", got)
	}
}

func clickstackHostOTLPEnvForTest() map[string]string {
	return clickstackHostOTLPEnv(clickstackEntities(map[string]string{"CLICKSTACK_OTLP_HTTP_PORT": "4318"}))
}

// A frontend dev server's /_otel proxy must aim at THIS project's collector,
// the port compose actually published, and the browser must learn the env name.
func TestFrontendDevServerGetsTheCollectorAndTheEnvName(t *testing.T) {
	e := clickstackEntities(map[string]string{"CLICKSTACK_OTLP_HTTP_PORT": "4419"})
	for ftype, envVar := range map[string]string{
		"nextjs": "NEXT_PUBLIC_ENVIRONMENT",
		"vite":   "VITE_ENVIRONMENT",
	} {
		t.Run(ftype, func(t *testing.T) {
			fe := FrontendEntity{Name: "web", Type: ftype}
			got := map[string]string{}
			for _, kv := range withFrontendTelemetryEnv([]string{"PATH=/bin"}, e, fe, "dev") {
				if i := strings.IndexByte(kv, '='); i > 0 {
					got[kv[:i]] = kv[i+1:]
				}
			}
			if got["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:4419" {
				t.Errorf("endpoint = %q, want the published port 4419", got["OTEL_EXPORTER_OTLP_ENDPOINT"])
			}
			if got[envVar] != "dev" {
				t.Errorf("%s = %q, want dev", envVar, got[envVar])
			}
			if _, ok := got["OTEL_EXPORTER_OTLP_PROTOCOL"]; ok {
				t.Error("a frontend dev server is handed the base URL only, not a protocol")
			}
		})
	}

	t.Run("the user's endpoint and an explicit environment win", func(t *testing.T) {
		fe := FrontendEntity{Name: "web", Type: "nextjs"}
		in := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://mine:4318", "NEXT_PUBLIC_ENVIRONMENT=staging"}
		if got := withFrontendTelemetryEnv(in, e, fe, "dev"); !reflect.DeepEqual(got, in) {
			t.Errorf("got %v, want the user's values untouched", got)
		}
	})
	t.Run("no clickstack: only the env name", func(t *testing.T) {
		fe := FrontendEntity{Name: "web", Type: "nextjs"}
		got := strings.Join(withFrontendTelemetryEnv(nil, &KCLEntities{}, fe, "dev"), "\n")
		if strings.Contains(got, "OTEL_EXPORTER_OTLP_ENDPOINT") || !strings.Contains(got, "NEXT_PUBLIC_ENVIRONMENT=dev") {
			t.Errorf("got %q", got)
		}
	})
}

// The command upFrontends actually runs carries both values.
func TestFrontendDevCmdCarriesTelemetryEnv(t *testing.T) {
	fl := frontendLaunch{
		entities: clickstackEntities(map[string]string{"CLICKSTACK_OTLP_HTTP_PORT": "4419"}),
		env:      "dev",
	}
	fe := FrontendEntity{Name: "web", Type: "nextjs", EnvFile: "/does/not/exist"}
	got := strings.Join(buildFrontendDevCmd(context.Background(), fl, fe, nil).Env, "\n")
	for _, want := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4419", "NEXT_PUBLIC_ENVIRONMENT=dev"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

// Docker creates a missing bind-mount source as root, which would leave forge
// unable to write the logs the collector tails.
func TestEnsureClickstackLogDirOnlyWhenTheServiceRuns(t *testing.T) {
	dir := t.TempDir()
	if err := ensureClickstackLogDir(dir, &KCLEntities{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".forge", "logs")); err == nil {
		t.Fatal("created .forge/logs although no clickstack workload is declared")
	}
	if err := ensureClickstackLogDir(dir, clickstackEntities(nil)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".forge", "logs")); err != nil || !fi.IsDir() {
		t.Fatalf(".forge/logs not created: %v", err)
	}
}
