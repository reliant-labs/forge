package accesstoken

import "slices"

// ── What the host credential may call ────────────────────────────────
//
// The end-tenant host credential (host_credential.go) holds endtenants:manage,
// endtenants:delete and connections:connect. Reliant's EndTenantService maps
// each of its procedures to exactly one of them here. The map is EXACT-PATH
// ONLY, with no prefix or service-wide matching (SR0 C21): a method added to
// the service is refused to a host credential until it is named here, and
// reliant sweeps its service descriptor against EndTenantProcedures to prove
// every method is either mapped or deliberately unmapped.
//
// connections:connect maps exactly the four connect-session procedures.
//
// Not yet mapped: Get/SetEndTenantLimits (R6-11) are added when they land.

const endTenantService = "/reliant.v1.EndTenantService/"

var endTenantProcedures = map[string]Scope{
	endTenantService + "CreateEndTenant":           ScopeEndTenantsManage,
	endTenantService + "GetEndTenant":              ScopeEndTenantsManage,
	endTenantService + "ListEndTenants":            ScopeEndTenantsManage,
	endTenantService + "ListEndTenantConnections":  ScopeEndTenantsManage,
	endTenantService + "CreateEndTenantActivation": ScopeEndTenantsManage,
	endTenantService + "GetEndTenantRun":           ScopeEndTenantsManage,
	endTenantService + "ListEndTenantRuns":         ScopeEndTenantsManage,

	endTenantService + "DeleteEndTenant":           ScopeEndTenantsDelete,
	endTenantService + "DeleteEndTenantConnection": ScopeEndTenantsDelete,

	endTenantService + "CreateConnectSession":  ScopeConnectionsConnect,
	endTenantService + "GetConnectSession":     ScopeConnectionsConnect,
	endTenantService + "ConfirmConnectSession": ScopeConnectionsConnect,
	endTenantService + "DiscardConnectSession": ScopeConnectionsConnect,
}

// EndTenantProcedureScope is the host-credential scope procedure needs, and
// false for any procedure not named exactly. procedure is a Connect
// procedure path: "/reliant.v1.EndTenantService/CreateEndTenant".
func EndTenantProcedureScope(procedure string) (Scope, bool) {
	s, ok := endTenantProcedures[procedure]
	return s, ok
}

// EndTenantProcedures lists the table's procedures, sorted.
func EndTenantProcedures() []string {
	out := make([]string, 0, len(endTenantProcedures))
	for p := range endTenantProcedures {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}
