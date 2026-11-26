package accesstoken

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func sortedKeys(m map[string]Scope) []string {
	out := slices.Collect(maps.Keys(m))
	slices.Sort(out)
	return out
}

func TestAppProcedures_GrowthGolden(t *testing.T) {
	want := map[string]Scope{
		"/controlplane.v1.DeployService/ListApps":                 ScopeDeployRead,
		"/controlplane.v1.DeployService/ListEnvironments":         ScopeDeployRead,
		"/controlplane.v1.DeployService/ListPromotions":           ScopeDeployRead,
		"/controlplane.v1.DeployService/ListReleases":             ScopeDeployRead,
		"/controlplane.v1.ObservabilityService/GetReleaseHealth":  ScopeObsRead,
		"/controlplane.v1.ObservabilityService/QueryPanel":        ScopeObsRead,
		"/controlplane.v1.AlertingService/ListAlerts":             ScopeObsRead,
		"/controlplane.v1.IssuesService/ListIssues":               ScopeObsRead,
		"/controlplane.v1.DeliveryInsightsService/ListReleases":   ScopeObsRead,
		"/controlplane.v1.DeliveryInsightsService/GetDoraMetrics": ScopeObsRead,
		"/reliant.v1.TriggerService/FireTrigger":                  ScopeTriggersFire,
	}
	got := AppProcedures("growth")
	if !maps.Equal(got, want) {
		t.Fatalf("growth table drifted.\n got: %v\nwant: %v", sortedKeys(got), sortedKeys(want))
	}
	golden := []string{
		"/controlplane.v1.AlertingService/ListAlerts",
		"/controlplane.v1.DeliveryInsightsService/GetDoraMetrics",
		"/controlplane.v1.DeliveryInsightsService/ListReleases",
		"/controlplane.v1.DeployService/ListApps",
		"/controlplane.v1.DeployService/ListEnvironments",
		"/controlplane.v1.DeployService/ListPromotions",
		"/controlplane.v1.DeployService/ListReleases",
		"/controlplane.v1.IssuesService/ListIssues",
		"/controlplane.v1.ObservabilityService/GetReleaseHealth",
		"/controlplane.v1.ObservabilityService/QueryPanel",
		"/reliant.v1.TriggerService/FireTrigger",
	}
	if !slices.Equal(sortedKeys(got), golden) {
		t.Errorf("sorted growth procedures = %v", sortedKeys(got))
	}
	for p, s := range want {
		if gs, ok := AppProcedureScope("growth", p); !ok || gs != s {
			t.Errorf("AppProcedureScope(growth, %s) = %q, %v; want %q", p, gs, ok, s)
		}
	}
}

func TestAppProcedures_UnknownInputs(t *testing.T) {
	for _, c := range []string{"", "Growth", "growth ", "growth2", "mcp_abc", "app_abc"} {
		if AppProcedures(c) != nil {
			t.Errorf("AppProcedures(%q) is not nil", c)
		}
		if AppConsentCeiling(c) != nil {
			t.Errorf("AppConsentCeiling(%q) is not nil", c)
		}
		if _, ok := AppProcedureScope(c, "/controlplane.v1.DeployService/ListApps"); ok {
			t.Errorf("AppProcedureScope(%q, ListApps) = true", c)
		}
	}
	for _, p := range []string{
		"",
		"/",
		"/controlplane.v1.DeployService/",
		"/controlplane.v1.DeployService/ListApp",
		"/controlplane.v1.DeployService/ListAppsX",
		"/controlplane.v1.DeployService/listapps",
		"controlplane.v1.DeployService/ListApps",
		"/controlplane.v1.DeployService/ListApps/",
		"/controlplane.v1.DeployService/Promote",
		"/controlplane.v1.DeployService/CutRelease",
		"/controlplane.v1.ObservabilityService/SearchLogs",
		"/controlplane.v1.AlertingService/ListAlertHistory",
		"/controlplane.v1.IssuesService/GetIssue",
		"/reliant.v1.TriggerService/FireTriggerX",
		"/reliant.v1.TriggerService/CreateTrigger",
		"/reliant.v1.TriggerService/",
		"/controlplane.v1.TriggerService/FireTrigger",
		"/reliant.v1.EndTenantService/CreateEndTenant",
	} {
		if s, ok := AppProcedureScope("growth", p); ok {
			t.Errorf("AppProcedureScope(growth, %q) = %q, true", p, s)
		}
	}
}

