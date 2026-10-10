package cli

import (
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
