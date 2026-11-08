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
// Keep it in step with the procedures above: an undeclared one still works
// (the client elevates after one refusal), it just costs that refusal.
func DeclareCloudScopes(c ScopeDeclarer) {
	c.DeclareScope("obs:read",
		procSearchLogs, procListTraces, procGetTrace, procListMetrics, procQueryPanel,
		procGetMetric, procListMetricExemplars,
		procGetServiceOverview, procListAlerts, procListTelemetryDeliveries, procListWorkloads,
		procGetReleaseHealth, procListActivity,
		procListIssues, procGetIssue, procListIssueEvents, procListIssueTimeline,
		procListSessions, procGetSession, procGetSessionTimeline,
		procListDashboards, procGetDashboard,
	)
	c.DeclareScope("obs:triage", procUpdateIssueStatus, procAddIssueComment, procAssignIssue)
}
