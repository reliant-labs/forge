package accesstoken

import (
	"maps"
	"slices"
)

// ── What an app:oauth token's client may call ────────────────────────
//
// An app:oauth token (ScopeAppOAuth) is one whole credential that holds no
// product scope: it names the OAuth client it was issued to and the app
// grant the person consented to, and nothing else. What the client may call
// is a per-client table of Connect procedures, each with the one scope it
// runs under, exactly as mcp_forge.go does for mcp:forge. The scopes appear
// ONLY here, never in the token: a grant that carries app:oauth plus any of
// them is refused (Grant.Validate).
//
// A control plane admits an app:oauth call on a procedure only when the
// client's table names it, the person holds the table's scope in the org
// right now, and the app grant (the consent, bounded by AppConsentCeiling)
// covers it. It then runs the call holding THAT scope alone.

// AppClientGrowth is the client id of the Growth app.
const AppClientGrowth = "growth"

// appProcedures is every app client's table: client id -> procedure path ->
// the scope the call runs with. Never returned by reference.
var appProcedures = map[string]map[string]Scope{
	AppClientGrowth: {
		// deploy:read: the release timeline.
		"/controlplane.v1.DeployService/ListApps":         ScopeDeployRead,
		"/controlplane.v1.DeployService/ListEnvironments": ScopeDeployRead,
		"/controlplane.v1.DeployService/ListReleases":     ScopeDeployRead,
		"/controlplane.v1.DeployService/ListPromotions":   ScopeDeployRead,

		// obs:read: release health, alerts, issues and delivery metrics.
		// SR0 C20: the control plane must refuse QueryPanel for an app:oauth
		// token outside an org allowlist until A-6 lands.
		"/controlplane.v1.ObservabilityService/GetReleaseHealth":  ScopeObsRead,
		"/controlplane.v1.ObservabilityService/QueryPanel":        ScopeObsRead,
		"/controlplane.v1.AlertingService/ListAlerts":             ScopeObsRead,
		"/controlplane.v1.IssuesService/ListIssues":               ScopeObsRead,
		"/controlplane.v1.DeliveryInsightsService/ListReleases":   ScopeObsRead,
		"/controlplane.v1.DeliveryInsightsService/GetDoraMetrics": ScopeObsRead,

		// triggers:fire: the one write, and the only table that holds it.
		"/reliant.v1.TriggerService/FireTrigger": ScopeTriggersFire,
	},
}

// AppProcedures returns a copy of clientID's table (procedure path -> scope),
// or nil for a client with none, whose app:oauth tokens a control plane
// refuses on every procedure.
func AppProcedures(clientID string) map[string]Scope {
	t, ok := appProcedures[clientID]
	if !ok {
		return nil
	}
	return maps.Clone(t)
}

// AppProcedureScope is the scope an app:oauth token of clientID runs
// procedure with, and false for an unknown client or a procedure its table
// does not name. procedure is a Connect procedure path:
// "/controlplane.v1.DeployService/ListReleases".
func AppProcedureScope(clientID, procedure string) (Scope, bool) {
	s, ok := appProcedures[clientID][procedure]
	return s, ok
}

// AppConsentCeiling is the distinct scopes of clientID's table, sorted: the
// most an app grant for the client may ever consent to. Nil for an unknown
// client.
func AppConsentCeiling(clientID string) []Scope {
	t, ok := appProcedures[clientID]
	if !ok {
		return nil
	}
	seen := map[Scope]struct{}{}
	for _, s := range t {
		seen[s] = struct{}{}
	}
	out := make([]Scope, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}
