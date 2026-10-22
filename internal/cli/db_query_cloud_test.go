package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cloud"
)

// fakeQueryDatabase is a control plane that answers QueryDatabase with reply
// (or err) and records the request bodies.
type fakeQueryDatabase struct {
	reply  string
	err    error
	bodies []map[string]any
}

func (f *fakeQueryDatabase) Call(_ context.Context, procedure string, req, out any) error {
	if procedure != procQueryDatabase {
		return errors.New("unexpected call " + procedure)
	}
	b, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.bodies = append(f.bodies, m)
	if f.err != nil {
		return f.err
	}
	return json.Unmarshal([]byte(f.reply), out)
}

// withHostedDB points the hosted transport at fake, as env "env-prod-1".
func withHostedDB(t *testing.T, fake *fakeQueryDatabase) {
	t.Helper()
	prev := dbQueryCloudOpen
	dbQueryCloudOpen = func(context.Context, string) (dbQueryCaller, string, error) { return fake, "env-prod-1", nil }
	t.Cleanup(func() { dbQueryCloudOpen = prev })
}

// hostedDBQueryFixture is a one-workload render of prod whose api is hosted, with
// the given declared policy.
func hostedDBQueryFixture(policy string) string {
	return `{"output":{"data_access":{"db_query":"` + policy + `"},"workloads":[{"name":"api","kind":"service","runtime":{"type":"` + RuntimeHosted + `"},"spec":{"kind":"service"}}]}}`
}

const queryDatabaseReply = `{
 "columns":[{"name":"id","pgType":"int8"},{"name":"name","pgType":"text"},{"name":"meta","pgType":"jsonb"},{"name":"gone","pgType":"timestamptz"},{"name":"ok","pgType":"bool"}],
 "rows":[{"values":[7,"ada",{"b":1, "a":[2]},null,true]},{"values":["12345678901234567890","bob",[],null,false]}],
 "rowCount":"2","durationMs":"12","replica":"ro","auditId":"aud-1","sqlSha256":"abc","database":"main"}`

