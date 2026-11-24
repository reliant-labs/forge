package codegen

import (
	"slices"
	"testing"
)

func TestReadVerb(t *testing.T) {
	for name, want := range map[string]string{
		// the hook generator's read verbs
		"GetOrder": "Get", "ListOrders": "List", "SearchOrders": "Search", "FindSeat": "Find",
		"CheckHealth": "Check", "HasAccess": "Has", "IsEnabled": "Is", "CountOrders": "Count",
		"ExistsOrder": "Exists",
		// the activity interceptor's
		"WatchOrders": "Watch", "QueryUsage": "Query", "DescribeCluster": "Describe",
		"BatchGetOrders": "Get", "BulkListOrders": "List",
		// a bare verb is a whole word
		"Get": "Get", "Get2Orders": "Get",
		// whole words only
		"CheckoutCart": "", "IssueRefund": "", "Listen": "", "Countersign": "", "Hash": "", "Getaway": "",
		// not a read verb at all
		"Health": "", "CreateOrder": "", "Batch": "", "BatchCreate": "",
	} {
		if got := ReadVerb(name); got != want {
			t.Errorf("ReadVerb(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWriteVerb(t *testing.T) {
	for name, want := range map[string]string{
		"CreateOrder": "Create", "UpdateOrder": "Update", "DeleteOrder": "Delete", "SetFlag": "Set",
		"StartRun": "Start", "CancelRun": "Cancel", "SendMessage": "Send", "UpsertRow": "Upsert",
		"BulkDeleteOrders": "Delete", "BatchCreateOrders": "Create", "Run": "Run",
		// whole words only
		"SettingsSummary": "", "Runtime": "", "Addresses": "", "Postal": "",
		// reads and the deliberately ambiguous verbs
		"GetOrder": "", "ValidateOrder": "", "PreviewPromotion": "", "ExportOrders": "", "SyncStatus": "",
	} {
		if got := WriteVerb(name); got != want {
			t.Errorf("WriteVerb(%q) = %q, want %q", name, got, want)
		}
	}
}

// No verb is both: a name WriteVerb claims can never be one ReadVerb asks to
// declare NO_SIDE_EFFECTS, or the two lint rules would contradict.
func TestReadAndWriteVerbsAreDisjoint(t *testing.T) {
	if !slices.IsSorted(rpcWriteVerbs) {
		t.Error("rpcWriteVerbs is not sorted")
	}
	for _, v := range rpcWriteVerbs {
		if r := ReadVerb(v + "Thing"); r != "" {
			t.Errorf("write verb %s is also a read verb (%s)", v, r)
		}
	}
}
