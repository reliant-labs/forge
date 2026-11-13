package obsbuiltin

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/dashspec"
)

// userFilterSeed is one environment, e1, over the 7 days before 2026-03-16
// 12:00 UTC:
//
//	u1      signs up (password) and signs in on 03-10: a person
//	owner   only billing_state, one a day, stored before anyone marked it:
//	        history, which only a declared system event corrects
//	owner2  billing_state marked by its emitter (forge.activity = system)
//	staff   signs up on 03-11 and carries the app's internal_user marker
//	canary  a synthetic monitor: password sign-ins on 03-12 and 03-13,
//	        staff by its id
//
// and two account snapshots a day: the managed identity's (no provider:
// 3) and the app's own provider's (supabase: 110, then 116).
func userFilterSeed(db string) string {
	rows := []string{
		"('2026-03-10 09:00:00', 'api', 'e1', 'u1', 'auth_signed_up', {'method': 'password', 'provider': 'forge'}, 'backend')",
		"('2026-03-10 09:00:01', 'api', 'e1', 'u1', 'auth_signed_in', {'kind': 'credential', 'method': 'password'}, 'backend')",
		"('2026-03-11 00:10:00', 'api', 'e1', 'staff', 'internal_user', {}, 'backend')",
		"('2026-03-11 10:00:00', 'api', 'e1', 'staff', 'auth_signed_up', {'method': 'password', 'provider': 'forge'}, 'backend')",
		"('2026-03-12 10:00:00', 'api', 'e1', 'canary', 'auth_signed_in', {'kind': 'credential', 'method': 'password'}, 'backend')",
		"('2026-03-13 10:00:00', 'api', 'e1', 'canary', 'auth_signed_in', {'kind': 'credential', 'method': 'password'}, 'backend')",
		"('2026-03-14 00:10:00', 'platform', 'e1', '', 'auth_accounts', {'total': '3'}, 'platform')",
		"('2026-03-14 00:20:00', 'api', 'e1', '', 'auth_accounts', {'total': '110', 'provider': 'supabase'}, 'backend')",
		"('2026-03-15 00:10:00', 'platform', 'e1', '', 'auth_accounts', {'total': '3'}, 'platform')",
		"('2026-03-15 00:20:00', 'api', 'e1', '', 'auth_accounts', {'total': '116', 'provider': 'supabase'}, 'backend')",
	}
	for d := 10; d <= 15; d++ {
		rows = append(rows,
			fmt.Sprintf("('2026-03-%02d 10:00:00', 'billing', 'e1', 'owner', 'billing_state', {'plan': 'pro'}, 'backend')", d),
			fmt.Sprintf("('2026-03-%02d 10:00:00', 'billing', 'e1', 'owner2', 'billing_state', {'plan': 'pro', 'forge.activity': 'system'}, 'backend')", d))
	}
	return "INSERT INTO " + db + ".product_events (Timestamp, ServiceName, EnvId, UserId, EventName, Props, Client) VALUES\n    " +
		strings.Join(rows, ",\n    ") + ";\n"
}

