// File: otelenv.go — the one place pkg/observe reads the process environment.
//
// The OpenTelemetry environment variables are the contract between a forge app
// and whatever collector the platform runs: an app never learns the backend, the
// platform renders OTEL_* and the runtime obeys. The exporters read most of them
// themselves (headers, timeout, compression, TLS material, the per-signal
// endpoint variants); this file only reads what they cannot decide:
//
//   - which signals are exported at all (an endpoint is present, the SDK is not
//     disabled, the signal's exporter is not "none"),
//   - which wire protocol to build (the SDK has one package per protocol),
//   - whether the opt-in OTLP logs exporter is on.
//
// pkg/.golangci.yml exempts exactly this file from the os.Getenv rule.

package observe

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Environment variable names of the contract. Exported so docs, doctor checks
// and renderers can refer to one spelling.
const (
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPProtocol = "OTEL_EXPORTER_OTLP_PROTOCOL"
	EnvOTLPHeaders  = "OTEL_EXPORTER_OTLP_HEADERS"

	// EnvLogsExporter is the explicit switch for the OTLP logs exporter. The
	// OpenTelemetry specification defaults it to "otlp"; forge defaults it OFF
	// because logs already have a shipping path (stdout/file collection), and
	// only the value "otlp" turns the exporter on.
	EnvLogsExporter = "OTEL_LOGS_EXPORTER"

	// EnvSDKDisabled turns every export off. Context propagation stays on.
	EnvSDKDisabled = "OTEL_SDK_DISABLED"
)

const (
	// ProtocolHTTPProtobuf is OTLP over HTTP with protobuf bodies (port 4318).
	// It is the default.
	ProtocolHTTPProtobuf = "http/protobuf"
	// ProtocolGRPC is OTLP over gRPC (port 4317).
	ProtocolGRPC = "grpc"
)

// signal names an OTLP signal as it appears in OTEL_EXPORTER_OTLP_<SIGNAL>_*.
type signal string

const (
	signalTraces  signal = "TRACES"
	signalMetrics signal = "METRICS"
	signalLogs    signal = "LOGS"
)

func envValue(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func (s signal) env(suffix string) string {
	return envValue("OTEL_EXPORTER_OTLP_" + string(s) + "_" + suffix)
}

// sdkDisabled reports OTEL_SDK_DISABLED=true.
func sdkDisabled() bool { return strings.EqualFold(envValue(EnvSDKDisabled), "true") }

// logsExportRequested reports whether OTEL_LOGS_EXPORTER asks for OTLP.
func logsExportRequested() bool {
	for _, v := range strings.Split(envValue(EnvLogsExporter), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "otlp") {
			return true
		}
	}
	return false
}

// exportEnabled reports whether signal s is exported. With no endpoint from any
// source (Config.OTLPEndpoint, OTEL_EXPORTER_OTLP_ENDPOINT or the per-signal
// variable) there is nothing to send to, and export stays off.
func exportEnabled(s signal, cfg Config) bool {
	if sdkDisabled() {
		return false
	}
	switch s {
	case signalLogs:
		if !cfg.OTLPLogs && !logsExportRequested() {
			return false
		}
	default:
		if strings.EqualFold(envValue("OTEL_"+string(s)+"_EXPORTER"), "none") {
			return false
		}
	}
	return cfg.OTLPEndpoint != "" || envValue(EnvOTLPEndpoint) != "" || s.env("ENDPOINT") != ""
}

// protocolFor resolves the wire protocol of signal s: the per-signal variable,
// then OTEL_EXPORTER_OTLP_PROTOCOL, then http/protobuf.
func protocolFor(s signal) (string, error) {
	p := strings.ToLower(s.env("PROTOCOL"))
	if p == "" {
		p = strings.ToLower(envValue(EnvOTLPProtocol))
	}
	switch p {
	case "", ProtocolHTTPProtobuf:
		return ProtocolHTTPProtobuf, nil
	case ProtocolGRPC:
		return ProtocolGRPC, nil
	default:
		return "", fmt.Errorf("unsupported OTLP protocol %q for %s (want %q or %q)", p, strings.ToLower(string(s)), ProtocolHTTPProtobuf, ProtocolGRPC)
	}
}

// typedEndpointURL turns the caller's Config.OTLPEndpoint into the URL handed
// to an exporter's WithEndpointURL. A bare "host:port" gains an http:// scheme.
// For HTTP the value has base-endpoint semantics, exactly like
// OTEL_EXPORTER_OTLP_ENDPOINT, so the signal path (/v1/traces, ...) is appended.
func typedEndpointURL(endpoint, protocol string, s signal) (string, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid OTLP endpoint %q: %w", endpoint, err)
	}
	if protocol == ProtocolHTTPProtobuf {
		u.Path = strings.TrimRight(u.Path, "/") + "/v1/" + strings.ToLower(string(s))
	}
	return u.String(), nil
}
