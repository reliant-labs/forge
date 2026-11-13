//go:build integration

package obsquery_test

// The user-activity statement against a real ClickHouse holding the hosted
// product_events table (control-plane's 010_tenant_create.sql shape), over a
// small population whose answer is worked out by hand.
//
//	go test -tags integration -run UserActivity ./pkg/obsquery/

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/reliant-labs/forge/pkg/obsquery"
)

const productEventsDDL = `CREATE TABLE %s.product_events (
    Timestamp DateTime64(9),
    ServiceName LowCardinality(String),
    ServiceVersion LowCardinality(String),
    EnvId LowCardinality(String),
    SessionId String,
    UserId String,
    EventName LowCardinality(String),
    Props Map(LowCardinality(String), String),
    RetentionDays UInt16 DEFAULT 90,
    Client LowCardinality(String),
    VisitorId String
) ENGINE = MergeTree PARTITION BY toDate(Timestamp) ORDER BY (ServiceName, EventName, Timestamp)`

type productEvent struct {
	at        string // RFC 3339
	env, user string
	event     string
	client    string
	retention uint16
	props     map[string]string
}

func seedProductEvents(t *testing.T, c clickhouse.Conn, db string, events []productEvent) {
	t.Helper()
	ctx := context.Background()
	if err := c.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(ctx, fmt.Sprintf(productEventsDDL, db)); err != nil {
		t.Fatal(err)
	}
	batch, err := c.PrepareBatch(ctx, "INSERT INTO "+db+".product_events (Timestamp, ServiceName, EnvId, UserId, EventName, Client, RetentionDays, Props)")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		ts, err := time.Parse(time.RFC3339, e.at)
		if err != nil {
			t.Fatal(err)
		}
		client, retention := e.client, e.retention
		if client == "" {
			client = "backend"
		}
		if retention == 0 {
			retention = 90
		}
		props := e.props
		if props == nil {
			props = map[string]string{}
		}
		if err := batch.Append(ts, "api", e.env, e.user, e.event, client, retention, props); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatal(err)
	}
}

