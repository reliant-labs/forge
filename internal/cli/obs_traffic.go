package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/internal/obsout"
	"github.com/reliant-labs/forge/pkg/obsquery"
)

const schemaObsTraffic = "forge.obs.traffic/v1"

// obsTrafficSchema is the document `forge obs traffic --json` writes, a
// public surface: fields are added, never renamed or removed, within v1.
var obsTrafficSchema = obsout.Schema{
	Name: schemaObsTraffic,
	Fields: []string{
		"scope", "scope.env", "scope.backend", "scope.env_id", "scope.from", "scope.to", "scope.services", "scope.instance", "scope.instance.id", "scope.instance.label", "scope.instance.user",
		"query_mode", "by", "class", "site", "table",
		"totals", "totals.site", "totals.requests", "totals.cache_hit_ratio", "totals.status_4xx", "totals.status_5xx", "totals.bytes",
		"rows", "rows.key", "rows.requests", "rows.humans", "rows.agents", "rows.crawlers", "rows.bytes",
		"agents", "agents.agent", "agents.operator", "agents.class", "agents.verified", "agents.requests", "agents.markdown", "agents.llms_txt", "agents.top_page",
		"statements", "statements.purpose", "statements.sql", "statements.params.*",
		"suggested",
	},
}

// obsTrafficDoc is forge.obs.traffic/v1. by is the grouping read: "" for the
// default read (totals, then rows by page). totals is set only by the default
// read, rows by every grouping but agent, agents only by --by agent; each is
// [] (never null) when it was read and empty, and absent when not read. table
// is the tenant table the counts came from: edge_requests, or the daily
// rollup edge_requests_1d for a range of a week or more.
type obsTrafficDoc struct {
	obsout.Head
	Scope      obsout.Scope             `json:"scope"`
	QueryMode  string                   `json:"query_mode"`
	By         string                   `json:"by"`
	Class      string                   `json:"class,omitempty"`
	Site       string                   `json:"site,omitempty"`
	Table      string                   `json:"table,omitempty"`
	Totals     []obsquery.TrafficTotal  `json:"totals,omitempty"`
	Rows       *[]obsquery.TrafficRow   `json:"rows,omitempty"`
	Agents     *[]obsquery.TrafficAgent `json:"agents,omitempty"`
	Statements []obsbackend.Statement   `json:"statements,omitempty"`
	obsout.Page
	Suggested []string `json:"suggested,omitempty"`
}

type obsTrafficOpts struct {
	obsCommon
	by, class, site string
}

func newObsTrafficCmd() *cobra.Command {
	var o obsTrafficOpts
	cmd := &cobra.Command{
		Use:   "traffic [env]",
		Short: "Edge traffic of a hosted static site: pages, statuses, bots and AI agents",
		Long: `Read what the CDN in front of the env's hosted static site served, from its
request logs: every request, cache hits and AI agents included, which the
browser SDK never sees. [env] defaults to dev; --since defaults to 24h.

The default read is the totals per site (requests, cache-hit ratio, 4xx, 5xx,
bytes) and the top pages, split into humans (browsers), agents (AI search and
assistant fetches, tools) and crawlers (search and AI crawlers, monitors, other
bots). A request of an unknown user agent counts in REQUESTS only.

  ` + Name() + ` obs traffic prod --since 7d
  ` + Name() + ` obs traffic prod --by agent --class ai
  ` + Name() + ` obs traffic prod --by status --site web --json
  ` + Name() + ` obs traffic prod --by referrer --class human

--by page|status|agent|referrer|country groups the table instead. --by agent
lists each agent's operator, class and whether the request came from the
operator's published IP ranges (VERIFIED): a "no" row is a request that only
claimed the agent's user agent. .md and LLMS.TXT count its fetches of Markdown
twins and llms.txt.

--class narrows to a user-agent class (browser, search_crawler, ai_crawler,
ai_search, ai_user, tool, monitor, other_bot, unknown) or a group: human, agent,
crawler, ai (the three ai_ classes), bot (every class but browser and unknown).
--site narrows to one frontend.

Edge request logs exist only on Reliant hosting, where the platform's CDN
writes them; a local env answers "could not look". Counts are weighted by each
row's sample weight, so they stay true when ingest is degraded under a quota.
A range of a week or more reads the daily rollup (whole UTC days) where it
answers the question. Edge logs carry no visitor identity: unique visitors
stay the browser SDK's number (` + "`" + Name() + ` obs users` + "`" + `).

` + obsTrafficSchema.Help(),
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runObsTraffic(cmd.Context(), defaultObsDeps(cmd), obsEnvArg(args, 0), o)
		},
	}
	o.register(cmd, 24*time.Hour, obsquery.DefaultTrafficLimit)
	f := cmd.Flags()
	f.StringVar(&o.by, "by", "", "group the table by page, status, agent, referrer or country (default: totals and top pages)")
	f.StringVar(&o.class, "class", "", "only this user-agent class or group ("+strings.Join(obsquery.TrafficClassNames(), ", ")+")")
	f.StringVar(&o.site, "site", "", "only this frontend")
	return cmd
}

