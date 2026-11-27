package obsquery

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Edge traffic over a hosted static site's CDN request logs: the tenant
// tables edge_requests (one row per request the CDN logged, sampled under a
// quota, so every count is sum(SampleWeight)) and edge_requests_1d (its daily
// rollup, Requests = sum(SampleWeight)). The control plane writes them for an
// env bound to Reliant hosting; a local stack has neither, so a local backend
// never runs these statements.
//
// Every statement here is ONE read-only SELECT in the sql-panel form
// (dashspec.KindSQL), bound by SQLPanel.Compile like any sql panel ({from},
// {to}, {env_ids}): `forge obs traffic` runs it through the backend's sql-panel
// path (the control plane's QueryPanel), `forge obs query` runs it as written,
// and the frontend dashboard's Traffic and Bots & agents groups draw it. Values
// a caller chooses (a class, a site) are never parameters, which the control
// plane does not bind: each is checked against a closed set or a name pattern
// and only then written into the statement as a quoted literal.
const (
	// TrafficTable is the per-request edge table.
	TrafficTable = "edge_requests"
	// TrafficRollupTable is its daily rollup.
	TrafficRollupTable = "edge_requests_1d"
	// TrafficRollupAfter is the shortest range read from the rollup, when the
	// question needs only its columns: past a week a per-request scan costs
	// far more than whole days answer, and a week of days is fine-grained
	// enough. The rollup counts whole UTC days, so the range is widened to
	// the days it touches.
	TrafficRollupAfter = 7 * 24 * time.Hour
	// DefaultTrafficLimit and MaxTrafficLimit bound a ranked table's rows.
	DefaultTrafficLimit = 20
	MaxTrafficLimit     = 200
	// MaxRangeTraffic is the longest range a traffic read takes: the paid
	// retention of the rollup.
	MaxRangeTraffic = 400 * 24 * time.Hour
)

// TrafficBy is what a ranked traffic table is grouped by.
type TrafficBy string

// The groupings of `forge obs traffic --by`.
const (
	TrafficByPage     TrafficBy = "page"
	TrafficByStatus   TrafficBy = "status"
	TrafficByAgent    TrafficBy = "agent"
	TrafficByReferrer TrafficBy = "referrer"
	TrafficByCountry  TrafficBy = "country"
)

// TrafficBys lists every grouping, for help text.
var TrafficBys = []TrafficBy{TrafficByPage, TrafficByStatus, TrafficByAgent, TrafficByReferrer, TrafficByCountry}

// The user-agent classes the control plane writes into UaClass
// (pkg/agentua's Class values).
var trafficClasses = []string{"browser", "search_crawler", "ai_crawler", "ai_search", "ai_user", "tool", "monitor", "other_bot", "unknown"}

// The three audiences a ranked table splits requests into. A request whose
// class is unknown is in requests and in none of them.
var (
	trafficHumans   = []string{"browser"}
	trafficCrawlers = []string{"search_crawler", "ai_crawler", "other_bot", "monitor"}
	trafficAgents   = []string{"ai_search", "ai_user", "tool"}
)

// trafficClassGroups are the names --class takes beside the classes
// themselves.
var trafficClassGroups = map[string][]string{
	"human":   trafficHumans,
	"crawler": trafficCrawlers,
	"agent":   trafficAgents,
	"ai":      {"ai_crawler", "ai_search", "ai_user"},
	"bot":     {"search_crawler", "ai_crawler", "ai_search", "ai_user", "tool", "monitor", "other_bot"},
}

// TrafficClassNames lists every value --class takes: the groups, then the
// classes.
func TrafficClassNames() []string {
	var groups []string
	for g := range trafficClassGroups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return append(groups, trafficClasses...)
}

// TrafficClasses resolves a --class value to the UaClass values it selects;
// "" selects every class (nil).
func TrafficClasses(name string) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	if g, ok := trafficClassGroups[name]; ok {
		return g, nil
	}
	for _, c := range trafficClasses {
		if c == name {
			return []string{c}, nil
		}
	}
	return nil, invalid("class %q: use one of %s", name, strings.Join(TrafficClassNames(), ", "))
}