func TestUserActivityOverAPopulation(t *testing.T) {
	c := startClickHouse(t)
	const db = "t_users"
	seedProductEvents(t, c, db, []productEvent{
		// prod reports sign-ups.
		{at: "2026-11-05T09:00:00Z", env: "prod", user: "u1", event: "auth_signed_up"},
		{at: "2026-11-05T09:00:01Z", env: "prod", user: "u1", event: "auth_signed_in"},
		{at: "2026-11-06T10:00:00Z", env: "prod", user: "u1", event: "project_created", client: "web"},
		{at: "2026-09-01T10:00:00Z", env: "prod", user: "u2", event: "auth_signed_up"},
		{at: "2026-10-30T10:00:00Z", env: "prod", user: "u2", event: "auth_signed_in"},
		{at: "2026-11-04T10:00:00Z", env: "prod", user: "u2", event: "auth_signed_in"},
		{at: "2026-11-04T11:00:00Z", env: "prod", user: "u2", event: "auth_signed_in"}, // a second replica's copy
		{at: "2026-10-28T10:00:00Z", env: "prod", user: "u3", event: "auth_signed_in"},
		{at: "2026-11-07T10:00:00Z", env: "prod", user: "u4", event: "auth_signed_out"}, // not activity
		{at: "2026-10-29T10:00:00Z", env: "prod", user: "u5", event: "auth_signed_up"},
		{at: "2026-10-29T10:00:01Z", env: "prod", user: "u5", event: "auth_signed_in"},
		{at: "2026-11-06T10:00:00Z", env: "prod", user: "", event: "page_viewed", client: "web"},        // anonymous
		{at: "2026-11-06T00:00:00Z", env: "prod", user: "", event: "auth_accounts", client: "platform"}, // platform
		// stage reports no sign-ups: new is first seen.
		{at: "2026-11-08T10:00:00Z", env: "stage", user: "s1", event: "task_created", retention: 30},
		{at: "2026-10-01T10:00:00Z", env: "stage", user: "s2", event: "task_created", retention: 30},
		{at: "2026-11-08T10:00:00Z", env: "stage", user: "s2", event: "task_created", retention: 30},
		// out of scope.
		{at: "2026-11-08T10:00:00Z", env: "other", user: "o1", event: "auth_signed_up"},
	})
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	q := obsquery.UserActivityQuery{
		Scope: obsquery.Scope{Database: db, EnvRestricted: true, EnvIDs: []string{"prod", "stage", "empty"},
			Envs: []obsquery.Env{{ID: "prod", Name: "Production"}}, Start: end.Add(-7 * 24 * time.Hour), End: end},
		SeriesDays: 7, WantSeenTotal: true,
	}
	got, err := obsquery.ReadUserActivity(context.Background(), rumRunner(c), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].EnvID != "prod" || got[1].EnvID != "stage" || got[2].EnvID != "empty" {
		t.Fatalf("envs = %+v", got)
	}
	prod, stage, empty := got[0], got[1], got[2]

	counts := func(a, n, r int64) obsquery.UserActivityCounts {
		return obsquery.UserActivityCounts{Active: a, New: n, Returning: r}
	}
	if prod.EnvName != "Production" || prod.NewRule != obsquery.NewUsersSignups {
		t.Errorf("prod name/rule = %q/%q", prod.EnvName, prod.NewRule)
	}
	if prod.Current != counts(2, 1, 1) {
		t.Errorf("prod current = %+v, want active u1,u2; new u1; returning u2", prod.Current)
	}
	if prod.Previous != counts(3, 1, 2) {
		t.Errorf("prod previous = %+v, want active u2,u3,u5; new u5", prod.Previous)
	}
	if !reflect.DeepEqual(prod.Clients, []string{"backend", "web"}) {
		t.Errorf("prod clients = %v", prod.Clients)
	}
	if prod.SeenTotal == nil || *prod.SeenTotal != 4 {
		t.Errorf("prod seen total = %v, want 4 (u1,u2,u3,u5)", prod.SeenTotal)
	}
	if want := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC); !prod.FirstEventAt.Equal(want) || prod.RetentionDays != 90 {
		t.Errorf("prod first event %v retention %d", prod.FirstEventAt, prod.RetentionDays)
	}
	wantDays := map[string]obsquery.UserActivityCounts{
		"2026-11-04": counts(1, 0, 1), "2026-11-05": counts(1, 1, 0), "2026-11-06": counts(1, 0, 1),
	}
	if len(prod.Days) != 7 || prod.Days[0].Day != "2026-11-04" || prod.Days[6].Day != "2026-11-10" {
		t.Fatalf("prod days = %+v", prod.Days)
	}
	for _, d := range prod.Days {
		if d.UserActivityCounts != wantDays[d.Day] {
			t.Errorf("prod %s = %+v, want %+v", d.Day, d.UserActivityCounts, wantDays[d.Day])
		}
	}

	if stage.NewRule != obsquery.NewUsersFirstSeen || stage.Current != counts(2, 1, 1) || stage.Previous != counts(0, 0, 0) {
		t.Errorf("stage = %+v, want first-seen rule, current active s1,s2 new s1", stage)
	}
	if stage.RetentionDays != 30 || stage.SeenTotal == nil || *stage.SeenTotal != 2 {
		t.Errorf("stage retention %d seen %v", stage.RetentionDays, stage.SeenTotal)
	}

	if empty.Current != counts(0, 0, 0) || !empty.FirstEventAt.IsZero() || len(empty.Days) != 7 ||
		empty.SeenTotal == nil || *empty.SeenTotal != 0 || len(empty.Clients) != 0 {
		t.Errorf("an env with no rows must be zero-filled: %+v", empty)
	}

	// The invariants the console relies on.
	for _, e := range got {
		for _, w := range []obsquery.UserActivityCounts{e.Current, e.Previous} {
			if w.Returning > w.Active || w.Active-w.Returning > w.New {
				t.Errorf("%s breaks returning <= active, active-returning <= new: %+v", e.EnvID, w)
			}
		}
	}
}

