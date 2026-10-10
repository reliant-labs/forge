"""
Positive test — a gRPC-port OTLP endpoint is never rendered without its
protocol. The runtime defaults to http/protobuf (:4318); an endpoint on the
gRPC port (:4317) with no OTEL_EXPORTER_OTLP_PROTOCOL=grpc exports nothing,
silently. forge.OTEL_ENV is the default a project splices into a workload.
"""

import forge

_env = {e.name: e.value for e in forge.OTEL_ENV if e.value}

assert_default_endpoint_unchanged = _env.OTEL_EXPORTER_OTLP_ENDPOINT == "http://otel-collector.observability:4317"
assert_grpc_endpoint_carries_protocol = _env.OTEL_EXPORTER_OTLP_PROTOCOL == "grpc"