func TestAppProcedures_ReturnsCopy(t *testing.T) {
	m := AppProcedures("growth")
	m["/controlplane.v1.DeployService/Promote"] = ScopeDeployWrite
	delete(m, "/controlplane.v1.DeployService/ListApps")
	if _, ok := AppProcedureScope("growth", "/controlplane.v1.DeployService/Promote"); ok {
		t.Error("mutating the returned map reached the table")
	}
	if _, ok := AppProcedures("growth")["/controlplane.v1.DeployService/ListApps"]; !ok {
		t.Error("deleting from the returned map reached the table")
	}
	c := AppConsentCeiling("growth")
	c[0] = ScopeDeployWrite
	if AppConsentCeiling("growth")[0] == ScopeDeployWrite {
		t.Error("mutating the ceiling reached the table")
	}
}

// C22: the app table reads and fires, and nothing else.
func TestAppProcedures_C22_NoWrites(t *testing.T) {
	allowed := SetOf(ScopeDeployRead, ScopeObsRead, ScopeTriggersFire)
	write := regexp.MustCompile(`^(Create|Update|Delete|Set|Promote|Deploy|Rollback|Revoke)`)
	for p, s := range AppProcedures("growth") {
		if !allowed.Has(s) {
			t.Errorf("%s maps to %s, outside {deploy:read, obs:read, triggers:fire}", p, s)
		}
		method := p[strings.LastIndex(p, "/")+1:]
		if write.MatchString(method) {
			t.Errorf("%s looks like a write", p)
		}
		if s == ScopeTriggersFire && p != "/reliant.v1.TriggerService/FireTrigger" {
			t.Errorf("%s holds triggers:fire", p)
		}
	}
	want := []Scope{ScopeDeployRead, ScopeObsRead, ScopeTriggersFire}
	slices.Sort(want)
	if got := AppConsentCeiling("growth"); !slices.Equal(got, want) {
		t.Errorf("AppConsentCeiling(growth) = %v, want %v", got, want)
	}
}

// The table's scopes never ride the grant: the token is app:oauth alone.
func TestAppProcedures_ScopesNeverInTheGrant(t *testing.T) {
	now := time.Now()
	g := appOAuthGrant()
	if err := g.Validate(now); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	if !g.Scopes.Has(ScopeAppOAuth) || len(g.Scopes) != 1 {
		t.Fatalf("the grant's scopes are %v, want exactly app:oauth", g.Scopes)
	}
	for _, s := range AppConsentCeiling("growth") {
		g := appOAuthGrant()
		g.Scopes = SetOf(ScopeAppOAuth, s)
		if err := g.Validate(now); err == nil {
			t.Errorf("an app:oauth grant carrying %s was accepted", s)
		}
		if appOAuthPrincipalPermits(s) {
			t.Errorf("an app:oauth token permits %s directly", s)
		}
	}
}

func appOAuthPrincipalPermits(s Scope) bool {
	return SetOf(ScopeAppOAuth).Permits(s)
}

// triggers:fire is in the growth table's FireTrigger entry and nowhere else.
func TestTriggersFire_IsolatedToGrowth(t *testing.T) {
	for p, s := range mcpForgeProcedures {
		if s == ScopeTriggersFire {
			t.Errorf("mcp:forge table maps %s to triggers:fire", p)
		}
	}
	for p, s := range endTenantProcedures {
		if s == ScopeTriggersFire {
			t.Errorf("end-tenant table maps %s to triggers:fire", p)
		}
	}
	for client, table := range appProcedures {
		for p, s := range table {
			if s == ScopeTriggersFire && (client != "growth" || p != "/reliant.v1.TriggerService/FireTrigger") {
				t.Errorf("client %s maps %s to triggers:fire", client, p)
			}
		}
	}
}

func TestEndTenantProcedures_Golden(t *testing.T) {
	const svc = "/reliant.v1.EndTenantService/"
	want := map[string]Scope{
		svc + "ConfirmConnectSession":     ScopeConnectionsConnect,
		svc + "CreateConnectSession":      ScopeConnectionsConnect,
		svc + "CreateEndTenant":           ScopeEndTenantsManage,
		svc + "CreateEndTenantActivation": ScopeEndTenantsManage,
		svc + "DeleteEndTenant":           ScopeEndTenantsDelete,
		svc + "DeleteEndTenantConnection": ScopeEndTenantsDelete,
		svc + "DiscardConnectSession":     ScopeConnectionsConnect,
		svc + "GetConnectSession":         ScopeConnectionsConnect,
		svc + "GetEndTenant":              ScopeEndTenantsManage,
		svc + "GetEndTenantRun":           ScopeEndTenantsManage,
		svc + "ListEndTenantConnections":  ScopeEndTenantsManage,
		svc + "ListEndTenantRuns":         ScopeEndTenantsManage,
		svc + "ListEndTenants":            ScopeEndTenantsManage,
	}
	golden := []string{
		svc + "ConfirmConnectSession",
		svc + "CreateConnectSession",
		svc + "CreateEndTenant",
		svc + "CreateEndTenantActivation",
		svc + "DeleteEndTenant",
		svc + "DeleteEndTenantConnection",
		svc + "DiscardConnectSession",
		svc + "GetConnectSession",
		svc + "GetEndTenant",
		svc + "GetEndTenantRun",
		svc + "ListEndTenantConnections",
		svc + "ListEndTenantRuns",
		svc + "ListEndTenants",
	}
	if got := EndTenantProcedures(); !slices.Equal(got, golden) {
		t.Fatalf("EndTenantProcedures() = %v\nwant %v", got, golden)
	}
	for p, s := range want {
		if got, ok := EndTenantProcedureScope(p); !ok || got != s {
			t.Errorf("EndTenantProcedureScope(%s) = %q, %v; want %q", p, got, ok, s)
		}
	}
	if len(endTenantProcedures) != len(want) {
		t.Errorf("table has %d procedures, golden %d", len(endTenantProcedures), len(want))
	}
}