// The declared system events and staff, and the emitters' marker, against
// one population. prod reports sign-ups; the range is the 7 days before
// 2026-11-10 12:00.
//
//	u1      signs up and in on 11-05: a person
//	owner   only billing_state, one a day, emitted before anyone marked it:
//	        history, which only a declared system event corrects
//	owner2  billing_state marked by its emitter (forge.activity = system)
//	staff   signs up on 11-06 and carries the app's internal_user marker
//	canary  a synthetic sign-in on 11-07, staff by its id
func TestUserActivityLeavesOutSystemEventsAndStaff(t *testing.T) {
	c := startClickHouse(t)
	const db = "t_users_filter"
	marked := map[string]string{"forge.activity": "system"}
	events := []productEvent{
		{at: "2026-11-05T09:00:00Z", env: "prod", user: "u1", event: "auth_signed_up"},
		{at: "2026-11-05T09:00:01Z", env: "prod", user: "u1", event: "auth_signed_in"},
		{at: "2026-11-06T10:00:00Z", env: "prod", user: "staff", event: "auth_signed_up"},
		{at: "2026-11-06T00:10:00Z", env: "prod", user: "staff", event: "internal_user"},
		{at: "2026-11-07T10:00:00Z", env: "prod", user: "canary", event: "auth_signed_in"},
	}
	for d := 4; d <= 9; d++ {
		at := fmt.Sprintf("2026-11-%02dT10:00:00Z", d)
		events = append(events,
			productEvent{at: at, env: "prod", user: "owner", event: "billing_state"},
			productEvent{at: at, env: "prod", user: "owner2", event: "billing_state", props: marked})
	}
	seedProductEvents(t, c, db, events)
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		filter obsquery.UserFilter
		want   obsquery.UserActivityCounts
		seen   int64
	}{
		{"defaults: only the marked events are left out", obsquery.UserFilter{},
			obsquery.UserActivityCounts{Active: 4, New: 2, Returning: 2}, 4}, // u1 owner staff canary; new u1 staff
		{"a declared system event corrects history", obsquery.UserFilter{SystemEvents: []string{"billing_state"}},
			obsquery.UserActivityCounts{Active: 3, New: 2, Returning: 1}, 3}, // u1 staff canary
		{"staff by marker event", obsquery.UserFilter{SystemEvents: []string{"billing_state"}, StaffEvents: []string{"internal_user"}},
			obsquery.UserActivityCounts{Active: 2, New: 1, Returning: 1}, 2}, // u1 canary
		{"staff by id", obsquery.UserFilter{SystemEvents: []string{"billing_state"}, StaffEvents: []string{"internal_user"}, StaffUserIDs: []string{"canary"}},
			obsquery.UserActivityCounts{Active: 1, New: 1, Returning: 0}, 1}, // u1
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := obsquery.UserActivityQuery{
				Scope:         obsquery.Scope{Database: db, EnvRestricted: true, EnvIDs: []string{"prod"}, Start: end.Add(-7 * 24 * time.Hour), End: end},
				WantSeenTotal: true, Filter: tc.filter,
			}
			got, err := obsquery.ReadUserActivity(context.Background(), rumRunner(c), q)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("envs = %+v", got)
			}
			if got[0].Current != tc.want {
				t.Errorf("current = %+v, want %+v", got[0].Current, tc.want)
			}
			if got[0].NewRule != obsquery.NewUsersSignups {
				t.Errorf("new rule = %q: a filter must not hide that the env reports sign-ups", got[0].NewRule)
			}
			if got[0].SeenTotal == nil || *got[0].SeenTotal != tc.seen {
				t.Errorf("seen total = %v, want %d", got[0].SeenTotal, tc.seen)
			}
		})
	}
}