// trafficSite is the form of a frontend name, the Site column's value.
var trafficSite = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// TrafficScannerRegex matches the paths of secret and exploit probes: what
// scanners request from every host whatever it serves (.env files, VCS and
// cloud credentials, the Vite dev server's /@fs/, /proc/self/environ, PHP and
// WordPress paths, private keys). A 404 on one is noise; a 404 on any other
// path is a broken link. It is an RE2 pattern for ClickHouse's match().
const TrafficScannerRegex = `(?i)(\.env|/\.git|/\.aws|/\.ssh|/\.docker|/\.netlify|/\.dev\.vars|/\.zshrc|/\.bashrc|/@fs/|/proc/|/wp-|/wordpress|/phpmyadmin|/cgi-bin|\.php$|server\.key|credentials|\.(pem|bak|sql)$)`

// trafficScanner is the predicate, with the pattern as a ClickHouse literal.
func trafficScanner() string { return "match(Path, " + sqlString(TrafficScannerRegex) + ")" }

// sqlString quotes s as a ClickHouse string literal.
func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func sqlStrings(vs []string) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = sqlString(v)
	}
	return "(" + strings.Join(q, ", ") + ")"
}

// trafficEnvScope is the environments in scope, as every obsquery statement
// binds them.
const trafficEnvScope = "(empty({env_ids:Array(String)}) OR has({env_ids:Array(String)}, EnvId))"

// trafficSource is the table a statement reads, with the expressions that
// count over it.
type trafficSource struct {
	table, weight, bytes, when string
	rollup                     bool
}

var (
	trafficRaw = trafficSource{table: TrafficTable, weight: "SampleWeight", bytes: "Bytes * SampleWeight",
		when: "Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}"}
	trafficDaily = trafficSource{table: TrafficRollupTable, weight: "Requests", bytes: "Bytes", rollup: true,
		when: "Day >= toDate({from:DateTime64(9)}, 'UTC') AND Day <= toDate({to:DateTime64(9)}, 'UTC')"}
)

func (s trafficSource) count(classes []string) string {
	if classes == nil {
		return "sum(" + s.weight + ")"
	}
	return "sumIf(" + s.weight + ", UaClass IN " + sqlStrings(classes) + ")"
}

// TrafficQuery is one `forge obs traffic` read: totals and top pages, or a
// table grouped By one dimension, narrowed to Class and Site.
type TrafficQuery struct {
	// By groups the ranked table; "" reads the totals and the top pages.
	By TrafficBy
	// Class is a --class value (TrafficClassNames); "" is every class.
	Class string
	// Site narrows to one frontend; "" is every site of the env.
	Site string
	// Limit is the ranked table's row cap; 0 is DefaultTrafficLimit.
	Limit int
	// Range is the length of the range read, which picks the table.
	Range time.Duration
}

// TrafficStatement is one compiled read of a TrafficQuery.
type TrafficStatement struct {
	// Purpose names what the statement reads: "totals", or the grouping.
	Purpose string
	SQL     string
	// Table is the table it reads (TrafficTable or TrafficRollupTable).
	Table string
}

// Rollup reports whether the query reads the daily rollup.
func (q TrafficQuery) Rollup() bool {
	if q.Range < TrafficRollupAfter {
		return false
	}
	switch q.By {
	case "", TrafficByPage, TrafficByStatus:
		return true
	}
	// The rollup carries no operator, content kind, referrer or country.
	return false
}

func (q TrafficQuery) limit() int {
	if q.Limit == 0 {
		return DefaultTrafficLimit
	}
	return q.Limit
}

