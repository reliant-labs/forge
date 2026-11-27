package obsquery

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func compileTraffic(t *testing.T, sql string) Stmt {
	t.Helper()
	sc := Scope{Database: "t_org", EnvRestricted: true, EnvIDs: []string{"env-1"}, Envs: []Env{{ID: "env-1", Name: "prod"}},
		Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	st, err := SQLPanel{Scope: sc, SQL: sql, StepSeconds: 3600}.Compile()
	if err != nil {
		t.Fatalf("compile: %v\n%s", err, sql)
	}
	return st
}

// Every statement is a sql panel: it passes the read-only guard, is scoped to
// the env by {env_ids} on EnvId, and counts by the sample weight.
func TestTrafficStatementsAreScopedWeightedSQLPanels(t *testing.T) {
	for _, by := range append([]TrafficBy{""}, TrafficBys...) {
		for _, rng := range []time.Duration{24 * time.Hour, 30 * 24 * time.Hour} {
			sts, err := TrafficQuery{By: by, Class: "ai", Site: "web", Range: rng}.Statements()
			if err != nil {
				t.Fatalf("by %q: %v", by, err)
			}
			if by == "" && (len(sts) != 2 || sts[0].Purpose != "totals" || sts[1].Purpose != "page") {
				t.Errorf("default read = %+v, want totals then pages", sts)
			}
			for _, st := range sts {
				c := compileTraffic(t, st.SQL)
				if c.Params["env_ids"] != "['env-1']" {
					t.Errorf("%s: env_ids = %q", st.Purpose, c.Params["env_ids"])
				}
				for _, want := range []string{"has({env_ids:Array(String)}, EnvId)", "UaClass IN ('ai_crawler', 'ai_search', 'ai_user')", "Site = 'web'", "FROM " + st.Table} {
					if !strings.Contains(st.SQL, want) {
						t.Errorf("by %q %s: missing %q\n%s", by, rng, want, st.SQL)
					}
				}
				weight := "SampleWeight"
				if st.Table == TrafficRollupTable {
					weight = "sum(Requests)"
				}
				if !strings.Contains(st.SQL, weight) {
					t.Errorf("by %q %s: does not count by %s\n%s", by, rng, weight, st.SQL)
				}
			}
		}
	}
}

func TestTrafficReadsTheRollupOnlyWhereItAnswers(t *testing.T) {
	long := 30 * 24 * time.Hour
	for by, want := range map[TrafficBy]string{
		"": TrafficRollupTable, TrafficByPage: TrafficRollupTable, TrafficByStatus: TrafficRollupTable,
		TrafficByAgent: TrafficTable, TrafficByReferrer: TrafficTable, TrafficByCountry: TrafficTable,
	} {
		sts, err := TrafficQuery{By: by, Range: long}.Statements()
		if err != nil {
			t.Fatal(err)
		}
		if sts[0].Table != want {
			t.Errorf("by %q over 30d reads %s, want %s", by, sts[0].Table, want)
		}
	}
	sts, _ := TrafficQuery{Range: 24 * time.Hour}.Statements()
	if sts[0].Table != TrafficTable || strings.Contains(sts[0].SQL, "Day >=") {
		t.Errorf("a day reads %s, want the per-request table", sts[0].Table)
	}
}

func TestTrafficRollupPreservesPartialBoundaryDays(t *testing.T) {
	sts, err := (TrafficQuery{Range: 30 * 24 * time.Hour}).Statements()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range sts {
		// Whole days must fit entirely inside [from,to). In particular the
		// to date is never included by the daily branch, even at midnight.
		for _, want := range []string{
			"toDateTime(Day, 'UTC') >= {from:DateTime64(9)}",
			"Day < toDate({to:DateTime64(9)}, 'UTC')",
			"UNION ALL",
			"SampleWeight AS Requests, raw.Bytes * SampleWeight AS Bytes",
			trafficRaw.when,
			"{from:DateTime64(9)} > toStartOfDay({from:DateTime64(9)}, 'UTC')",
			"Timestamp < toStartOfDay({from:DateTime64(9)}, 'UTC') + INTERVAL 1 DAY",
			"OR Timestamp >= toStartOfDay({to:DateTime64(9)}, 'UTC')",
		} {
			if !strings.Contains(st.SQL, want) {
				t.Errorf("%s missing boundary constraint %q:\n%s", st.Purpose, want, st.SQL)
			}
		}
		for _, hour := range []int{0, 12} {
			sc := Scope{Database: "t_org", EnvRestricted: true, EnvIDs: []string{"env-1"},
				Start: time.Date(2026, 10, 1, hour, 0, 0, 0, time.UTC),
				End:   time.Date(2026, 11, 1, hour, 0, 0, 0, time.UTC)}
			if _, err := (SQLPanel{Scope: sc, SQL: st.SQL, StepSeconds: 3600}).Compile(); err != nil {
				t.Fatalf("%s hour %d: %v", st.Purpose, hour, err)
			}
		}
	}
}

func TestTrafficAgentsKeepConflictingOperatorsSeparate(t *testing.T) {
	sts, err := (TrafficQuery{By: TrafficByAgent}).Statements()
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{sts[0].SQL, BotsByAgentSQL()} {
		if !strings.Contains(sql, "AgentOperator AS operator") ||
			!strings.Contains(sql, "GROUP BY agent, operator, class, verified") ||
			strings.Contains(sql, "any(AgentOperator)") {
			t.Errorf("operator is not a row dimension:\n%s", sql)
		}
	}
	got, err := ParseTrafficAgents(&fakeTrafficRows{rows: [][]any{
		{"ExampleBot", "First", "ai_crawler", "yes", uint64(3), uint64(0), uint64(0), "/"},
		{"ExampleBot", "Second", "ai_crawler", "yes", uint64(2), uint64(0), uint64(0), "/"},
	}})
	if err != nil || len(got) != 2 {
		t.Fatalf("agents = %+v, %v", got, err)
	}
	if got[0].Operator != "First" || got[1].Operator != "Second" || got[0].Requests != 3 || got[1].Requests != 2 {
		t.Errorf("conflicting operators lost attribution: %+v", got)
	}
}

func TestTrafficRefusesWhatItCannotQuoteSafely(t *testing.T) {
	for name, q := range map[string]TrafficQuery{
		"class":   {Class: "browser') OR 1=1 --"},
		"site":    {Site: "web' OR '1"},
		"by":      {By: "path"},
		"limit":   {Limit: MaxTrafficLimit + 1},
		"range":   {Range: MaxRangeTraffic + time.Hour},
		"neg lim": {Limit: -1},
	} {
		if _, err := q.Statements(); err == nil {
			t.Errorf("%s: %+v compiled", name, q)
		}
	}
}

func TestTrafficClassGroups(t *testing.T) {
	got, err := TrafficClasses("bot")
	if err != nil || strings.Contains(strings.Join(got, ","), "browser") || strings.Contains(strings.Join(got, ","), "unknown") {
		t.Errorf("bot = %v, %v", got, err)
	}
	if got, _ := TrafficClasses("ai_user"); len(got) != 1 || got[0] != "ai_user" {
		t.Errorf("ai_user = %v", got)
	}
	if got, _ := TrafficClasses(""); got != nil {
		t.Errorf("empty class selects %v, want every class", got)
	}
}

// The scanner pattern separates the probes seen on reliantlabs.io's CDN logs
// from the paths of a real site.
func TestTrafficScannerRegexSplitsProbesFromRealPaths(t *testing.T) {
	re := regexp.MustCompile(TrafficScannerRegex)
	for _, p := range []string{"/.env", "/@fs/.env", "/proc/self/environ", "/.env.development", "/ses.env", "/@fs/root/.aws/credentials",
		"/server.key", "/.git/config", "/wp-login.php", "/xmlrpc.php", "/.dev.vars", "/.docker/.env", "/.zshrc", "/backup.sql"} {
		if !re.MatchString(p) {
			t.Errorf("%s is a probe", p)
		}
	}
	for _, p := range []string{"/", "/features", "/pricing", "/llms.txt", "/docs/llms-full.txt", "/robots.txt", "/features.md", "/assets/index-abc.js", "/blog/environment-variables"} {
		if re.MatchString(p) {
			t.Errorf("%s is a real path", p)
		}
	}
}

func TestTrafficDashboardStatementsCompile(t *testing.T) {
	for name, sql := range map[string]string{
		"summary": TrafficSummarySQL(), "by-class": TrafficByClassSQL(), "top-pages": TrafficTopPagesSQL(),
		"404s": TrafficNotFoundSQL(), "top-404s": TrafficTopNotFoundSQL(), "cache": TrafficCacheSQL(), "bytes": TrafficBytesSQL(),
		"agents": BotsByAgentSQL(), "verified": BotsVerifiedSQL(), "discovery": BotsDiscoverySQL(), "top-pages-per-agent": BotsTopPagesSQL(),
	} {
		compileTraffic(t, sql)
		if !strings.Contains(sql, "SampleWeight") || !strings.Contains(sql, "EnvId") {
			t.Errorf("%s is not weighted and env-scoped:\n%s", name, sql)
		}
	}
}

type fakeTrafficRows struct {
	rows [][]any
	at   int
}

func (f *fakeTrafficRows) Next() bool   { f.at++; return f.at <= len(f.rows) }
func (f *fakeTrafficRows) Err() error   { return nil }
func (f *fakeTrafficRows) Close() error { return nil }
func (f *fakeTrafficRows) Scan(dest ...any) error {
	row := f.rows[f.at-1]
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = row[i].(string)
		case *uint64:
			*p = row[i].(uint64)
		case *float64:
			*p = row[i].(float64)
		}
	}
	return nil
}

func TestParseTrafficAgentsReadsVerified(t *testing.T) {
	got, err := ParseTrafficAgents(&fakeTrafficRows{rows: [][]any{
		{"ClaudeBot", "Anthropic", "ai_crawler", "yes", uint64(443), uint64(15), uint64(2), "/features"},
		{"GPTBot", "OpenAI", "ai_crawler", "no", uint64(87), uint64(0), uint64(0), "/.env"},
	}})
	if err != nil || len(got) != 2 || !got[0].Verified || got[1].Verified || got[0].LLMSTxt != 2 || got[1].TopPage != "/.env" {
		t.Errorf("agents = %+v, %v", got, err)
	}
}
