package obsbackend_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/internal/obsbackend/obstest"
	"github.com/reliant-labs/forge/pkg/releasehealth"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// TestAIAgainstClickHouse runs the AI panel through the real
// clickhouse-client invocation over recorded GenAI spans: model calls of two
// operations, a cancelled call that is no failure, tool calls by name, a
// version filter, and spans that are not AI calls.
func TestAIAgainstClickHouse(t *testing.T) {
	testkit.Slow(t, "boots a ClickHouse container and runs the AI panel against it")
	runner, write := obstest.Start(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	i := 0
	span := func(svc, version, status string, dur time.Duration, attrs string) string {
		i++
		return fmt.Sprintf(`INSERT INTO otel_traces (Timestamp, TraceId, SpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, SpanAttributes, Duration, StatusCode)
SELECT toDateTime64('%s', 9), 't%d', 's%d', 'x', 'Internal', '%s', map('deployment.environment.name','dev','service.version','%s'), map(%s), %d, '%s'`,
			now.Add(-time.Duration(i)*time.Second).Format("2006-01-02 15:04:05.000000000"), i, i, svc, version, attrs, dur.Nanoseconds(), status)
	}
	chat := `'gen_ai.operation.name','chat'`
	var stmts []string
	for range 8 {
		stmts = append(stmts, span("agent", "v2", "Unset", 100*time.Millisecond, chat))
	}
	stmts = append(stmts,
		span("agent", "v2", "Error", 100*time.Millisecond, chat),
		span("agent", "v2", "Error", 100*time.Millisecond, chat+`,'error.type','canceled'`),
		span("agent", "v2", "Unset", 10*time.Millisecond, `'gen_ai.operation.name','embeddings'`),
		span("agent", "v2", "Error", 5*time.Millisecond, `'gen_ai.operation.name','execute_tool','gen_ai.tool.name','search'`),
		span("agent", "v2", "Unset", 5*time.Millisecond, `'gen_ai.operation.name','execute_tool','gen_ai.tool.name','search'`),
		span("agent", "v2", "Unset", 5*time.Millisecond, `'gen_ai.operation.name','execute_tool','gen_ai.tool.name','grep'`),
		span("agent", "v1", "Error", 100*time.Millisecond, chat),                // an older version
		span("agent", "v2", "Error", 100*time.Millisecond, `'http.route','/x'`), // not an AI call
	)
	write(t, strings.Join(stmts, ";\n")+";")

	l := obsbackend.Local{Env: "dev", Runner: runner}
	n := obsbackend.Narrow{Range: obsbackend.Range{From: now.Add(-time.Hour), To: now.Add(time.Minute)}}
	res, err := l.AI(context.Background(), obsbackend.AIRequest{Narrow: n, Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Services) != 1 {
		t.Fatalf("services = %+v", res.Services)
	}
	s := res.Services[0]
	// Eleven model calls of v2 (ten chat, one embeddings), one failure (the
	// cancellation is none); the chat p95, the bigger operation's, is 100ms.
	want := releasehealth.AIWindow{Model: releasehealth.AICalls{Calls: 11, Errors: 1}, Tool: releasehealth.AICalls{Calls: 3, Errors: 1}, ModelP95Ms: 100}
	if s.Service != "agent" || s.Window != want {
		t.Errorf("window = %+v, want %+v", s.Window, want)
	}
	if s.Tools["search"] != (releasehealth.AICalls{Calls: 2, Errors: 1}) || s.Tools["grep"] != (releasehealth.AICalls{Calls: 1}) {
		t.Errorf("tools = %+v", s.Tools)
	}

	all, err := l.AI(context.Background(), obsbackend.AIRequest{Narrow: n})
	if err != nil {
		t.Fatal(err)
	}
	if got := all.Services[0].Window.Model; got != (releasehealth.AICalls{Calls: 12, Errors: 2}) {
		t.Errorf("every version: model = %+v", got)
	}
}
