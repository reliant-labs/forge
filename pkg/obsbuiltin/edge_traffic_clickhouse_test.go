package obsbuiltin

import (
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/obsquery"
)

// edgeTables creates the control plane's edge request tables in db (tenant
// migration 0035, the columns the traffic statements read): edge_requests,
// one row per CDN request, and its daily rollup edge_requests_1d.
func edgeTables(db string) string {
	return `
CREATE TABLE ` + db + `.edge_requests (
    Timestamp DateTime64(9), InsertId String, EnvId LowCardinality(String), Site LowCardinality(String),
    ServiceName LowCardinality(String), Host LowCardinality(String), Path String, HasQuery Bool,
    Method LowCardinality(String), Status UInt16, Protocol LowCardinality(String),
    CacheStatus LowCardinality(String), CachePop LowCardinality(String), Bytes UInt64, LatencyMs Float64,
    RefererHost LowCardinality(String), RefererPath String, UserAgent String, UaClass LowCardinality(String),
    Agent LowCardinality(String), AgentOperator LowCardinality(String), AgentVerified Bool,
    ClassifierVersion LowCardinality(String), ContentKind LowCardinality(String), ClientNet String,
    Country LowCardinality(String), SampleWeight UInt32
) ENGINE = MergeTree ORDER BY (EnvId, Site, Timestamp);
CREATE TABLE ` + db + `.edge_requests_1d (
    EnvId LowCardinality(String), Site LowCardinality(String), Day Date, Path String, Status UInt16,
    UaClass LowCardinality(String), Agent LowCardinality(String), AgentVerified Bool, CacheStatus LowCardinality(String),
    Requests UInt64, Bytes UInt64
) ENGINE = SummingMergeTree ORDER BY (EnvId, Site, Day, Path, Status, UaClass, Agent, AgentVerified, CacheStatus);
`
}

// edgeSeed is an hour of one site's requests in e1 on 2026-03-16 (and one in
// e2 the scope never reads):
//
//	3 browser page views of / (cache hits, 1000 B each)
//	ClaudeBot, verified: /features twice, /features.md, /llms.txt
//	GPTBot, claimed only: /.env (404)
//	Googlebot, verified: /robots.txt, one row of sample weight 2
//	ChatGPT-User, verified: /pricing (a cache hit) from chatgpt.com
//
// and the same day in the rollup.
func edgeSeed(db string) string {
	return edgeTables(db) + `
INSERT INTO ` + db + `.edge_requests (Timestamp, EnvId, Site, Path, Status, CacheStatus, Bytes, RefererHost, UaClass, Agent, AgentOperator, AgentVerified, ContentKind, ClientNet, SampleWeight) VALUES
('2026-03-16 00:10:00', 'e1', 'web', '/', 200, 'hit', 1000, '', 'browser', '', '', false, 'page', '192.0.2.0/24', 1),
('2026-03-16 00:11:00', 'e1', 'web', '/', 200, 'hit', 1000, 'news.ycombinator.com', 'browser', '', '', false, 'page', '192.0.2.0/24', 1),
('2026-03-16 00:12:00', 'e1', 'web', '/', 200, 'hit', 1000, '', 'browser', '', '', false, 'page', '198.51.100.0/24', 1),
('2026-03-16 00:20:00', 'e1', 'web', '/features', 200, 'miss', 500, '', 'ai_crawler', 'ClaudeBot', 'Anthropic', true, 'page', '198.51.100.0/24', 1),
('2026-03-16 00:21:00', 'e1', 'web', '/features', 200, 'miss', 500, '', 'ai_crawler', 'ClaudeBot', 'Anthropic', true, 'page', '198.51.100.0/24', 1),
('2026-03-16 00:22:00', 'e1', 'web', '/features.md', 200, 'miss', 200, '', 'ai_crawler', 'ClaudeBot', 'Anthropic', true, 'markdown', '198.51.100.0/24', 1),
('2026-03-16 00:23:00', 'e1', 'web', '/llms.txt', 200, 'miss', 100, '', 'ai_crawler', 'ClaudeBot', 'Anthropic', true, 'llms', '198.51.100.0/24', 1),
('2026-03-16 00:30:00', 'e1', 'web', '/.env', 404, 'miss', 50, '', 'ai_crawler', 'GPTBot', 'OpenAI', false, 'other', '2001:db8::/48', 1),
('2026-03-16 00:40:00', 'e1', 'web', '/robots.txt', 200, 'miss', 80, '', 'search_crawler', 'Googlebot', 'Google', true, 'robots', '198.51.100.0/24', 2),
('2026-03-16 00:50:00', 'e1', 'web', '/pricing', 200, 'hit', 400, 'chatgpt.com', 'ai_user', 'ChatGPT-User', 'OpenAI', true, 'page', '198.51.100.0/24', 1),
('2026-03-16 00:50:00', 'e2', 'web', '/', 500, 'miss', 9, '', 'browser', '', '', false, 'page', '192.0.2.0/24', 9);
INSERT INTO ` + db + `.edge_requests_1d (EnvId, Site, Day, Path, Status, UaClass, Agent, AgentVerified, CacheStatus, Requests, Bytes)
SELECT EnvId, Site, toDate(Timestamp), Path, Status, UaClass, Agent, AgentVerified, CacheStatus, sum(SampleWeight), sum(Bytes * SampleWeight)
FROM ` + db + `.edge_requests GROUP BY ALL;
`
}