// Statements compiles the query: totals then pages for the default read,
// otherwise the one grouped table.
func (q TrafficQuery) Statements() ([]TrafficStatement, error) {
	switch q.By {
	case "", TrafficByPage, TrafficByStatus, TrafficByAgent, TrafficByReferrer, TrafficByCountry:
	default:
		return nil, invalid("by %q: use one of %s", q.By, joinBys())
	}
	if q.Limit < 0 || q.Limit > MaxTrafficLimit {
		return nil, invalid("limit must be 1-%d", MaxTrafficLimit)
	}
	if q.Range > MaxRangeTraffic {
		return nil, invalid("the range is longer than the %s edge traffic is kept", MaxRangeTraffic)
	}
	classes, err := TrafficClasses(q.Class)
	if err != nil {
		return nil, err
	}
	if q.Site != "" && !trafficSite.MatchString(q.Site) {
		return nil, invalid("site %q is not a frontend name", q.Site)
	}
	src := trafficRaw
	if q.Rollup() {
		src = trafficDaily
	}
	where := []string{trafficEnvScope, src.when}
	if classes != nil {
		where = append(where, "UaClass IN "+sqlStrings(classes))
	}
	if q.Site != "" {
		where = append(where, "Site = "+sqlString(q.Site))
	}
	cond := strings.Join(where, "\n  AND ")
	var out []TrafficStatement
	add := func(purpose, sql string) {
		out = append(out, TrafficStatement{Purpose: purpose, SQL: sql, Table: src.table})
	}
	switch q.By {
	case "":
		add("totals", trafficTotalsSQL(src, cond))
		add(string(TrafficByPage), trafficRankSQL(src, cond, "Path", q.limit()))
	case TrafficByPage:
		add(string(q.By), trafficRankSQL(src, cond, "Path", q.limit()))
	case TrafficByStatus:
		add(string(q.By), trafficRankSQL(src, cond, "toString(Status)", q.limit()))
	case TrafficByReferrer:
		add(string(q.By), trafficRankSQL(src, cond, "if(RefererHost = '', '(direct)', RefererHost)", q.limit()))
	case TrafficByCountry:
		add(string(q.By), trafficRankSQL(src, cond, "if(Country = '', '(unknown)', Country)", q.limit()))
	case TrafficByAgent:
		add(string(q.By), trafficAgentsSQL(src, cond, q.limit()))
	}
	return out, nil
}

func joinBys() string {
	s := make([]string, len(TrafficBys))
	for i, b := range TrafficBys {
		s[i] = string(b)
	}
	return strings.Join(s, ", ")
}

// TrafficTotalsColumns are trafficTotalsSQL's columns, in order.
var TrafficTotalsColumns = []string{"site", "requests", "cache_hit_ratio", "status_4xx", "status_5xx", "bytes"}

func trafficTotalsSQL(src trafficSource, cond string) string {
	statusCol := "Status"
	return fmt.Sprintf(`SELECT Site AS site, sum(%[1]s) AS requests,
       round(sumIf(%[1]s, CacheStatus = 'hit') / greatest(sum(%[1]s), 1), 4) AS cache_hit_ratio,
       sumIf(%[1]s, %[3]s >= 400 AND %[3]s < 500) AS status_4xx,
       sumIf(%[1]s, %[3]s >= 500) AS status_5xx,
       sum(%[2]s) AS bytes
FROM %[4]s
WHERE %[5]s
GROUP BY site ORDER BY requests DESC, site`, src.weight, src.bytes, statusCol, src.table, cond)
}

// TrafficRankColumns are trafficRankSQL's columns, in order.
var TrafficRankColumns = []string{"key", "requests", "humans", "agents", "crawlers", "bytes"}

func trafficRankSQL(src trafficSource, cond, key string, limit int) string {
	return fmt.Sprintf(`SELECT %[1]s AS key, %[2]s AS requests, %[3]s AS humans, %[4]s AS agents, %[5]s AS crawlers, sum(%[6]s) AS bytes
FROM %[7]s
WHERE %[8]s
GROUP BY key ORDER BY requests DESC, key LIMIT %[9]d`,
		key, src.count(nil), src.count(trafficHumans), src.count(trafficAgents), src.count(trafficCrawlers), src.bytes, src.table, cond, limit)
}

// TrafficAgentColumns are trafficAgentsSQL's columns, in order. A claimed
// agent whose request came from outside its operator's published ranges is
// its own row, verified "no", beside the verified one.
var TrafficAgentColumns = []string{"agent", "operator", "class", "verified", "requests", "markdown", "llms_txt", "top_page"}

func trafficAgentsSQL(src trafficSource, cond string, limit int) string {
	return fmt.Sprintf(`SELECT Agent AS agent, any(AgentOperator) AS operator, UaClass AS class, if(AgentVerified, 'yes', 'no') AS verified,
       sum(%[1]s) AS requests,
       sumIf(%[1]s, ContentKind = 'markdown') AS markdown,
       sumIf(%[1]s, ContentKind = 'llms') AS llms_txt,
       arrayElement(topKWeighted(1)(Path, %[1]s), 1) AS top_page
FROM %[2]s
WHERE %[3]s
  AND Agent != ''
GROUP BY agent, class, verified ORDER BY requests DESC, agent LIMIT %[4]d`, src.weight, src.table, cond, limit)
}

