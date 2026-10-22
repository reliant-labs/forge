package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/reliant-labs/forge/pkg/dbquery"
	"github.com/reliant-labs/forge/pkg/pgtest"
)

// dbQueryFixture is a one-workload render whose api runs as runtime with the
// given DATABASE_URL.
func dbQueryFixture(runtime, dsn string) string {
	env := ""
	if dsn != "" {
		env = fmt.Sprintf(`"env":[{"name":"DATABASE_URL","value":%q}]`, dsn)
	}
	return fmt.Sprintf(`{"output":{"workloads":[{"name":"api","kind":"service","runtime":{"type":%q},"spec":{"kind":"service"%s}}]}}`,
		runtime, map[bool]string{true: "," + env, false: ""}[env != ""])
}

// runDBQueryCmd runs `forge db query args...` and returns stdout and the error.
func runDBQueryCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newDBCmd()
	var out, errout bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errout)
	cmd.SetArgs(append([]string{"query"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestDBQueryRequiresTheEnvAndOneStatement(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"dev", "select 1", "extra"},
	} {
		if _, err := runDBQueryCmd(t, args...); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	for name, o := range map[string]dbQueryOptions{
		"no statement":   {env: "dev", format: "table"},
		"both forms":     {env: "dev", sql: "select 1", file: "q.sql", format: "table"},
		"unknown format": {env: "dev", sql: "select 1", format: "xml"},
		"long reason":    {env: "dev", sql: "select 1", format: "table", reason: strings.Repeat("x", 501)},
	} {
		if err := validateDBQueryFlags(o); exitCodeForError(err) != exitWrong {
			t.Errorf("%s: err = %v, want exit 1", name, err)
		}
	}
}

func TestDBQueryPolicy(t *testing.T) {
	cluster := []WorkloadEntity{{Name: "api", Runtime: RuntimeEntity{Type: RuntimeCluster}}}
	for name, tc := range map[string]struct {
		e    *KCLEntities
		want string
	}{
		"host-only env":          {&KCLEntities{Workloads: []WorkloadEntity{{Name: "api", Runtime: RuntimeEntity{Type: RuntimeHost}}}}, dbQueryOpen},
		"local cluster":          {&KCLEntities{Lifecycle: lifecycleLocal, Workloads: cluster}, dbQueryOpen},
		"ephemeral cluster":      {&KCLEntities{Lifecycle: lifecycleEphemeral, Workloads: cluster}, dbQueryOpen},
		"undeclared cluster":     {&KCLEntities{Workloads: cluster}, dbQueryAudited},
		"applier is not scratch": {&KCLEntities{Applier: applierForge, Workloads: cluster}, dbQueryAudited},
	} {
		if got := dbQueryPolicy(tc.e); got != tc.want {
			t.Errorf("%s: policy %s, want %s", name, got, tc.want)
		}
	}

	o := dbQueryOptions{env: "prod"}
	if err := checkDBQueryPolicy(o, dbQueryOpen); err != nil {
		t.Errorf("open env refused: %v", err)
	}
	for _, bad := range []dbQueryOptions{
		{env: "prod"},
		{env: "prod", confirmEnv: "prod"},
		{env: "prod", confirmEnv: "staging", reason: "x"},
		{env: "prod", confirmEnv: "prod", reason: "   "},
	} {
		if err := checkDBQueryPolicy(bad, dbQueryAudited); exitCodeForError(err) != exitWrong {
			t.Errorf("%+v on an audited env: err = %v, want exit 1", bad, err)
		}
	}
	if err := checkDBQueryPolicy(dbQueryOptions{env: "prod", confirmEnv: "prod", reason: "ISS-7f3a"}, dbQueryAudited); err != nil {
		t.Errorf("confirmed audited query refused: %v", err)
	}
}

func TestDBQueryErrorExitCodes(t *testing.T) {
	for kind, want := range map[dbquery.Kind]int{
		dbquery.KindRefused:     exitWrong,
		dbquery.KindReadOnly:    exitWrong,
		dbquery.KindPermission:  exitWrong,
		dbquery.KindSQL:         exitWrong,
		dbquery.KindTimeout:     exitUndetermined,
		dbquery.KindCanceled:    exitUndetermined,
		dbquery.KindUnavailable: exitUndetermined,
		dbquery.KindAudit:       exitUndetermined,
	} {
		if got := exitCodeForError(dbQueryError(&dbquery.Error{Kind: kind, Message: "m"})); got != want {
			t.Errorf("%s: exit %d, want %d", kind, got, want)
		}
	}
}

func fixtureResult() *dbquery.Result {
	return &dbquery.Result{
		Columns: []dbquery.Column{{Name: "id", PGType: "int4"}, {Name: "name", PGType: "text"}, {Name: "meta", PGType: "jsonb"}, {Name: "ok", PGType: "bool"}},
		Rows: [][]any{
			{json.Number("1"), "ada", json.RawMessage(`{"a":1}`), true},
			{json.Number("2"), "multi\nline, \"quoted\"", nil, false},
		},
		RowCount: 2, Truncated: true, TruncatedBy: "rows", DurationMS: 3, Bytes: 40,
	}
}

func TestDBQueryTableAndCSV(t *testing.T) {
	var table bytes.Buffer
	if err := writeDBQueryTable(&table, fixtureResult()); err != nil {
		t.Fatal(err)
	}
	wantTable := "id  name                  meta     ok\n" +
		"1   ada                   {\"a\":1}  true\n" +
		"2   multi line, \"quoted\"  NULL     false\n" +
		"(2 rows, 3 ms; truncated by the rows cap, raise --max-rows or narrow the query)\n"
	if table.String() != wantTable {
		t.Errorf("table:\n%s\nwant:\n%s", table.String(), wantTable)
	}

	var csvOut bytes.Buffer
	if err := writeDBQueryCSV(&csvOut, fixtureResult()); err != nil {
		t.Fatal(err)
	}
	wantCSV := "id,name,meta,ok\n" +
		"1,ada,\"{\"\"a\"\":1}\",true\n" +
		"2,\"multi\nline, \"\"quoted\"\"\",,false\n"
	if csvOut.String() != wantCSV {
		t.Errorf("csv:\n%s\nwant:\n%s", csvOut.String(), wantCSV)
	}
}

func TestDBQueryRefusesTransportsItCannotReach(t *testing.T) {
	for _, tc := range []struct{ runtime, dsn string }{
		{RuntimeHosted, ""},
		{RuntimeCluster, ""},
		{RuntimeHost, ""}, // no DATABASE_URL declared
	} {
		t.Run(tc.runtime+tc.dsn, func(t *testing.T) {
			withKCLFixture(t, dbQueryFixture(tc.runtime, tc.dsn))
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			out, err := runDBQueryCmd(t, "dev", "select 1", "--json")
			if exitCodeForError(err) != exitUndetermined {
				t.Fatalf("err = %v, want exit 2", err)
			}
			var doc dbQueryDocument
			if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
				t.Fatalf("not one JSON document: %v\n%s", jerr, out)
			}
			if doc.OK || doc.ExitCode != exitUndetermined || doc.Schema != dbQuerySchema || doc.Error == "" {
				t.Errorf("envelope %+v", doc.jsonEnvelope)
			}
		})
	}
}

// End to end against a real Postgres: the workload's resolved DATABASE_URL,
// rows in every format, and writes refused with exit 1.
func TestDBQueryDirectAgainstPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a real postgres (pgtest); runs in task test")
	}
	dsn, cleanup, err := pgtest.NewURL()
	if err != nil {
		t.Fatalf("pgtest.NewURL: %v", err)
	}
	t.Cleanup(cleanup)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE jobs (id int PRIMARY KEY, crew text); INSERT INTO jobs VALUES (1, 'a'), (2, NULL), (3, 'b')`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)

	withKCLFixture(t, dbQueryFixture(RuntimeHost, dsn))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	out, err := runDBQueryCmd(t, "dev", "select id, crew from jobs order by id")
	if err != nil {
		t.Fatalf("query: %v\n%s", err, out)
	}
	if want := "id  crew\n1   a\n2   NULL\n3   b\n(3 rows, "; !strings.HasPrefix(out, want) {
		t.Errorf("table:\n%s", out)
	}

	out, err = runDBQueryCmd(t, "dev", "select count(*) as n from jobs where crew is null", "--json")
	if err != nil {
		t.Fatalf("json query: %v", err)
	}
	var doc struct {
		OK       bool             `json:"ok"`
		ExitCode int              `json:"exit_code"`
		Schema   string           `json:"schema"`
		Scope    dbQueryScope     `json:"scope"`
		Columns  []dbquery.Column `json:"columns"`
		Rows     [][]json.Number  `json:"rows"`
		RowCount int              `json:"row_count"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if !doc.OK || doc.Schema != dbQuerySchema || doc.RowCount != 1 || doc.Rows[0][0] != "1" || doc.Columns[0].PGType != "int8" {
		t.Errorf("json doc %+v", doc)
	}
	if doc.Scope != (dbQueryScope{Env: "dev", Transport: "direct", Workload: "api", Policy: dbQueryOpen}) {
		t.Errorf("scope %+v", doc.Scope)
	}

	file := t.TempDir() + "/q.sql"
	if err := os.WriteFile(file, []byte("select id from jobs where id < 3 order by id"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runDBQueryCmd(t, "dev", "-f", file, "--format", "csv")
	if err != nil || out != "id\n1\n2\n" {
		t.Errorf("csv from file: %v\n%q", err, out)
	}

	out, err = runDBQueryCmd(t, "dev", "select id from jobs order by id", "--max-rows", "2", "--json")
	if err != nil || !strings.Contains(out, `"truncated": true`) || !strings.Contains(out, `"truncated_by": "rows"`) {
		t.Errorf("row cap: %v\n%s", err, out)
	}

	for _, write := range []string{
		"insert into jobs values (4, 'c')",
		"update jobs set crew = 'x'",
		"select 1; delete from jobs",
		"with d as (delete from jobs returning id) select * from d",
	} {
		out, err := runDBQueryCmd(t, "dev", write, "--json")
		if exitCodeForError(err) != exitWrong {
			t.Errorf("%q: err = %v, want exit 1", write, err)
		}
		if !strings.Contains(out, `"ok": false`) || !strings.Contains(out, `"exit_code": 1`) {
			t.Errorf("%q: envelope:\n%s", write, out)
		}
	}
	out, err = runDBQueryCmd(t, "dev", "select count(*) from jobs", "--format", "csv")
	if err != nil || out != "count\n3\n" {
		t.Errorf("a write landed: %v %q", err, out)
	}
}