// The declared system events and staff, and the emitters' marker, on every
// panel that counts users, against one population.
func TestDeclaredSystemEventsAndStaffAreLeftOut(t *testing.T) {
	const db = "forge_obsbuiltin_user_filter"
	ch := newClickHouse(t, db, userFilterSeed(db))
	end := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := end.Add(-7 * 24 * time.Hour)

	type want struct {
		active, new, returning int // Users and the Overview's 7-day figures
		mostActive             int // rows of Most active users
		signins                int // events on Sign-ins by method
		signups                int // Acquisition's Sign-ups
	}
	system := []string{"billing_state"}
	for _, tc := range []struct {
		name      string
		analytics *Analytics
		want      want
	}{
		// u1 owner staff canary; owner2's events are marked.
		{"defaults: only the marked events are left out", nil, want{4, 2, 2, 4, 3, 2}},
		{"a declared system event corrects history", &Analytics{SystemEvents: system}, want{3, 2, 1, 3, 3, 2}},
		{"staff by marker event", &Analytics{SystemEvents: system, StaffEvents: []string{"internal_user"}}, want{2, 1, 1, 2, 3, 1}},
		{"staff by id", &Analytics{SystemEvents: system, StaffEvents: []string{"internal_user"}, StaffUserIDs: []string{"canary"}}, want{1, 1, 0, 1, 1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Project: "shop", Web: true, Analytics: tc.analytics}
			users, acq := Users(in), Acquisition(in)
			overview := ExtendOverview(dashspec.Dashboard{ID: "overview"}, in)
			count := func(d dashspec.Dashboard, id string) int {
				t.Helper()
				// A figure's first column; the Overview's figures add the
				// previous 7 days after it.
				got := strings.SplitN(ch.run(panel(t, d, id).SQL, start, end, "e1"), "\t", 2)[0]
				n, err := strconv.Atoi(got)
				if err != nil {
					t.Fatalf("%s/%s: %q is not a count", d.ID, id, got)
				}
				return n
			}
			for _, f := range []struct {
				d    dashspec.Dashboard
				id   string
				want int
			}{
				{users, "active", tc.want.active}, {users, "new", tc.want.new}, {users, "returning", tc.want.returning},
				{overview, "active-7d", tc.want.active}, {overview, "new-7d", tc.want.new}, {overview, "returning-7d", tc.want.returning},
				{acq, "signups", tc.want.signups},
				// The newest snapshot of each provider, summed; no
				// declaration changes it.
				{users, "accounts", 3 + 116}, {overview, "accounts", 3 + 116},
			} {
				if got := count(f.d, f.id); got != f.want {
					t.Errorf("%s/%s = %d, want %d", f.d.ID, f.id, got, f.want)
				}
			}
			if rows := ch.run(panel(t, users, "most-active-users").SQL, start, end, "e1"); len(strings.Split(rows, "\n")) != tc.want.mostActive {
				t.Errorf("most active users:\n%s\nwant %d rows", rows, tc.want.mostActive)
			}
			signins := 0
			for _, row := range strings.Split(ch.run(panel(t, users, "signins-by-method").SQL, start, end, "e1"), "\n") {
				cols := strings.Split(row, "\t")
				n, _ := strconv.Atoi(cols[len(cols)-1])
				signins += n
			}
			if signins != tc.want.signins {
				t.Errorf("sign-ins by method = %d events, want %d", signins, tc.want.signins)
			}
			// The DAU/WAU/MAU series' last day: its monthly column is
			// every active user of the 30 days before it.
			series := strings.Split(ch.run(panel(t, users, "dau-wau-mau").SQL, start, end, "e1"), "\n")
			last := strings.Split(series[len(series)-1], "\t")
			if mau, _ := strconv.Atoi(last[len(last)-1]); mau != tc.want.active {
				t.Errorf("monthly active on %s = %d, want %d", last[0], mau, tc.want.active)
			}
		})
	}
}

// A staff marker marks its user in the environment it was emitted in, never
// the same id in another: "u" is staff in e1 (its internal_user marker) and a
// customer in e2, where the same opaque id is someone else.
func TestStaffMarkerIsScopedToItsEnvironment(t *testing.T) {
	const db = "forge_obsbuiltin_staff_env"
	seed := "INSERT INTO " + db + ".product_events (Timestamp, ServiceName, EnvId, UserId, EventName, Props, Client) VALUES\n    " +
		strings.Join([]string{
			"('2026-03-11 00:10:00', 'api', 'e1', 'u', 'internal_user', {}, 'backend')",
			"('2026-03-11 10:00:00', 'api', 'e1', 'u', 'auth_signed_in', {'kind': 'credential', 'method': 'password'}, 'backend')",
			"('2026-03-12 10:00:00', 'api', 'e2', 'u', 'auth_signed_in', {'kind': 'credential', 'method': 'password'}, 'backend')",
		}, ",\n    ") + ";\n"
	ch := newClickHouse(t, db, seed)
	end := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := end.Add(-7 * 24 * time.Hour)
	users := Users(Input{Project: "shop", Web: true, Analytics: &Analytics{StaffEvents: []string{"internal_user"}}})
	active := func(envs ...string) string {
		t.Helper()
		return strings.SplitN(ch.run(panel(t, users, "active").SQL, start, end, envs...), "\t", 2)[0]
	}
	if got := active("e1"); got != "0" {
		t.Errorf("active in e1 = %s, want 0: u is staff there", got)
	}
	if got := active("e2"); got != "1" {
		t.Errorf("active in e2 = %s, want 1: e1's marker must not hide e2's u", got)
	}
	if got := active("e1", "e2"); got != "1" {
		t.Errorf("active over e1 and e2 = %s, want 1: only e2's u counts", got)
	}
}
