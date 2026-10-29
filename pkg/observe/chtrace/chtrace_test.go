package chtrace

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// fakeConn answers every statement with err, and Query with rows of n rows.
type fakeConn struct {
	driver.Conn
	err  error
	rows int
	// rowsErr is what the rows report after the last one.
	rowsErr error
	// scanErr is what QueryRow's row reports from Scan and ScanStruct.
	scanErr error
	got     []string
}

func (f *fakeConn) Query(_ context.Context, q string, _ ...any) (driver.Rows, error) {
	f.got = append(f.got, q)
	if f.err != nil {
		return nil, f.err
	}
	return &fakeRows{left: f.rows, err: f.rowsErr}, nil
}

func (f *fakeConn) Exec(_ context.Context, q string, _ ...any) error {
	f.got = append(f.got, q)
	return f.err
}

func (f *fakeConn) Select(_ context.Context, _ any, q string, _ ...any) error {
	f.got = append(f.got, q)
	return f.err
}

func (f *fakeConn) QueryRow(_ context.Context, q string, _ ...any) driver.Row {
	f.got = append(f.got, q)
	return fakeRow{err: f.err, scanErr: f.scanErr}
}

type fakeRows struct {
	driver.Rows
	left int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.left == 0 {
		return false
	}
	r.left--
	return true
}
func (r *fakeRows) Close() error { return nil }
func (r *fakeRows) Err() error   { return r.err }

type fakeRow struct {
	driver.Row
	err, scanErr error
}

func (r fakeRow) Err() error           { return r.err }
func (r fakeRow) Scan(...any) error    { return r.scanErr }
func (r fakeRow) ScanStruct(any) error { return r.scanErr }