func (o obsTrafficOpts) validate() error {
	if o.out.Limit < 1 || o.out.Limit > obsquery.MaxTrafficLimit {
		return obsbackend.Wrong("--limit must be 1-%d", obsquery.MaxTrafficLimit)
	}
	if len(o.filters) > 0 {
		return obsbackend.Wrong("--filter narrows telemetry attributes and does not apply to edge traffic; use --class and --site")
	}
	if len(o.services) > 0 {
		return obsbackend.Wrong("--service names a telemetry service; edge traffic is per site: use --site")
	}
	switch obsquery.TrafficBy(o.by) {
	case "", obsquery.TrafficByPage, obsquery.TrafficByStatus, obsquery.TrafficByAgent, obsquery.TrafficByReferrer, obsquery.TrafficByCountry:
	default:
		return obsbackend.Wrong("--by %q: use page, status, agent, referrer or country", o.by)
	}
	if _, err := obsquery.TrafficClasses(o.class); err != nil {
		return obsbackend.Wrong("--class %q: use one of %s", o.class, strings.Join(obsquery.TrafficClassNames(), ", "))
	}
	return nil
}

// trafficReader is the backend's traffic side; a backend without one cannot look.
func trafficReader(b obsbackend.Backend) (obsbackend.TrafficReader, error) {
	r, ok := b.(obsbackend.TrafficReader)
	if !ok {
		return nil, obsbackend.Undetermined("the %s backend serves no edge traffic", b.Info().Backend)
	}
	return r, nil
}

func runObsTraffic(ctx context.Context, d obsDeps, env string, o obsTrafficOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}
	fields, err := o.out.Parsed(obsTrafficSchema)
	if err != nil {
		return obsFail(d, o.out.JSON, schemaObsTraffic, err)
	}
	fail := func(err error) error { return obsFail(d, o.out.JSON, schemaObsTraffic, err) }
	if err := o.validate(); err != nil {
		return fail(err)
	}
	n, err := o.narrow(d.Now())
	if err != nil {
		return fail(err)
	}
	if n.Range.To.Sub(n.Range.From) > obsquery.MaxRangeTraffic {
		return fail(obsbackend.Wrong("the range is longer than the %s edge traffic is kept", obsquery.MaxRangeTraffic))
	}
	b, _, err := o.open(ctx, d, env, "traffic")
	if err != nil {
		return fail(err)
	}
	r, err := trafficReader(b)
	if err != nil {
		return fail(err)
	}
	by := obsquery.TrafficBy(o.by)
	res, err := r.Traffic(ctx, obsbackend.TrafficRequest{Narrow: n, By: by, Class: o.class, Site: o.site, Limit: o.out.Limit})
	if err != nil {
		return fail(err)
	}
	doc := trafficDoc(obsScope(b.Info(), n), by, o.class, o.site, res)
	if n.Explain {
		doc.Statements = res.Statements
	}
	doc.Page = obsout.Page{Truncated: res.Truncated}
	doc.Suggested = o.inst.carrySuggested(trafficSuggestions(env, by))
	doc.Stamp(schemaObsTraffic, nil)
	if done, err := o.out.Emit(d.Stdout, obsTrafficSchema, doc, fields); done || err != nil {
		return err
	}
	if n.Explain {
		explainText(d.Stdout, res.Statements)
		return nil
	}
	renderTraffic(d.Stdout, doc)
	return nil
}

