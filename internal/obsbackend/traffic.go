package obsbackend

import (
	"context"
	"strings"

	"github.com/reliant-labs/forge/pkg/obsquery"
)

// TrafficRequest is one `forge obs traffic` read: totals and top pages, or
// one table grouped By, narrowed to Class and Site (obsquery.TrafficQuery).
type TrafficRequest struct {
	Narrow
	By    obsquery.TrafficBy
	Class string
	Site  string
	Limit int
}

// TrafficResult is what a traffic read found. Totals is set for the default
// read; Rows for a grouping by page, status, referrer or country; Agents for
// --by agent. Table is the table the counts came from (obsquery.TrafficTable,
// or obsquery.TrafficRollupTable for a long range).
type TrafficResult struct {
	Table      string
	Totals     []obsquery.TrafficTotal
	Rows       []obsquery.TrafficRow
	Agents     []obsquery.TrafficAgent
	Truncated  bool
	Statements []Statement
}

// errNoEdgeLogsLocally is the local backend's answer: the CDN writes the
// edge request logs, and a local stack serves the site with none in front.
const errNoEdgeLogsLocally = "edge request logs exist only on Reliant hosting: the platform's CDN writes them, and `forge env up` serves the site with no CDN in front of it; read a hosted env (forge obs traffic prod)"

// Traffic implements TrafficReader. A local ClickStack has no edge tables, so
// it answers "could not look", with the reason.
func (l Local) Traffic(_ context.Context, _ TrafficRequest) (TrafficResult, error) {
	return TrafficResult{}, Undetermined("%s", errNoEdgeLogsLocally)
}

func (r TrafficRequest) query() obsquery.TrafficQuery {
	return obsquery.TrafficQuery{By: r.By, Class: r.Class, Site: r.Site, Limit: r.Limit, Range: r.Range.To.Sub(r.Range.From)}
}

// Traffic implements TrafficReader: each compiled statement runs as a sql
// panel through the control plane's QueryPanel (the path `forge obs query
// --sql` takes), which binds the env's {env_ids} from the verified scope and
// runs it on the org's tenant database. No traffic-specific rpc is needed.
func (c Cloud) Traffic(ctx context.Context, req TrafficRequest) (TrafficResult, error) {
	if len(req.Filters) > 0 {
		return TrafficResult{}, Wrong("--filter narrows telemetry attributes and does not apply to edge traffic; use --class and --site")
	}
	sts, err := req.query().Statements()
	if err != nil {
		return TrafficResult{}, compileErr(err)
	}
	limit := req.Limit
	if limit == 0 {
		limit = obsquery.DefaultTrafficLimit
	}
	var res TrafficResult
	for _, st := range sts {
		res.Table = st.Table
		qr, err := c.Query(ctx, QueryRequest{Narrow: req.Narrow, SQL: st.SQL, Limit: limit})
		res.Statements = append(res.Statements, relabel(qr.Statements, "traffic "+st.Purpose)...)
		if err != nil {
			return res, trafficErr(err)
		}
		if req.Explain {
			continue
		}
		res.Truncated = res.Truncated || qr.Truncated
		if err := res.read(st.Purpose, RowsOf(Result{Rows: qr.Rows})); err != nil {
			return res, Undetermined("reading the %s of the edge traffic", st.Purpose).Cause(err)
		}
	}
	return res, nil
}

func relabel(sts []Statement, purpose string) []Statement {
	out := make([]Statement, len(sts))
	for i, s := range sts {
		s.Purpose = purpose
		out[i] = s
	}
	return out
}

// read parses one statement's rows into the field its purpose fills.
func (r *TrafficResult) read(purpose string, rows obsquery.Rows) error {
	var err error
	switch purpose {
	case "totals":
		r.Totals, err = obsquery.ParseTrafficTotals(rows)
	case string(obsquery.TrafficByAgent):
		r.Agents, err = obsquery.ParseTrafficAgents(rows)
	default:
		r.Rows, err = obsquery.ParseTrafficRows(rows)
	}
	return err
}

// trafficErr names the likely cause when the tenant has no edge tables: a
// control plane that does not write edge request logs yet.
func trafficErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, obsquery.TrafficTable) && (strings.Contains(msg, "UNKNOWN_TABLE") || strings.Contains(strings.ToLower(msg), "unknown table") || strings.Contains(msg, "doesn't exist") || strings.Contains(msg, "does not exist")) {
		return Undetermined("this org's telemetry has no edge request logs table yet: the control plane writes edge logs for envs on Reliant hosting once it ships them").Cause(err)
	}
	return err
}
