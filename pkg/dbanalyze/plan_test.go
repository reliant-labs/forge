package dbanalyze

import "testing"

// A table read in full under a join is unserved even when the catalog holds
// an index leading with the join column: the plan, not the catalog, says
// whether an index serves the join.
func TestClassifyJoinReadInFullIsUnservedDespiteAnIndexOnTheJoinKey(t *testing.T) {
	plan := []byte(`[{"Plan": {"Node Type": "Hash Join", "Hash Cond": "(o.customer_id = c.id)", "Plans": [
	  {"Node Type": "Seq Scan", "Relation Name": "orders", "Schema": "public", "Alias": "o", "Disabled": true},
	  {"Node Type": "Hash", "Plans": [
	    {"Node Type": "Index Scan", "Relation Name": "customers", "Schema": "public", "Alias": "c",
	     "Index Name": "customers_org", "Index Cond": "(c.org_id = $1)"}]}]}}]`)
	root, err := parsePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	rels := map[string]*relInfo{
		"orders":    {table: "orders", indexes: map[string]indexInfo{"orders_customer": {}}},
		"customers": {table: "customers", indexes: map[string]indexInfo{"customers_org": {}}},
	}
	got, err := classify(root, root, func(_, name string) (*relInfo, error) { return rels[name], nil })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"orders": AccessUnserved + "/" + UnservedJoin, "customers": AccessServed + "/" + ServedByIndexCond}
	if len(got) != len(want) {
		t.Fatalf("access = %+v, want %v", got, want)
	}
	for _, c := range got {
		if v := c.Verdict + "/" + c.How; v != want[c.Table] {
			t.Errorf("%s: %s, want %s", c.Table, v, want[c.Table])
		}
	}
}
