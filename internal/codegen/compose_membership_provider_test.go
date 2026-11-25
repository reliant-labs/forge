package codegen

import "testing"

// An internal/app provider that returns a contract package's own type
// supplies that package: the package is not also made a node, which would give
// the type two sources (forge bug 811f1d28).
func TestProviderSuppliedComponentsProviderWins(t *testing.T) {
	const module = "example.com/x"
	comps := []BuildComponent{
		{FieldName: "OrgMove", Alias: "orgmove", ImportPath: "internal/orgmove", compRoleRoot: "internal"},
		{FieldName: "Billing", Alias: "billing", ImportPath: "internal/billing", compRoleRoot: "internal", compFieldType: "*billing.Service"},
		{FieldName: "Audit", Alias: "audit", ImportPath: "internal/audit", compRoleRoot: "internal"},
		{FieldName: "Tools", Alias: "tools", ImportPath: "internal/handlers/tools", compRoleRoot: "internal/handlers"},
	}
	providers := []graphProvider{
		{Name: "OrgMove", Result: "orgmove.Service", Optional: true, Imports: map[string]string{"orgmove": module + "/internal/orgmove"}},
		{Name: "Billing", Result: "*bill.Service", Imports: map[string]string{"bill": module + "/internal/billing"}},
		// A wrapper over audit is another type: audit stays a node.
		{Name: "AuditView", Result: "auditview.View", Imports: map[string]string{"auditview": module + "/internal/audit"}},
		{Name: "Tools", Result: "tools.Service", Imports: map[string]string{"tools": module + "/internal/handlers/tools"}},
	}
	kept, supplied := providerSuppliedComponents(module, comps, providers)
	var names []string
	for _, c := range kept {
		names = append(names, c.FieldName)
	}
	if len(names) != 2 || names[0] != "Audit" || names[1] != "Tools" {
		t.Fatalf("kept = %v, want [Audit Tools]", names)
	}
	if supplied["internal/orgmove"] != "OrgMove" || supplied["internal/billing"] != "Billing" || len(supplied) != 2 {
		t.Fatalf("suppliedBy = %v", supplied)
	}
	if kept, supplied := providerSuppliedComponents(module, comps, nil); len(kept) != len(comps) || supplied != nil {
		t.Fatalf("no providers changes nothing, got %d kept, %v", len(kept), supplied)
	}
}
