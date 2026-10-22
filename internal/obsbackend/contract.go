package obsbackend

import "context"

// Backend is what a `forge obs` verb reads telemetry through. The local
// backend (the project's ClickStack) is the first implementation; the cloud
// backend (the control plane's ObservabilityService) is the second. Both
// compile with pkg/obsquery.
//
//forge:contract
type Backend interface {
	Info() Info
	Logs(ctx context.Context, req LogsRequest) (LogsResult, error)
	Traces(ctx context.Context, req TracesRequest) (TracesResult, error)
	Trace(ctx context.Context, req TraceRequest) (TraceResult, error)
	Endpoints(ctx context.Context, req EndpointsRequest) (EndpointsResult, error)
	MetricsList(ctx context.Context, req MetricsListRequest) (MetricsListResult, error)
	MetricsQuery(ctx context.Context, req MetricsQueryRequest) (MetricsQueryResult, error)
	Query(ctx context.Context, req QueryRequest) (QueryResult, error)
}