func TestDBQueryHostedRunsThroughTheControlPlane(t *testing.T) {
	withKCLFixture(t, hostedDBQueryFixture("audited"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	fake := &fakeQueryDatabase{reply: queryDatabaseReply}
	withHostedDB(t, fake)

	out, err := runDBQueryCmd(t, "prod", "select * from jobs", "--json", "--max-rows", "50", "--timeout", "3s",
		"--confirm-env", "prod", "--reason", " ISS-7f3a ", "--database", "main")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(fake.bodies) != 1 {
		t.Fatalf("calls = %d", len(fake.bodies))
	}
	body := fake.bodies[0]
	for k, want := range map[string]any{
		"environmentId": "env-prod-1", "sql": "select * from jobs", "maxRows": float64(50), "timeoutMs": float64(3000),
		"confirmEnvId": "env-prod-1", "reason": "ISS-7f3a", "database": "main",
	} {
		if body[k] != want {
			t.Errorf("request %s = %v, want %v", k, body[k], want)
		}
	}

	var doc struct {
		OK    bool `json:"ok"`
		Scope struct {
			Transport, Database string
			Policy              *string
		} `json:"scope"`
		Audit    dbQueryAudit     `json:"audit"`
		Rows     [][]any          `json:"rows"`
		RowCount int              `json:"row_count"`
		Duration int64            `json:"duration_ms"`
		Columns  []map[string]any `json:"columns"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !doc.OK || doc.Scope.Transport != "control-plane" || doc.Scope.Database != "main" || doc.Scope.Policy != nil {
		t.Errorf("scope: %+v (the control plane applies the recorded policy; forge reports none)", doc.Scope)
	}
	if doc.Audit != (dbQueryAudit{ID: "aud-1", SQLSHA256: "abc", Replica: "ro"}) {
		t.Errorf("audit = %+v", doc.Audit)
	}
	if doc.RowCount != 2 || doc.Duration != 12 || len(doc.Columns) != 5 || doc.Columns[2]["pg_type"] != "jsonb" {
		t.Errorf("result: rows=%d duration=%d columns=%v", doc.RowCount, doc.Duration, doc.Columns)
	}
	if big, ok := doc.Rows[1][0].(string); !ok || big != "12345678901234567890" {
		t.Errorf("a big number stays a string: %#v", doc.Rows[1][0])
	}
	if obj, ok := doc.Rows[0][2].(map[string]any); !ok || obj["b"] != float64(1) {
		t.Errorf("a jsonb object stays an object: %#v", doc.Rows[0][2])
	}

	table, err := runDBQueryCmd(t, "prod", "select 1", "--confirm-env", "prod", "--reason", "x")
	if err != nil || !strings.Contains(table, "(2 rows, 12 ms; audit aud-1)") || !strings.Contains(table, "ada") {
		t.Errorf("table (%v):\n%s", err, table)
	}
}

// The declared policy of this checkout is not the authority for a hosted env:
// the control plane judges from the recorded shape. A declared deny does not
// stop the call; a confirmation for another env is not passed on.
func TestDBQueryHostedLeavesThePolicyToTheControlPlane(t *testing.T) {
	withKCLFixture(t, hostedDBQueryFixture("deny"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	fake := &fakeQueryDatabase{reply: queryDatabaseReply}
	withHostedDB(t, fake)
	if out, err := runDBQueryCmd(t, "prod", "select 1", "--confirm-env", "staging", "--json"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(fake.bodies) != 1 {
		t.Fatal("a declared deny stopped a hosted query the control plane decides")
	}
	if _, sent := fake.bodies[0]["confirmEnvId"]; sent {
		t.Errorf("a confirmation of another env was sent: %v", fake.bodies[0])
	}
	if _, sent := fake.bodies[0]["reason"]; sent {
		t.Errorf("an empty reason was sent: %v", fake.bodies[0])
	}
}

func TestDBQueryHostedErrorsMapToTheExitTable(t *testing.T) {
	withKCLFixture(t, hostedDBQueryFixture("open"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, tc := range []struct {
		name       string
		err        error
		wantCode   int
		wantReason string
		wantMsg    string
	}{
		{"guard", &cloud.Error{Code: cloud.CodeInvalidArgument, Reason: "multiple_statements", Message: "one statement only"}, exitWrong, "multiple_statements", "one statement only"},
		{"confirm", &cloud.Error{Code: cloud.CodeFailedPrecondition, Reason: "confirmation_required", Message: "confirm"}, exitWrong, "confirmation_required", "confirm"},
		{"no database", &cloud.Error{Code: cloud.CodeFailedPrecondition, Reason: "no_database"}, exitWrong, "no_database", ""},
		{"role not provisioned", &cloud.Error{Code: cloud.CodeFailedPrecondition, Reason: "credential_missing"}, exitUndetermined, "credential_missing", ""},
		{"policy", &cloud.Error{Code: cloud.CodePermissionDenied, Message: "disabled by its data_access policy"}, exitWrong, "", "data_access"},
		{"secret column", &cloud.Error{Code: cloud.CodePermissionDenied, Reason: "permission_denied"}, exitWrong, "permission_denied", ""},
		{"timeout", &cloud.Error{Code: cloud.CodeDeadlineExceeded, Reason: "statement_timeout"}, exitUndetermined, "statement_timeout", ""},
		{"rate", &cloud.Error{Code: cloud.CodeResourceExhausted, Reason: "rate_limited", Message: "retry in 3s"}, exitUndetermined, "rate_limited", "retry in 3s"},
		{"down", &cloud.Error{Code: cloud.CodeUnavailable}, exitUndetermined, "", ""},
		{"old control plane", &cloud.Error{Code: cloud.CodeUnimplemented}, exitUndetermined, "", "does not serve database queries yet"},
		{"transport", errors.New("dial tcp: connection refused"), exitUndetermined, "", "could not be reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withHostedDB(t, &fakeQueryDatabase{err: tc.err})
			out, err := runDBQueryCmd(t, "prod", "select 1", "--json")
			if got := exitCodeForError(err); got != tc.wantCode {
				t.Fatalf("exit = %d, want %d (%v)", got, tc.wantCode, err)
			}
			var doc dbQueryDocument
			if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
				t.Fatalf("not one JSON document: %v\n%s", jerr, out)
			}
			if doc.OK || doc.ExitCode != tc.wantCode || doc.Reason != tc.wantReason || !strings.Contains(doc.Error, tc.wantMsg) {
				t.Errorf("doc: ok=%v exit=%d reason=%q error=%q", doc.OK, doc.ExitCode, doc.Reason, doc.Error)
			}
			if doc.Scope == nil || doc.Scope.Transport != "control-plane" {
				t.Errorf("scope = %+v", doc.Scope)
			}
		})
	}
}
