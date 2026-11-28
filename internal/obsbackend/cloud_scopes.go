package obsbackend

// ScopeDeclarer is a control-plane client that can be told, before its first
// call, the scope each procedure needs; *cloud.Client satisfies it.
type ScopeDeclarer interface {
	DeclareScope(scope string, procedures ...string)
}

// DeclareCloudScopes tells c the scope each procedure the cloud backend calls
// needs of a machine credential, so a narrow `forge login` or helper token is
// elevated for it before the first read instead of after a refusal. Every
// read needs obs:read; changing an issue needs obs:triage, which obs:read does
// not imply. The deploy procedures the backend also calls (environments,
// releases) need deploy:read, which the base credential carries, so they are
// not declared.
//
// It also tells a client that takes response limits (ResponseLimitDeclarer)
// the bigger read GetTrace needs (MaxTraceResponseBytes).
//
// Keep it in step with the procedures above: an undeclared one still works
// (the client elevates after one refusal), it just costs that refusal, and a
// wrong declaration falls back to that same reactive path.
//
// Where the control plane checks each scope for a machine credential
// (control-plane, verified 2026-11-10): the reads go through
// authutil.Service.RequireObsRead (observability, activity, telemetry status,
// alerting's gate for obsRead) or obsread's admitMachine without Triage
// (issues, rum), both obs:read; dashboard reads need obs:read
// (dashboard.authorizeMachine); UpdateIssueStatus, AddIssueComment,
// UpdateIssueComment, DeleteIssueComment and AssignIssue admit with Triage
// (issues admitWrite), which is obs:triage.
func DeclareCloudScopes(c ScopeDeclarer) {
	c.DeclareScope("obs:read",
		procSearchLogs, procListTraces, procGetTrace, procListMetrics, procQueryPanel,
		procGetMetric, procListMetricExemplars,
		procGetServiceOverview, procListAlerts, procListAlertHistory, procListTelemetryDeliveries, procListWorkloads,
		procGetReleaseHealth, procListActivity, procGetUserActivity,
		procListIssues, procGetIssue, procListIssueEvents, procListIssueTimeline,
		procListSessions, procGetSession, procGetSessionTimeline,
		procListDashboards, procGetDashboard,
		procExportPGOProfile, procGetFlamegraph,
	)
	c.DeclareScope("obs:triage", procUpdateIssueStatus, procAddIssueComment, procUpdateIssueComment, procDeleteIssueComment, procAssignIssue)
	if d, ok := c.(ResponseLimitDeclarer); ok {
		d.DeclareResponseLimit(MaxTraceResponseBytes, procGetTrace)
	}
}

// ResponseLimitDeclarer is a control-plane client that can be told the most
// bytes of a procedure's reply it reads; *cloud.Client satisfies it.
type ResponseLimitDeclarer interface {
	DeclareResponseLimit(limit int64, procedures ...string)
}

// MaxTraceResponseBytes bounds the GetTrace answer read. A trace is up to
// obsquery.MaxSpansPerTrace spans, each with its span and resource attributes:
// a 10,000-span prod trace measured 17 MiB (about 1.7 KiB a span), well over
// the client's 8 MiB default, which cut it off and failed to decode it
// ("unexpected end of JSON input"). 64 MiB leaves room for spans with
// several times those attributes, and stays a bound.
const MaxTraceResponseBytes int64 = 64 << 20