func underSpan(t *testing.T) (context.Context, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, _ := tp.Tracer("test").Start(context.Background(), "handler")
	return ctx, rec
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, a := range s.Attributes() {
		m[a.Key] = a.Value
	}
	return m
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// A Query is one CLIENT span named by its summary, open until the rows are
// closed, carrying the sanitized text, the query id and the rows returned.
// The EXECUTE AS prefix is not the operation.
func TestQueryIsASpanUntilTheRowsClose(t *testing.T) {
	ctx, rec := underSpan(t)
	fc := &fakeConn{rows: 3}
	conn := Wrap(fc)
	rows, err := conn.Query(ctx, "EXECUTE AS `r_org_a` SELECT ServiceName FROM t_org_a.otel_logs WHERE Body = 'secret' AND x = {p1:String}", "v")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Ended()) != 0 {
		t.Fatal("the span ended before the rows were read")
	}
	for rows.Next() {
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("%d spans, want 1", len(ended))
	}
	s := ended[0]
	if s.Name() != "SELECT t_org_a.otel_logs" || s.SpanKind() != trace.SpanKindClient {
		t.Errorf("span = %q %v", s.Name(), s.SpanKind())
	}
	a := attrs(s)
	for k, want := range map[attribute.Key]string{
		"db.system.name":     "clickhouse",
		"db.system":          "clickhouse",
		"db.operation.name":  "SELECT",
		"db.collection.name": "t_org_a.otel_logs",
		"db.query.summary":   "SELECT t_org_a.otel_logs",
	} {
		if got := a[k].AsString(); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if text := a["db.query.text"].AsString(); text == "" || regexp.MustCompile(`secret`).MatchString(text) {
		t.Errorf("db.query.text = %q: want the statement with its literals removed", text)
	}
	if id := a[AttrQueryID].AsString(); !uuidRe.MatchString(id) {
		t.Errorf("query id = %q, want a UUID", id)
	}
	if got := a["db.response.returned_rows"].AsInt64(); got != 3 {
		t.Errorf("returned rows = %d, want 3", got)
	}
	for _, k := range []attribute.Key{AttrReadRows, AttrReadBytes} {
		if _, ok := a[k]; !ok {
			t.Errorf("%s is missing", k)
		}
	}
	if s.Status().Code == codes.Error {
		t.Errorf("a successful statement is marked failed: %v", s.Status())
	}
	if fc.got[0][:10] != "EXECUTE AS" {
		t.Errorf("the statement reached the server changed: %q", fc.got[0])
	}
}

// A failure is the span's status and message, an exception event, and the
// ClickHouse exception's name and code; a cancellation says so.
func TestFailuresAreRecordedOnTheSpan(t *testing.T) {
	ctx, rec := underSpan(t)
	timeout := &clickhouse.Exception{Code: 159, Name: "TIMEOUT_EXCEEDED", Message: "Timeout exceeded: elapsed 20 seconds"}
	_, _ = Wrap(&fakeConn{err: timeout}).Query(ctx, "SELECT 1")
	_ = Wrap(&fakeConn{err: context.Canceled}).Exec(ctx, "INSERT INTO t VALUES (1)")
	rows, _ := Wrap(&fakeConn{rows: 1, rowsErr: context.DeadlineExceeded}).Query(ctx, "SELECT a FROM t")
	for rows.Next() {
	}
	_ = rows.Close()
	ended := rec.Ended()
	if len(ended) != 3 {
		t.Fatalf("%d spans, want 3", len(ended))
	}
	for i, want := range []struct{ errType, code, msg string }{
		{"TIMEOUT_EXCEEDED", "159", "Timeout exceeded"},
		{"context_canceled", "", "context canceled"},
		{"context_deadline_exceeded", "", "context deadline exceeded"},
	} {
		s := ended[i]
		if s.Status().Code != codes.Error || !regexp.MustCompile(want.msg).MatchString(s.Status().Description) {
			t.Errorf("%s: status = %v, want Error with %q", s.Name(), s.Status(), want.msg)
		}
		a := attrs(s)
		if got := a["error.type"].AsString(); got != want.errType {
			t.Errorf("%s: error.type = %q, want %q", s.Name(), got, want.errType)
		}
		if got := a["db.response.status_code"].AsString(); got != want.code {
			t.Errorf("%s: status code = %q, want %q", s.Name(), got, want.code)
		}
		var exception bool
		for _, e := range s.Events() {
			exception = exception || e.Name == "exception"
		}
		if !exception {
			t.Errorf("%s: no exception event", s.Name())
		}
	}
}

func TestSelectQueryRowAndExecAreSpans(t *testing.T) {
	ctx, rec := underSpan(t)
	conn := Wrap(&fakeConn{})
	_ = conn.Select(ctx, &[]int{}, "SELECT n FROM numbers")
	_ = conn.QueryRow(ctx, "SELECT count() FROM t").Scan(new(uint64))
	_ = conn.Exec(ctx, "ALTER TABLE t DELETE WHERE 1")
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	if len(names) != 3 || names[0] != "SELECT numbers" || names[1] != "SELECT t" || names[2] != "ALTER" {
		t.Errorf("spans = %v", names)
	}
}

// No caller span, no span: a statement outside a request is not a trace.
func TestNoSpanWithoutACallerSpan(t *testing.T) {
	_, rec := underSpan(t)
	fc := &fakeConn{rows: 1}
	rows, err := Wrap(fc).Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows.(*tracedRows); ok {
		t.Error("rows were wrapped with no span to end")
	}
	_ = rows.Close()
	if n := len(rec.Ended()); n != 0 {
		t.Errorf("%d spans with no caller span", n)
	}
	if Wrap(Wrap(fc)).(*tracedConn).Conn != fc {
		t.Error("wrapping twice nests the tracer")
	}
}

func TestStripExecuteAs(t *testing.T) {
	for in, want := range map[string]string{
		"EXECUTE AS `r_a` SELECT 1":       "SELECT 1",
		"execute as r_a\nSELECT 1":        "SELECT 1",
		"  EXECUTE AS \"u x\" INSERT ...": "INSERT ...",
		"SELECT 1":                        "SELECT 1",
		"EXECUTE AS `unterminated":        "EXECUTE AS `unterminated",
	} {
		if got := stripExecuteAs(in); got != want {
			t.Errorf("stripExecuteAs(%q) = %q, want %q", in, got, want)
		}
	}
	if ErrorType(errors.New("x")) != "_OTHER" {
		t.Error("an unclassified error is not _OTHER")
	}
}

// A QueryRow span lasts until the row is scanned, and a scan that fails is
// the statement's failure; a statement that failed outright ends at once.
func TestQueryRowSpanEndsAtScan(t *testing.T) {
	ctx, rec := underSpan(t)
	row := Wrap(&fakeConn{}).QueryRow(ctx, "SELECT count() FROM t")
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("the span ended before the row was scanned (%d ended)", n)
	}
	if err := row.Scan(new(uint64)); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.Ended()); n != 1 {
		t.Fatalf("%d spans after Scan, want 1", n)
	}
	if s := rec.Ended()[0]; s.Status().Code == codes.Error {
		t.Errorf("a successful scan is marked failed: %v", s.Status())
	}

	bad := errors.New("converting UInt64 to *string is unsupported")
	_ = Wrap(&fakeConn{scanErr: bad}).QueryRow(ctx, "SELECT count() FROM t").ScanStruct(&struct{}{})
	if s := rec.Ended()[1]; s.Status().Code != codes.Error || s.Status().Description != bad.Error() {
		t.Errorf("a failed ScanStruct: status = %v, want Error %q", s.Status(), bad)
	}

	timeout := &clickhouse.Exception{Code: 159, Name: "TIMEOUT_EXCEEDED", Message: "Timeout exceeded"}
	row = Wrap(&fakeConn{err: timeout}).QueryRow(ctx, "SELECT count() FROM t")
	if n := len(rec.Ended()); n != 3 {
		t.Fatalf("a QueryRow that failed outright left its span open (%d ended)", n)
	}
	if err := row.Scan(new(uint64)); err != nil {
		t.Fatal(err) // the fake's row scans nothing; the point is no second end
	}
	if n := len(rec.Ended()); n != 3 {
		t.Errorf("a failed QueryRow's span ended twice: %d ended", n)
	}
}

// A ClickHouse literal whose quote is backslash-escaped stays out of both
// recorded texts: read by Postgres's rules it would end at the escape.
func TestEscapedLiteralsNeverRecorded(t *testing.T) {
	_, attrs := Describe(`SELECT a FROM t WHERE b = 'x\'secret' AND c = ‘secret’ -- secret`)
	for _, a := range attrs {
		if strings.Contains(a.Value.Emit(), "secret") {
			t.Errorf("%s = %q carries the literal", a.Key, a.Value.Emit())
		}
	}
}
