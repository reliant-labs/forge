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

// HealthReader judges a release: the windows before and after a promotion,
// per service, and what the release opened (`forge obs check`). The local
// backend judges from the ClickStack with pkg/releasehealth; the cloud
// backend asks the control plane's GetReleaseHealth, which applies the same
// rule.
//
//forge:contract
type HealthReader interface {
	ReleaseHealth(ctx context.Context, req HealthRequest) (HealthResult, error)
}

// ChangesReader lists what changed in an env: promotions, failed deploys and
// activity (`forge obs changes`).
//
//forge:contract
type ChangesReader interface {
	Changes(ctx context.Context, req ChangesRequest) (ChangesResult, error)
}

// IssueReader is what `forge obs issues|issue` read and write through. Both
// backends implement it. The local one groups ERROR logs at query time and
// keeps no state, so its UpdateIssueStatus is always a refusal; the cloud one
// is the control plane's IssuesService.
//
//forge:contract
type IssueReader interface {
	ListIssues(ctx context.Context, req ListIssuesRequest) (ListIssuesResult, error)
	GetIssue(ctx context.Context, req GetIssueRequest) (GetIssueResult, error)
	ListIssueEvents(ctx context.Context, req ListIssueEventsRequest) (ListIssueEventsResult, error)
	UpdateIssueStatus(ctx context.Context, req UpdateIssueStatusRequest) (UpdateIssueStatusResult, error)
}
