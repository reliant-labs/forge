// Package clickstack names the facts about the local ClickStack compose
// service that `forge env up`, the doctor checks and the integration gate all
// have to agree on, so none of them learns a name from another's source.
package clickstack

import "strings"

const (
	// Service is the compose service (and the workload name in dev/main.k).
	Service = "clickstack"

	// Port variables are declared once in deploy/kcl/<env>/main.k, handed to
	// compose through OnCompose.env, and read back by `forge env up` to build
	// the endpoint host processes export to.
	EnvOTLPHTTPPort = "CLICKSTACK_OTLP_HTTP_PORT"
	EnvOTLPGRPCPort = "CLICKSTACK_OTLP_GRPC_PORT"
	EnvUIPort       = "CLICKSTACK_UI_PORT"

	// The standard OTel variables a process is handed.
	EnvOTLPPrefix   = "OTEL_EXPORTER_OTLP_"
	EnvOTLPEndpoint = EnvOTLPPrefix + "ENDPOINT"
	EnvOTLPProtocol = EnvOTLPPrefix + "PROTOCOL"

	// DashboardsDir is the project-relative directory mounted as HyperDX's
	// DASHBOARD_PROVISIONER_DIR. A HyperDX v2 dashboard JSON dropped here is
	// loaded without a restart.
	DashboardsDir = "deploy/observability/dashboards"

	// CollectorConfig is the project-relative collector pipeline file.
	CollectorConfig = "deploy/observability/otel-collector.yaml"
)

// HostOTLPEnv is the OTel environment a host process gets so its traces and
// metrics reach the local collector with no further configuration. Apps never
// learn the backend: this is the whole contract.
func HostOTLPEnv(httpPort string) map[string]string {
	return map[string]string{
		EnvOTLPEndpoint: "http://127.0.0.1:" + strings.TrimSpace(httpPort),
		EnvOTLPProtocol: "http/protobuf",
	}
}
