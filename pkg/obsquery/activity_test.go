package obsquery

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestActiveEvidenceForWithoutRulesIsTheConvention(t *testing.T) {
	if got := ActiveEvidenceFor("product_events", Activity{}); got != ActiveEvidence {
		t.Errorf("ActiveEvidenceFor(zero) = %q, want ActiveEvidence", got)
	}
	if !strings.Contains(ActiveEvidence, "Props['forge.kind'] != 'state'") {
		t.Errorf("ActiveEvidence %q does not leave observe.TrackState events out", ActiveEvidence)
	}
	if got := NewUsersSourceFor("product_events", "{to:DateTime64(9)}", "", Activity{}); got != NewUsersSource("product_events", "{to:DateTime64(9)}", "") {
		t.Error("NewUsersSourceFor(zero) differs from NewUsersSource")
	}
}

func TestActiveEvidenceForCompilesTheRules(t *testing.T) {
	a := Activity{NotActivity: []string{"billing_subscription_state", "claimable_account_created"}, ExcludeUsersWith: []string{"internal_user"}}
	got := ActiveEvidenceFor("t_org.product_events", a)
	for _, want := range []string{
		ActiveEvidence + " AND ",
		"EventName NOT IN ('billing_subscription_state', 'claimable_account_created')",
		"(EnvId, UserId) NOT IN (SELECT EnvId, UserId FROM t_org.product_events WHERE EventName IN ('internal_user') AND UserId != '')",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ActiveEvidenceFor = %q, want it to contain %q", got, want)
		}
	}
	src := NewUsersSourceFor("t_org.product_events", "{to:DateTime64(9)}", "", a)
	if !strings.Contains(src, "WHERE "+got+" AND Timestamp") {
		t.Errorf("NewUsersSourceFor does not read the rules' evidence:\n%s", src)
	}
}

func TestActivityRefusesNamesThatAreNotEvents(t *testing.T) {
	bad := Activity{NotActivity: []string{"ok_event", "x') OR 1=1 --"}}
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("Validate(%v) = %v, want ErrInvalid", bad, err)
	}
	// Unvalidated, the bad name never reaches the SQL.
	if got := ActiveEvidenceFor("product_events", bad); strings.Contains(got, "1=1") || !strings.Contains(got, "'ok_event'") {
		t.Errorf("ActiveEvidenceFor(unvalidated) = %q", got)
	}
	if got := ActiveEvidenceFor("product_events", Activity{ExcludeUsersWith: []string{"'"}}); got != ActiveEvidence {
		t.Errorf("a rule with no valid name must add no clause: %q", got)
	}
	long := Activity{ExcludeUsersWith: make([]string, MaxActivityEvents+1)}
	for i := range long.ExcludeUsersWith {
		long.ExcludeUsersWith[i] = "e"
	}
	if err := long.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("Validate(%d names) = %v, want ErrInvalid", len(long.ExcludeUsersWith), err)
	}
}

func TestActivityMergeIsTheSortedUnion(t *testing.T) {
	got := Activity{NotActivity: []string{"b", "a"}}.Merge(Activity{NotActivity: []string{"a", "c"}, ExcludeUsersWith: []string{"internal_user", ""}})
	want := Activity{NotActivity: []string{"a", "b", "c"}, ExcludeUsersWith: []string{"internal_user"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %+v, want %+v", got, want)
	}
	if !(Activity{}).Merge(Activity{}).IsZero() {
		t.Error("the union of no rules has rules")
	}
}

func TestUserActivityCompilesTheDeclaredRules(t *testing.T) {
	q := userActivityQuery()
	q.Activity = Activity{NotActivity: []string{"billing_subscription_state"}, ExcludeUsersWith: []string{"internal_user"}}
	st, err := q.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(st.SQL, "EventName IN ('internal_user')"); n != 4 {
		t.Errorf("the exclusion appears %d times, want 4 (act, seen, nu_cur, nu_prev):\n%s", n, st.SQL)
	}
	q.Activity = Activity{NotActivity: []string{"no quotes'"}}
	if _, err := q.Compile(); !errors.Is(err, ErrInvalid) {
		t.Errorf("Compile with a bad rule = %v, want ErrInvalid", err)
	}
}