// TrafficTotal is one site's totals.
type TrafficTotal struct {
	Site          string  `json:"site"`
	Requests      uint64  `json:"requests"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	Status4xx     uint64  `json:"status_4xx"`
	Status5xx     uint64  `json:"status_5xx"`
	Bytes         uint64  `json:"bytes"`
}

// TrafficRow is one row of a ranked table: Key is the page, status,
// referrer host or country.
type TrafficRow struct {
	Key      string `json:"key"`
	Requests uint64 `json:"requests"`
	Humans   uint64 `json:"humans"`
	Agents   uint64 `json:"agents"`
	Crawlers uint64 `json:"crawlers"`
	Bytes    uint64 `json:"bytes"`
}

// TrafficAgent is one row of the by-agent table.
type TrafficAgent struct {
	Agent    string `json:"agent"`
	Operator string `json:"operator"`
	Class    string `json:"class"`
	Verified bool   `json:"verified"`
	Requests uint64 `json:"requests"`
	Markdown uint64 `json:"markdown"`
	LLMSTxt  uint64 `json:"llms_txt"`
	TopPage  string `json:"top_page"`
}

// ParseTrafficTotals reads a totals statement's rows.
func ParseTrafficTotals(rows Rows) ([]TrafficTotal, error) {
	defer rows.Close()
	var out []TrafficTotal
	for rows.Next() {
		var t TrafficTotal
		if err := rows.Scan(&t.Site, &t.Requests, &t.CacheHitRatio, &t.Status4xx, &t.Status5xx, &t.Bytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ParseTrafficRows reads a ranked statement's rows.
func ParseTrafficRows(rows Rows) ([]TrafficRow, error) {
	defer rows.Close()
	var out []TrafficRow
	for rows.Next() {
		var r TrafficRow
		if err := rows.Scan(&r.Key, &r.Requests, &r.Humans, &r.Agents, &r.Crawlers, &r.Bytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ParseTrafficAgents reads a by-agent statement's rows.
func ParseTrafficAgents(rows Rows) ([]TrafficAgent, error) {
	defer rows.Close()
	var out []TrafficAgent
	for rows.Next() {
		var a TrafficAgent
		var verified string
		if err := rows.Scan(&a.Agent, &a.Operator, &a.Class, &verified, &a.Requests, &a.Markdown, &a.LLMSTxt, &a.TopPage); err != nil {
			return nil, err
		}
		a.Verified = verified == "yes"
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── dashboard panels ────────────────────────────────────────────────────
//
// The frontend dashboard's Traffic and Bots & agents groups, for an env with
// a hosted static frontend. Each reads the per-request table over the
// dashboard's range, every class counted by sum(SampleWeight).

func trafficPanelWhere(extra ...string) string {
	return strings.Join(append([]string{trafficEnvScope, trafficRaw.when}, extra...), "\n  AND ")
}

// TrafficClassAudience labels a UaClass as the audience a ranked table
// counts it in (humans, agents, crawlers, unknown).
func trafficAudienceExpr() string {
	return fmt.Sprintf("multiIf(UaClass IN %s, 'humans', UaClass IN %s, 'agents', UaClass IN %s, 'crawlers', 'unknown')",
		sqlStrings(trafficHumans), sqlStrings(trafficAgents), sqlStrings(trafficCrawlers))
}

// TrafficSummarySQL is one row of figures: requests, cache-hit ratio, 4xx,
// 5xx and bytes across the env's sites.
func TrafficSummarySQL() string {
	return `SELECT sum(SampleWeight) AS "Requests",
       round(sumIf(SampleWeight, CacheStatus = 'hit') / greatest(sum(SampleWeight), 1), 4) AS "Cache hit ratio",
       sumIf(SampleWeight, Status >= 400 AND Status < 500) AS "4xx",
       sumIf(SampleWeight, Status >= 500) AS "5xx",
       sum(Bytes * SampleWeight) AS "Bytes"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere()
}

// TrafficByClassSQL is requests over time, one series per UaClass.
func TrafficByClassSQL() string {
	return `SELECT toStartOfInterval(Timestamp, toIntervalSecond({step:UInt32})) AS t, UaClass AS "Class", sum(SampleWeight) AS "Requests"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere() + `
GROUP BY t, "Class" ORDER BY t, "Class"`
}

// TrafficTopPagesSQL is the busiest pages with the audience split.
func TrafficTopPagesSQL() string {
	return trafficRankSQL(trafficRaw, trafficPanelWhere("ContentKind = 'page'"), "Path", DefaultTrafficLimit)
}

// TrafficNotFoundSQL is 404s over time, split into scanner-shaped probes
// (TrafficScannerRegex) and real broken links.
func TrafficNotFoundSQL() string {
	return `SELECT toStartOfInterval(Timestamp, toIntervalSecond({step:UInt32})) AS t,
       if(` + trafficScanner() + `, 'scanner probes', 'real') AS "Kind", sum(SampleWeight) AS "404s"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere("Status = 404") + `
GROUP BY t, "Kind" ORDER BY t, "Kind"`
}

