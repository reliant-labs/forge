package obsquery

import (
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

var updateUserActivity = flag.Bool("update-user-activity", false, "rewrite testdata/user_activity*.golden.sql")

// golden compares got with testdata/<name>, or rewrites it under
// -update-user-activity. The fragments are shared with the control plane's
// GetUserActivity and the generated dashboards, so a change to them is a
// reviewed diff, not a silent one.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := "testdata/" + name
	if *updateUserActivity {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-user-activity to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s changed (run with -update-user-activity if that is intended):\n%s", path, got)
	}
}

func userActivityQuery() UserActivityQuery {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	return UserActivityQuery{
		Scope: Scope{Database: "t_org", EnvRestricted: true, EnvIDs: []string{"env-prod"}, Services: []string{"api"},
			Start: end.Add(-7 * 24 * time.Hour), End: end},
		SeriesDays: 30, WantSeenTotal: true,
	}
}

func TestUserActivityFragmentsAreGolden(t *testing.T) {
	golden(t, "user_activity_fragments.golden.sql",
		"-- ActiveEvidence\n"+ActiveEvidence+"\n\n-- NewUsersSource\n"+
			NewUsersSource("product_events", "{to:DateTime64(9)}", "(empty({env_ids:Array(String)}) OR has({env_ids:Array(String)}, EnvId))")+"\n")
}

func TestUserActivityStatementIsGolden(t *testing.T) {
	st, err := userActivityQuery().Compile()
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "user_activity.golden.sql", st.SQL+"\n")
	for _, frag := range []string{ActiveEvidence, "max(has_signup) OVER (PARTITION BY EnvId)"} {
		if !strings.Contains(st.SQL, frag) {
			t.Errorf("the statement does not compile from the shared fragment %q", frag)
		}
	}
}

func TestUserActivityWindow(t *testing.T) {
	q := userActivityQuery()
	w := q.window()
	if !w.prevFrom.Equal(q.Scope.Start.Add(-7*24*time.Hour)) || !w.curFrom.Equal(q.Scope.Start) {
		t.Errorf("previous window = [%v, %v)", w.prevFrom, w.curFrom)
	}
	// 30 days ending with the day of End (2026-11-10).
	if got := w.seriesFrom.Format(time.DateOnly); got != "2026-10-12" || !w.readFrom.Equal(w.seriesFrom) {
		t.Errorf("series from %s, read from %v", got, w.readFrom)
	}
	// An End on midnight belongs to the day before it.
	q.Scope.End = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	q.Scope.Start = q.Scope.End.Add(-24 * time.Hour)
	q.SeriesDays = 1
	if got := q.window().seriesFrom.Format(time.DateOnly); got != "2026-11-09" {
		t.Errorf("one-day series from %s, want 2026-11-09", got)
	}
}

func TestUserActivityRefusesWhatItCannotBound(t *testing.T) {
	q := userActivityQuery()
	q.SeriesDays = MaxUserActivitySeriesDays + 1
	if _, err := q.Compile(); err == nil {
		t.Error("a series past the cap compiled")
	}
	q = userActivityQuery()
	q.Scope.Start = q.Scope.End.Add(-MaxRangeUserActivity - time.Hour)
	if _, err := q.Compile(); err == nil {
		t.Error("a range past the cap compiled")
	}
	q = userActivityQuery()
	q.Scope.Database = "default"
	if _, err := q.Compile(); err == nil {
		t.Error("an org scope on the server's default database compiled")
	}
}

func TestUserActivityWithoutSeenTotalReadsNoRetention(t *testing.T) {
	q := userActivityQuery()
	q.WantSeenTotal = false
	st, err := q.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(st.SQL, "'seen'") {
		t.Error("the retention-wide read ran without WantSeenTotal")
	}
}

func TestUserFilterPredicate(t *testing.T) {
	const scope = "has(['e1'], EnvId)"
	for _, tc := range []struct {
		name   string
		filter UserFilter
		want   string
	}{
		{"zero leaves nothing more out", UserFilter{}, ""},
		{"system events, sorted and deduplicated", UserFilter{SystemEvents: []string{"b", "a", "b", ""}},
			"EventName NOT IN ('a', 'b')"},
		{"staff by marker event, in the read's environments", UserFilter{StaffEvents: []string{"internal_user"}},
			"(EnvId, UserId) NOT IN (SELECT EnvId, UserId FROM t.product_events WHERE EventName IN ('internal_user') AND UserId != '' AND " + scope + ")"},
		{"staff by id, quoted", UserFilter{StaffUserIDs: []string{"o'brien", "c1"}},
			`UserId NOT IN ('c1', 'o\'brien')`},
		{"all three", UserFilter{SystemEvents: []string{"billing_state"}, StaffEvents: []string{"internal_user"}, StaffUserIDs: []string{"c1"}},
			"EventName NOT IN ('billing_state') AND (EnvId, UserId) NOT IN (SELECT EnvId, UserId FROM t.product_events WHERE EventName IN ('internal_user') AND UserId != '' AND " + scope + ") AND UserId NOT IN ('c1')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Predicate("t.product_events", scope); got != tc.want {
				t.Errorf("Predicate =\n%s\nwant\n%s", got, tc.want)
			}
			if tc.filter.IsZero() != (tc.want == "") {
				t.Errorf("IsZero = %v", tc.filter.IsZero())
			}
		})
	}
}

func TestUserActivityAppliesTheFilterEverywhere(t *testing.T) {
	q := userActivityQuery()
	plain, err := q.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain.SQL, NotSystemActivity) {
		t.Errorf("the default statement does not leave out marked events:\n%s", plain.SQL)
	}
	q.Filter = UserFilter{SystemEvents: []string{"billing_state"}, StaffUserIDs: []string{"c1"}}
	st, err := q.Compile()
	if err != nil {
		t.Fatal(err)
	}
	// act, nu_cur, nu_prev and seen each read the table once, and each
	// leaves the declared rows out.
	if n, reads := strings.Count(st.SQL, "EventName NOT IN ('billing_state') AND UserId NOT IN ('c1')"), strings.Count(st.SQL, "FROM t_org.product_events"); n != reads || n != 4 {
		t.Errorf("the filter is applied to %d of %d reads, want 4 of 4:\n%s", n, reads, st.SQL)
	}
}
