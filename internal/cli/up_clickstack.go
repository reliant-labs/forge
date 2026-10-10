package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/clickstack"
)

// clickstackHostOTLPEnv is the OTel environment every host process gets when
// the env runs the local ClickStack: the standard variables, aimed at the
// OTLP/HTTP port compose published on loopback. Nil when the env has no
// clickstack workload (observability off, or a custom backend).
//
// The port is read from the compose workload's own declared env, the one place
// dev/main.k states it, so the endpoint cannot drift from the published port.
func clickstackHostOTLPEnv(e *KCLEntities) map[string]string {
	if e == nil {
		return nil
	}
	for _, w := range e.WorkloadsOn(RuntimeCompose) {
		if w.Runtime.Compose == nil || composeServiceName(w) != clickstack.Service {
			continue
		}
		port := strings.TrimSpace(w.Runtime.Compose.Env[clickstack.EnvOTLPHTTPPort])
		if port == "" || port == "0" {
			return nil
		}
		return clickstack.HostOTLPEnv(port)
	}
	return nil
}

// withDefaultEnv adds each key of defaults to env unless env already carries a
// NON-EMPTY value for it. Empty counts as unset on purpose: the project-config
// projection emits one entry per config field, so an unset otlp_endpoint
// arrives as OTEL_EXPORTER_OTLP_ENDPOINT="" and must not shadow the default.
// A value the user exported, declared in KCL or pinned in config.k always wins.
func withDefaultEnv(env []string, defaults map[string]string) []string {
	have := make(map[string]bool, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[i+1:] != "" {
			have[kv[:i]] = true
		}
	}
	for _, k := range sortedKeys(defaults) {
		if !have[k] {
			env = withForcedEnv(env, k, defaults[k])
		}
	}
	return env
}

func composeServiceName(w WorkloadEntity) string {
	if w.Runtime.Compose != nil && w.Runtime.Compose.Service != "" {
		return w.Runtime.Compose.Service
	}
	return w.Name
}

// clickstackRowLabel names a clickstack port for the env-up summary. The
// service publishes three, and ":8080" alone does not say which one is the UI.
func clickstackRowLabel(service string, targetPort int) (string, bool) {
	if service != clickstack.Service {
		return "", false
	}
	switch targetPort {
	case 8080:
		return "HyperDX UI", true
	case 4318:
		return "OTLP/HTTP", true
	case 4317:
		return "OTLP/gRPC", true
	}
	return "", false
}

// ensureClickstackLogDir creates .forge/logs before compose starts the
// clickstack service. The service bind-mounts it read-only to tail the host
// processes' logs, and docker creates a missing bind source as ROOT, after
// which forge could not write the very logs the collector is there to read.
func ensureClickstackLogDir(projectDir string, e *KCLEntities) error {
	if e == nil {
		return nil
	}
	for _, w := range e.WorkloadsOn(RuntimeCompose) {
		if w.Runtime.Compose != nil && composeServiceName(w) == clickstack.Service {
			return os.MkdirAll(filepath.Join(projectDir, ".forge", "logs"), 0o755)
		}
	}
	return nil
}