// TrafficTopNotFoundSQL is the most requested missing paths, scanner probes
// marked, so a real broken link is not buried under them.
func TrafficTopNotFoundSQL() string {
	return `SELECT Path AS "Path", if(` + trafficScanner() + `, 'scanner', 'real') AS "Kind",
       sum(SampleWeight) AS "404s", uniqExact(Agent) AS "Agents"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere("Status = 404") + `
GROUP BY "Path", "Kind" ORDER BY "404s" DESC, "Path" LIMIT ` + fmt.Sprint(DefaultTrafficLimit)
}

// TrafficCacheSQL is the cache-hit ratio and bytes served over time.
func TrafficCacheSQL() string {
	return `SELECT toStartOfInterval(Timestamp, toIntervalSecond({step:UInt32})) AS t,
       round(sumIf(SampleWeight, CacheStatus = 'hit') / greatest(sum(SampleWeight), 1), 4) AS "Cache hit ratio"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere() + `
GROUP BY t ORDER BY t`
}

// TrafficBytesSQL is bytes served over time, one series per audience.
func TrafficBytesSQL() string {
	return `SELECT toStartOfInterval(Timestamp, toIntervalSecond({step:UInt32})) AS t, ` + trafficAudienceExpr() + ` AS "Audience",
       sum(Bytes * SampleWeight) AS "Bytes"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere() + `
GROUP BY t, "Audience" ORDER BY t, "Audience"`
}

// BotsByAgentSQL is the by-agent table of `forge obs traffic --by agent`
// over every non-browser class.
func BotsByAgentSQL() string {
	return trafficAgentsSQL(trafficRaw, trafficPanelWhere("UaClass != 'browser'"), DefaultTrafficLimit)
}

// BotsVerifiedSQL is requests per agent name, verified (from the operator's
// published ranges) against claimed only: a claimed-only agent is the user
// agent string with nothing behind it, the shape of a spoofed probe.
func BotsVerifiedSQL() string {
	return `SELECT Agent AS "Agent", sumIf(SampleWeight, AgentVerified) AS "Verified", sumIf(SampleWeight, NOT AgentVerified) AS "Claimed only"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere("Agent != ''") + `
GROUP BY "Agent" ORDER BY "Verified" + "Claimed only" DESC, "Agent" LIMIT ` + fmt.Sprint(DefaultTrafficLimit)
}

// BotsDiscoverySQL is which agents fetched the files written for them:
// llms.txt (and llms-full.txt), the Markdown twins and robots.txt.
func BotsDiscoverySQL() string {
	return `SELECT Agent AS "Agent", any(AgentOperator) AS "Operator",
       sumIf(SampleWeight, ContentKind = 'llms') AS "llms.txt",
       sumIf(SampleWeight, ContentKind = 'markdown') AS ".md",
       sumIf(SampleWeight, ContentKind = 'robots') AS "robots.txt"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere("Agent != ''", "ContentKind IN ('llms', 'markdown', 'robots')") + `
GROUP BY "Agent" ORDER BY "llms.txt" + ".md" + "robots.txt" DESC, "Agent" LIMIT ` + fmt.Sprint(DefaultTrafficLimit)
}

// BotsTopPagesSQL is each agent's most requested pages: its top three, the
// busiest agents first.
func BotsTopPagesSQL() string {
	return `SELECT Agent AS "Agent", Path AS "Page", sum(SampleWeight) AS "Requests"
FROM ` + TrafficTable + `
WHERE ` + trafficPanelWhere("Agent != ''") + `
GROUP BY "Agent", "Page"
ORDER BY sum("Requests") OVER (PARTITION BY "Agent") DESC, "Agent", "Requests" DESC, "Page"
LIMIT 3 BY "Agent"
LIMIT ` + fmt.Sprint(DefaultTrafficLimit*3)
}
