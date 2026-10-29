// Package chtrace makes a ClickHouse connection's statements OpenTelemetry
// spans, the way dbtrace does for Postgres.
//
// A ClickHouse read is usually the slow part of the request that runs it, and
// without a span it is invisible: a trace shows the handler taking ten seconds
// and children covering fifteen milliseconds, and nothing says the time went
// to ClickHouse, which statement it was, or why it ended. Wrap the connection
// once, where it is opened, and every Query, QueryRow, Select and Exec on it is
// a CLIENT span carrying:
//
//   - db.system.name / db.system "clickhouse", db.operation.name,
//     db.collection.name, and db.query.summary ("SELECT t_org.otel_logs"),
//     which is also the span name;
//   - db.query.text, the statement with every literal replaced by ?
//     (dbtrace.SanitizeSQL). Bound parameters are never recorded;
//   - db.clickhouse.query_id, the id the statement runs under, so the span
//     joins system.query_log (`WHERE query_id = '…'`) for the server's side:
//     its read rows, memory, exception and profile events;
//   - db.clickhouse.read_rows / read_bytes, what the server reported reading
//     (progress packets), and db.response.returned_rows;
//   - on failure: the error recorded as an exception event, the span status
//     Error with the message, error.type (the ClickHouse exception name such
//     as TIMEOUT_EXCEEDED, or context_canceled / context_deadline_exceeded),
//     and db.response.status_code (the ClickHouse exception code).
//
// A Query span lasts until its rows are closed: streaming the result is part
// of the statement's cost.
//
// Like dbtrace, a span is only opened under an existing span, so a statement
// with no span in its context (a startup migration) stays out of the traces.
//
// The wrapper sets the statement's query id, progress and profile-info
// callbacks and its OpenTelemetry span context through clickhouse.Context. A
// caller that sets its own query id or callbacks on the same context has them
// replaced; settings and parameters are preserved.
package chtrace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/reliant-labs/forge/pkg/observe/dbtrace"
)

// System is the db.system.name every span carries.
const System = "clickhouse"

// Attribute keys this package sets beyond the OpenTelemetry semantic
// conventions' db.* set.
const (
	AttrQueryID   = "db.clickhouse.query_id"
	AttrReadRows  = "db.clickhouse.read_rows"
	AttrReadBytes = "db.clickhouse.read_bytes"
)

const tracerName = "github.com/reliant-labs/forge/pkg/observe/chtrace"

// Wrap returns conn with every statement traced. Methods that run no
// statement (Ping, Stats, PrepareBatch, ...) pass through.
func Wrap(conn driver.Conn) driver.Conn {
	if conn == nil {
		return nil
	}
	if _, ok := conn.(*tracedConn); ok {
		return conn
	}
	return &tracedConn{Conn: conn}
}

type tracedConn struct {
	driver.Conn
}

func (c *tracedConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	ctx, st := start(ctx, query)
	rows, err := c.Conn.Query(ctx, query, args...)
	if err != nil {
		st.end(err)
		return nil, err
	}
	if st == nil {
		return rows, nil
	}
	return &tracedRows{Rows: rows, st: st}, nil
}

func (c *tracedConn) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	ctx, st := start(ctx, query)
	row := c.Conn.QueryRow(ctx, query, args...)
	if st != nil {
		st.returned.Store(-1)
		st.end(row.Err())
	}
	return row
}

func (c *tracedConn) Select(ctx context.Context, dest any, query string, args ...any) error {
	ctx, st := start(ctx, query)
	err := c.Conn.Select(ctx, dest, query, args...)
	st.end(err)
	return err
}

func (c *tracedConn) Exec(ctx context.Context, query string, args ...any) error {
	ctx, st := start(ctx, query)
	err := c.Conn.Exec(ctx, query, args...)
	st.end(err)
	return err
}

// statement is one traced statement's span and what the server reported.
type statement struct {
	span                trace.Span
	readRows, readBytes atomic.Uint64
	// returned is the rows the caller read (Query), or -1 to take the
	// server's profile-info count instead (QueryRow, Select, Exec).
	returned    atomic.Int64
	profileRows atomic.Uint64
	ended       atomic.Bool
}