func TestEdgeTrafficStatementsOnAnHour(t *testing.T) {
	const db = "forge_obsbuiltin_traffic"
	ch := newClickHouse(t, db, edgeSeed(db))
	start := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	read := func(q obsquery.TrafficQuery, st, en time.Time) []string {
		t.Helper()
		q.Range = en.Sub(st)
		sts, err := q.Statements()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range sts {
			out = append(out, ch.run(s.SQL, st, en, "e1"))
		}
		return out
	}
	got := read(obsquery.TrafficQuery{}, start, end)
	if got[0] != "web\t11\t0.3636\t1\t0\t4910" {
		t.Errorf("totals = %q", got[0])
	}
	wantPages := strings.Join([]string{
		"/\t3\t3\t0\t0\t3000",
		"/features\t2\t0\t0\t2\t1000",
		"/robots.txt\t2\t0\t0\t2\t160",
	}, "\n")
	if !strings.HasPrefix(got[1], wantPages) {
		t.Errorf("pages =\n%s\nwant prefix\n%s", got[1], wantPages)
	}
	// Thirty days around it reads the rollup and counts the same.
	long := read(obsquery.TrafficQuery{}, start.Add(-15*24*time.Hour), start.Add(15*24*time.Hour))
	if long[0] != got[0] || !strings.HasPrefix(long[1], wantPages) {
		t.Errorf("rollup read differs:\n%v\nwant\n%v", long, got)
	}
	agents := read(obsquery.TrafficQuery{By: obsquery.TrafficByAgent, Class: "bot"}, start, end)[0]
	wantAgents := strings.Join([]string{
		"ClaudeBot\tAnthropic\tai_crawler\tyes\t4\t1\t1\t/features",
		"Googlebot\tGoogle\tsearch_crawler\tyes\t2\t0\t0\t/robots.txt",
		"ChatGPT-User\tOpenAI\tai_user\tyes\t1\t0\t0\t/pricing",
		"GPTBot\tOpenAI\tai_crawler\tno\t1\t0\t0\t/.env",
	}, "\n")
	if agents != wantAgents {
		t.Errorf("agents =\n%s\nwant\n%s", agents, wantAgents)
	}
	if ref := read(obsquery.TrafficQuery{By: obsquery.TrafficByReferrer, Class: "human"}, start, end)[0]; ref != "(direct)\t2\t2\t0\t0\t2000\nnews.ycombinator.com\t1\t1\t0\t0\t1000" {
		t.Errorf("referrers = %q", ref)
	}
	if st := read(obsquery.TrafficQuery{By: obsquery.TrafficByStatus, Site: "web"}, start, end)[0]; st != "200\t10\t3\t1\t6\t4860\n404\t1\t0\t0\t1\t50" {
		t.Errorf("statuses = %q", st)
	}
}

// Every dashboard statement runs on the edge table and reads only e1.
func TestEdgeTrafficPanelsRun(t *testing.T) {
	const db = "forge_obsbuiltin_traffic_panels"
	ch := newClickHouse(t, db, edgeSeed(db))
	start := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for name, sql := range map[string]string{
		"summary": obsquery.TrafficSummarySQL(), "by-class": obsquery.TrafficByClassSQL(), "top-pages": obsquery.TrafficTopPagesSQL(),
		"404s": obsquery.TrafficNotFoundSQL(), "top-404s": obsquery.TrafficTopNotFoundSQL(), "cache": obsquery.TrafficCacheSQL(),
		"bytes": obsquery.TrafficBytesSQL(), "agents": obsquery.BotsByAgentSQL(), "verified": obsquery.BotsVerifiedSQL(),
		"discovery": obsquery.BotsDiscoverySQL(), "top-pages-per-agent": obsquery.BotsTopPagesSQL(),
	} {
		got := ch.run(sql, start, end, "e1")
		if got == "" {
			t.Errorf("%s read no rows", name)
		}
		if strings.Contains(got, "\t9\t") {
			t.Errorf("%s read e2's row:\n%s", name, got)
		}
	}
	if got := ch.run(obsquery.TrafficSummarySQL(), start, end, "e1"); got != "11\t0.3636\t1\t0\t4910" {
		t.Errorf("summary = %q", got)
	}
	if got := ch.run(obsquery.TrafficTopNotFoundSQL(), start, end, "e1"); got != "/.env\tscanner\t1\t1" {
		t.Errorf("top 404s = %q", got)
	}
	if got := ch.run(obsquery.BotsDiscoverySQL(), start, end, "e1"); got != "ClaudeBot\tAnthropic\t1\t1\t0\nGooglebot\tGoogle\t0\t0\t2" {
		t.Errorf("discovery = %q", got)
	}
	if got := ch.run(obsquery.BotsVerifiedSQL(), start, end, "e1"); !strings.Contains(got, "GPTBot\t0\t1") {
		t.Errorf("verified = %q", got)
	}
	if got := ch.run(obsquery.BotsTopPagesSQL(), start, end, "e1"); !strings.HasPrefix(got, "ClaudeBot\t/features\t2\nClaudeBot\t/features.md\t1\nClaudeBot\t/llms.txt\t1\nGooglebot\t/robots.txt\t2") {
		t.Errorf("top pages per agent =\n%s", got)
	}
}
