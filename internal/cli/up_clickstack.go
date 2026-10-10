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

// clickstackFrontendOTLPEnv is the collector address a frontend dev server gets:
// the BASE URL only. The dev server's /_otel proxy forwards the browser's
// OTLP/HTTP to it; the wire protocol is not the proxy's to choose.
func clickstackFrontendOTLPEnv(e *KCLEntities) map[string]string {
	host := clickstackHostOTLPEnv(e)
	if host == nil {
		return nil
	}
	return map[string]string{clickstack.EnvOTLPEndpoint: host[clickstack.EnvOTLPEndpoint]}
}

// withFrontendTelemetryEnv gives a frontend dev server what its browser
// telemetry needs from `forge env up`, each unless already set:
//
//   - OTEL_EXPORTER_OTLP_ENDPOINT, this project's collector base URL (the port
//     compose actually published). The dev server's /_otel proxy forwards the
//     browser's OTLP/HTTP there; without it the proxy guesses 127.0.0.1:4318,
//     which after a port step-off is ANOTHER project's ClickStack.
//   - <NEXT_PUBLIC_|VITE_|EXPO_PUBLIC_>ENVIRONMENT, the forge env name, which
//     the browser reports as deployment.environment.name exactly as the
//     backend does. An explicit config.environment already arrives through the
//     frontend's env_vars and is kept.
func withFrontendTelemetryEnv(env []string, e *KCLEntities, fe FrontendEntity, envName string) []string {
	// Only the endpoint is injected, so only the endpoint can be a decision to
	// respect: a protocol exported for some other process is not one.
	env = withDefaultEnv(env, clickstackFrontendOTLPEnv(e))
	return withDefaultEnv(env, map[string]string{frontendEnvironmentEnvVar(fe.Type): envName})
}

// nonEmptyEnvKeys is the set of keys env carries a NON-EMPTY value for. Empty
// counts as unset on purpose: the project-config projection emits one entry per
// config field, so an unset otlp_endpoint arrives as
// OTEL_EXPORTER_OTLP_ENDPOINT="" and must not shadow a default.
func nonEmptyEnvKeys(env []string) map[string]bool {
	have := make(map[string]bool, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[i+1:] != "" {
			have[kv[:i]] = true
		}
	}
	return have
}

// withDefaultEnv adds defaults to env as ONE UNIT: if env already carries a
// non-empty value for any key of defaults, nothing is added. A value the user
// exported, declared in KCL or pinned in config.k always wins, and wins whole.
//
// All-or-nothing is the point. An endpoint and the protocol that speaks to it
// are a pair; filling in only the missing half pairs a user's own endpoint
// with a protocol they never chose (their gRPC :4317 with our http/protobuf),
// and export then fails with nothing on screen saying why.
func withDefaultEnv(env []string, defaults map[string]string) []string {
	have := nonEmptyEnvKeys(env)
	for k := range defaults {
		if have[k] {
			return env
		}
	}
	for _, k := range sortedKeys(defaults) {
		env = withForcedEnv(env, k, defaults[k])
	}
	return env
}

// otlpSignals are the per-signal infixes of OTEL_EXPORTER_OTLP_<SIGNAL>_*.
var otlpSignals = []string{"TRACES", "METRICS", "LOGS"}

// withDefaultOTLPEnv is withDefaultEnv for a process that EXPORTS over OTLP:
// the user having set the endpoint or protocol for any one signal
// (OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, …) is a decision about the transport as
// well, so no default is added beside it.
func withDefaultOTLPEnv(env []string, defaults map[string]string) []string {
	have := nonEmptyEnvKeys(env)
	for _, suffix := range []string{"ENDPOINT", "PROTOCOL"} {
		if have[clickstack.EnvOTLPPrefix+suffix] {
			return env
		}
		for _, s := range otlpSignals {
			if have[clickstack.EnvOTLPPrefix+s+"_"+suffix] {
				return env
			}
		}
	}
	return withDefaultEnv(env, defaults)
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