// start opens the statement's span under the caller's, and returns the
// context the statement must run with. A nil statement means no span (no
// caller span); every method on it is then a no-op.
func start(ctx context.Context, query string) (context.Context, *statement) {
	parent := trace.SpanFromContext(ctx)
	if !parent.SpanContext().IsValid() {
		return ctx, nil
	}
	summary, attrs := Describe(query)
	id := newQueryID()
	attrs = append(attrs, attribute.String(AttrQueryID, id))
	ctx, span := parent.TracerProvider().Tracer(tracerName).Start(ctx, summary,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	st := &statement{span: span}
	st.returned.Store(-1)
	ctx = clickhouse.Context(ctx,
		clickhouse.WithQueryID(id),
		clickhouse.WithSpan(span.SpanContext()),
		clickhouse.WithProgress(func(p *clickhouse.Progress) {
			st.readRows.Add(p.Rows)
			st.readBytes.Add(p.Bytes)
		}),
		clickhouse.WithProfileInfo(func(p *clickhouse.ProfileInfo) {
			st.profileRows.Add(p.Rows)
		}),
	)
	return ctx, st
}

// Describe is what a statement's span says about it: its summary (the span
// name: "SELECT t_org.otel_logs") and the db.* attributes, the statement text
// sanitized. An `EXECUTE AS <user>` prefix is not the operation: the summary
// is the statement it runs.
func Describe(query string) (summary string, attrs []attribute.KeyValue) {
	op, table := dbtrace.Summarize(stripExecuteAs(query))
	attrs = []attribute.KeyValue{
		attribute.String("db.system.name", System),
		attribute.String("db.system", System),
	}
	if text := dbtrace.SanitizeSQL(query); text != "" {
		// db.statement is the pre-1.26 name of db.query.text.
		attrs = append(attrs, attribute.String("db.query.text", text), attribute.String("db.statement", text))
	}
	switch {
	case op == "":
		summary = "clickhouse"
	case table == "":
		summary = op
	default:
		summary = op + " " + table
	}
	if op != "" {
		attrs = append(attrs, attribute.String("db.operation.name", op))
	}
	if table != "" {
		attrs = append(attrs, attribute.String("db.collection.name", table))
	}
	attrs = append(attrs, attribute.String("db.query.summary", summary))
	return summary, attrs
}

// stripExecuteAs drops a leading `EXECUTE AS <user> ` (ClickHouse's
// impersonation prefix), quoted or not.
func stripExecuteAs(query string) string {
	q := strings.TrimLeft(query, " \t\r\n")
	const kw = "EXECUTE AS "
	if len(q) < len(kw) || !strings.EqualFold(q[:len(kw)], kw) {
		return query
	}
	rest := strings.TrimLeft(q[len(kw):], " ")
	if rest == "" {
		return query
	}
	if rest[0] == '`' || rest[0] == '"' {
		if i := strings.IndexByte(rest[1:], rest[0]); i >= 0 {
			return strings.TrimLeft(rest[i+2:], " \t\r\n")
		}
		return query
	}
	if i := strings.IndexAny(rest, " \t\r\n"); i > 0 {
		return strings.TrimLeft(rest[i:], " \t\r\n")
	}
	return query
}

func (st *statement) end(err error) {
	if st == nil || !st.ended.CompareAndSwap(false, true) {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.Int64(AttrReadRows, int64(st.readRows.Load())),
		attribute.Int64(AttrReadBytes, int64(st.readBytes.Load())),
	}
	if n := st.returned.Load(); n >= 0 {
		attrs = append(attrs, attribute.Int64("db.response.returned_rows", n))
	} else if n := st.profileRows.Load(); n > 0 {
		attrs = append(attrs, attribute.Int64("db.response.returned_rows", int64(n)))
	}
	st.span.SetAttributes(attrs...)
	if err != nil {
		RecordError(st.span, err)
	}
	st.span.End()
}

// RecordError marks span failed by err: the exception event, status Error
// with the message, error.type, and the ClickHouse code when there is one.
func RecordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(attribute.String("error.type", ErrorType(err)))
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		span.SetAttributes(attribute.String("db.response.status_code", strconv.Itoa(int(ex.Code))))
	}
}

// ErrorType is err's low-cardinality class for error.type: the ClickHouse
// exception name (TIMEOUT_EXCEEDED, QUOTA_EXCEEDED, ...), context_canceled,
// context_deadline_exceeded, or _OTHER.
func ErrorType(err error) string {
	var ex *clickhouse.Exception
	switch {
	case errors.As(err, &ex) && ex.Name != "":
		return ex.Name
	case errors.As(err, &ex):
		return "clickhouse_" + strconv.Itoa(int(ex.Code))
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	}
	return "_OTHER"
}

// tracedRows ends the statement's span when the caller is done reading.
type tracedRows struct {
	driver.Rows
	st *statement
	n  int64
}

func (r *tracedRows) Next() bool {
	ok := r.Rows.Next()
	if ok {
		r.n++
	}
	return ok
}

func (r *tracedRows) Close() error {
	err := r.Rows.Close()
	r.st.returned.Store(r.n)
	if rerr := r.Rows.Err(); rerr != nil {
		r.st.end(rerr)
	} else {
		r.st.end(err)
	}
	return err
}

// newQueryID is a random query id in UUID form, ClickHouse's own format.
func newQueryID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
