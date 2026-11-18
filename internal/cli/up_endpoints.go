package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"
)

// THE PAYOFF IN `forge env up`'S OWN OUTPUT.
//
// Every RPC a forge service serves is a span and a metric sample with no code
// (edge telemetry), and `forge obs endpoints <env>` turns that into p50, p95,
// p99 and an error rate per RPC. An evaluating agent called that table its one
// "this is good" moment, and it only ran the command because someone asked
// (forge eval 2026-11-17, feedback #1). So a stack that comes up with services
// ends with that table when it has served requests, and otherwise with one
// line naming the command and what it shows.
//
// NO SYNTHETIC REQUEST. Forge cannot send one that is both traced and free of
// side effects: /readyz and /healthz are plain HTTP, outside the RPC chain
// that records spans, and every RPC is authenticated by default and may write.
// An unauthenticated probe would put a 100%-error row in the table, which is
// worse than no table. The first `env up` of a fresh project therefore prints
// the line; once the app has served traffic (the frontend, a test, a curl),
// later runs print the table.
//
// IT NEVER COSTS THE RUN. One query, bounded by upEndpointsBudget, after the
// stack is up and its URLs are printed; any failure (no ClickStack, no
// hosted session, a slow query) prints the line instead.

// upEndpointsBudget bounds the one query. The stack is already up.
var upEndpointsBudget = 5 * time.Second

// upEndpointsTop is how many endpoints the table shows, busiest first.
const upEndpointsTop = 5

// upEndpointsQuery reads the env's busiest endpoints over the last hour, the
// same document `forge obs endpoints <env> --json` prints. A seam for tests.
var upEndpointsQuery = func(ctx context.Context, env string) ([]obsEndpointDoc, error) {
	cmd := newObsEndpointsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{env, "--json", "--top", strconv.Itoa(upEndpointsTop)})
	if err := cmd.ExecuteContext(ctx); err != nil {
		return nil, err
	}
	var doc obsEndpointsDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		return nil, fmt.Errorf("read obs endpoints: %w", err)
	}
	return doc.Endpoints, nil
}

// printUpEndpoints prints the per-RPC table for env, or the line naming the
// command when there is nothing to show yet.
func printUpEndpoints(ctx context.Context, w io.Writer, env string) {
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upEndpointsBudget)
	defer cancel()
	rows, err := upEndpointsQuery(qctx, env)
	cmd := fmt.Sprintf("%s obs endpoints %s", Name(), env)
	if err != nil || len(rows) == 0 {
		fmt.Fprintf(w, "\nEvery RPC this stack serves is traced and measured, with no code: `%s` shows p50/p95/p99 and the error rate per RPC once it has served requests.\n", cmd)
		return
	}
	fmt.Fprintf(w, "\nPer-RPC latency and errors over the last hour, no instrumentation code (`%s` for more):\n", cmd)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ENDPOINT\tREQUESTS\tERRORS\tP50\tP95\tP99")
	for _, e := range rows {
		fmt.Fprintf(tw, "  %s\t%.0f\t%s%%\t%s\t%s\t%s\n", e.Name, e.Requests,
			strconv.FormatFloat(e.ErrorRatio*100, 'f', 1, 64), ms(e.P50Ms), ms(e.P95Ms), ms(e.P99Ms))
	}
	_ = tw.Flush()
}
