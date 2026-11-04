//go:build integration

package obsquery_test

// Who and where, against a real ClickHouse holding the hosted browser tables:
// a session carries the location the platform stamped on its browser resource,
// and a backend trace joined to it by the web runtime's session baggage is
// found by session id and by the authenticated user id, with both columns set.
//
//	go test -tags integration -run 'RumIdentity' ./pkg/obsquery/

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/obsquery"
)

func TestRumIdentitySessionGeoAndTraceLinks(t *testing.T) {
	c := startClickHouse(t)
	ctx := context.Background()
	rumHostedDB(t, c, "ri")
	// The captured sessions, as the gateway stamps them: one located browser.
	located := strings.ReplaceAll(rumFixture(t, "traces.jsonl"), `"rum.sessionId":"`+rumReplaySession+`"`,
		`"rum.sessionId":"`+rumReplaySession+`","geo.country.iso_code":"DE","geo.region.name":"Land Berlin","geo.locality.name":"Berlin"`)
	rumInsert(t, c, "ri", "otel_traces", located)
	rumInsert(t, c, "ri", "otel_logs", rumFixture(t, "logs.jsonl"))

	// A backend trace the browser's baggage reached: the server span carries
	// session.id (from baggage) and user.id (from the authenticated caller).
	server, _ := json.Marshal(map[string]any{
		"Timestamp": "2026-10-19 11:44:09.000000000", "TraceId": "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0", "SpanId": "b0b0b0b0b0b0b0b1",
		"ParentSpanId": "", "SpanName": "/svc.v1.Svc/Get", "SpanKind": "Server", "ServiceName": "api",
		"ResourceAttributes": map[string]string{"service.name": "api", "reliant.env.id": "env-prod"},
		"SpanAttributes":     map[string]string{"session.id": rumReplaySession, "user.id": "u-auth"},
		"Duration":           1000000, "StatusCode": "Unset",
	})
	rumInsert(t, c, "ri", "otel_traces", string(server))
	run := rumRunner(c)
	sc := rumScopeOf("ri", "env-prod")

	s, err := obsquery.GetSession(ctx, run, obsquery.SessionLookup{Scope: sc, SessionID: rumReplaySession})
	if err != nil {
		t.Fatal(err)
	}
	if s.Country != "DE" || s.Region != "Land Berlin" || s.City != "Berlin" || s.ClientAddress != "" {
		t.Errorf("located session = %+v", s)
	}
	plain, err := obsquery.GetSession(ctx, run, obsquery.SessionLookup{Scope: sc, SessionID: rumPlainSession})
	if err != nil || plain.Country != "" || plain.City != "" {
		t.Errorf("unlocated session = %+v, %v", plain, err)
	}

	traces := func(q obsquery.TraceQuery) map[string][]string {
		t.Helper()
		q.Scope, q.Limit = sc, 100
		st, err := q.Compile()
		if err != nil {
			t.Fatal(err)
		}
		rows, err := run(ctx, st)
		if err != nil {
			t.Fatalf("%v\n%s", err, st.SQL)
		}
		defer rows.Close()
		out := map[string][]string{}
		for rows.Next() {
			var (
				id, rs, rn, src, user, sess, country, region, city string
				start                                              time.Time
				dur, rd, tw                                        float64
				spans, errs                                        uint64
				svcs                                               []string
			)
			if err := rows.Scan(&id, &start, &dur, &rs, &rn, &rd, &spans, &errs, &tw, &svcs, &src, &user, &sess, &country, &region, &city); err != nil {
				t.Fatal(err)
			}
			out[id] = []string{user, sess, country, city}
		}
		return out
	}
	bySession := traces(obsquery.TraceQuery{SessionID: rumReplaySession})
	if got := bySession["b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"]; len(got) == 0 || got[0] != "u-auth" || got[1] != rumReplaySession {
		t.Errorf("the backend trace by session = %v (all: %v)", got, bySession)
	}
	for id, v := range bySession {
		if v[1] != rumReplaySession {
			t.Errorf("trace %s of another session matched: %v", id, v)
		}
	}
	locatedTraces := 0
	for _, v := range bySession {
		if v[2] == "DE" && v[3] == "Berlin" {
			locatedTraces++
		}
	}
	if locatedTraces == 0 {
		t.Errorf("no browser trace of the session carries its location: %v", bySession)
	}
	byUser := traces(obsquery.TraceQuery{UserID: "u-auth"})
	if len(byUser) != 1 || byUser["b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"] == nil {
		t.Errorf("traces of u-auth = %v, want the one backend trace", byUser)
	}
	if n := len(traces(obsquery.TraceQuery{UserID: "user-42"})); n == 0 {
		t.Error("the page-reported user id finds none of its browser traces")
	}
}