// trafficDoc builds the document from a result: each section the grouping
// reads is present, [] when empty.
func trafficDoc(scope obsout.Scope, by obsquery.TrafficBy, class, site string, res obsbackend.TrafficResult) obsTrafficDoc {
	doc := obsTrafficDoc{Scope: scope, QueryMode: QueryModeStructured, By: string(by), Class: class, Site: site, Table: res.Table}
	switch by {
	case "":
		doc.Totals = res.Totals
		if doc.Totals == nil {
			doc.Totals = []obsquery.TrafficTotal{}
		}
		fallthrough
	case obsquery.TrafficByPage, obsquery.TrafficByStatus, obsquery.TrafficByReferrer, obsquery.TrafficByCountry:
		rows := append([]obsquery.TrafficRow{}, res.Rows...)
		doc.Rows = &rows
	case obsquery.TrafficByAgent:
		agents := append([]obsquery.TrafficAgent{}, res.Agents...)
		doc.Agents = &agents
	}
	return doc
}

func trafficSuggestions(env string, by obsquery.TrafficBy) []string {
	switch by {
	case "", obsquery.TrafficByPage:
		return []string{
			Name() + " obs traffic " + env + " --by agent --class ai",
			Name() + " obs traffic " + env + " --by status",
		}
	case obsquery.TrafficByAgent:
		return []string{Name() + " obs traffic " + env + " --by agent --class ai"}
	}
	return nil
}

func renderTraffic(w io.Writer, doc obsTrafficDoc) {
	if doc.By == "" {
		if len(doc.Totals) == 0 {
			fmt.Fprintln(w, "no edge requests in the range")
			return
		}
		rows := make([][]string, len(doc.Totals))
		for i, t := range doc.Totals {
			rows[i] = []string{orDash(t.Site), thousands(int(t.Requests)), "—", trafficPercent(t.CacheHitRatio),
				thousands(int(t.Status4xx)), thousands(int(t.Status5xx)), trafficBytes(t.Bytes)}
		}
		obsout.Table(w, []string{"SITE", "REQUESTS", "VISITORS*", "CACHE HIT", "4xx", "5xx", "BYTES"}, rows, 40)
		fmt.Fprintln(w)
	}
	switch {
	case doc.Agents != nil:
		renderTrafficAgents(w, *doc.Agents)
	case doc.Rows != nil:
		renderTrafficRows(w, trafficKeyHeader(doc.By), *doc.Rows)
	}
	if doc.By == "" {
		fmt.Fprintln(w, "\n(VISITORS*: edge logs carry no visitor identity; unique visitors are the browser SDK's: obs users)")
	}
	if doc.Truncated {
		fmt.Fprintln(w, "\n(more rows: raise --limit)")
	}
}

func trafficKeyHeader(by string) string {
	switch obsquery.TrafficBy(by) {
	case obsquery.TrafficByStatus:
		return "STATUS"
	case obsquery.TrafficByReferrer:
		return "REFERRER"
	case obsquery.TrafficByCountry:
		return "COUNTRY"
	}
	return "TOP PAGES"
}

func renderTrafficRows(w io.Writer, key string, in []obsquery.TrafficRow) {
	if len(in) == 0 {
		fmt.Fprintln(w, "no edge requests in the range")
		return
	}
	rows := make([][]string, len(in))
	for i, r := range in {
		rows[i] = []string{r.Key, thousands(int(r.Requests)), thousands(int(r.Humans)), thousands(int(r.Agents)), thousands(int(r.Crawlers))}
	}
	obsout.Table(w, []string{key, "REQUESTS", "HUMANS", "AGENTS", "CRAWLERS"}, rows, 60)
}

func renderTrafficAgents(w io.Writer, in []obsquery.TrafficAgent) {
	if len(in) == 0 {
		fmt.Fprintln(w, "no requests from a known agent in the range")
		return
	}
	rows := make([][]string, len(in))
	for i, a := range in {
		verified := "no"
		if a.Verified {
			verified = "yes"
		}
		rows[i] = []string{a.Agent, orDash(a.Operator), a.Class, verified, thousands(int(a.Requests)),
			strconv.FormatUint(a.Markdown, 10), strconv.FormatUint(a.LLMSTxt, 10), orDash(a.TopPage)}
	}
	obsout.Table(w, []string{"AGENT", "OPERATOR", "CLASS", "VERIFIED", "REQUESTS", ".md", "llms.txt", "TOP PAGE"}, rows, 40)
}

func trafficPercent(r float64) string {
	return strconv.FormatFloat(r*100, 'f', 0, 64) + "%"
}

// trafficBytes writes n in binary units: 1.9 GiB.
func trafficBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