func TestEndTenantProcedures_UnknownAndNearMiss(t *testing.T) {
	for _, p := range []string{
		"",
		"/",
		"/reliant.v1.EndTenantService/",
		"/reliant.v1.EndTenantService",
		"/reliant.v1.EndTenantService/CreateEndTenantX",
		"/reliant.v1.EndTenantService/CreateEndTenant/",
		"/reliant.v1.EndTenantService/createendtenant",
		"/reliant.v1.EndTenantService/CreateConnectSessions",
		"/reliant.v1.EndTenantService/GetEndTenantLimits",
		"/reliant.v1.EndTenantService/SetEndTenantLimits",
		"/reliant.v1.EndTenantServiceX/CreateEndTenant",
		"/reliant.v1.EndTenantService/Unknown",
		"reliant.v1.EndTenantService/CreateEndTenant",
		"/reliant.v1.TriggerService/CreateEndTenant",
		"/controlplane.v1.EndTenantService/CreateEndTenant",
		"/reliant.v1.TriggerService/FireTrigger",
		"/controlplane.v1.DeployService/ListApps",
	} {
		if s, ok := EndTenantProcedureScope(p); ok {
			t.Errorf("EndTenantProcedureScope(%q) = %q, true", p, s)
		}
	}
}

// C21: exact paths, and connections:connect is exactly the four
// connect-session procedures.
func TestEndTenantProcedures_C21(t *testing.T) {
	var connect []string
	for _, p := range EndTenantProcedures() {
		if !strings.HasPrefix(p, "/reliant.v1.EndTenantService/") {
			t.Errorf("%s is outside reliant.v1.EndTenantService", p)
		}
		if s, _ := EndTenantProcedureScope(p); s == ScopeConnectionsConnect {
			connect = append(connect, p)
		}
	}
	want := []string{
		"/reliant.v1.EndTenantService/ConfirmConnectSession",
		"/reliant.v1.EndTenantService/CreateConnectSession",
		"/reliant.v1.EndTenantService/DiscardConnectSession",
		"/reliant.v1.EndTenantService/GetConnectSession",
	}
	if !slices.Equal(connect, want) {
		t.Errorf("connections:connect maps %v, want exactly %v", connect, want)
	}
	// Every procedure a host scope maps to is one of those three scopes, and
	// every host scope is used.
	used := Set{}
	for _, s := range endTenantProcedures {
		used[s] = struct{}{}
	}
	for _, s := range HostCredentialScopes {
		if !used.Has(s) {
			t.Errorf("host scope %s maps no procedure", s)
		}
	}
	// The returned list is a copy.
	l := EndTenantProcedures()
	l[0] = "x"
	if EndTenantProcedures()[0] == "x" {
		t.Error("mutating the list reached the table")
	}
}

// The two shapes' scopes never meet.
func TestProcedureTables_Disjoint(t *testing.T) {
	for p, s := range endTenantProcedures {
		if !IsHostCredentialScope(s) {
			t.Errorf("end-tenant procedure %s maps to non-host scope %s", p, s)
		}
	}
	for client, table := range appProcedures {
		for p, s := range table {
			if IsHostCredentialScope(s) || ClassOf(s) == ClassHost {
				t.Errorf("app client %s procedure %s maps to host scope %s", client, p, s)
			}
			if s == ScopeAppOAuth {
				t.Errorf("app client %s procedure %s maps to app:oauth itself", client, p)
			}
		}
	}
	for _, s := range AppConsentCeiling("growth") {
		if IsHostCredentialScope(s) {
			t.Errorf("consent ceiling holds host scope %s", s)
		}
	}
}

func TestElevatable_RefusesHostClass(t *testing.T) {
	for _, s := range AllScopes {
		if ClassOf(s) == ClassHost && Elevatable(s) {
			t.Errorf("host scope %s is elevatable", s)
		}
	}
}
