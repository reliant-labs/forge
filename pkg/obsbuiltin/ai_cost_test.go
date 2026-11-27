package obsbuiltin

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/dashspec"
	"github.com/reliant-labs/forge/pkg/llmprice"
	"github.com/reliant-labs/forge/pkg/obsquery"
)

func TestAICostGolden(t *testing.T) {
	d := AICost(Input{Project: "shop"})
	b := marshal(t, d)
	golden(t, d.ID+".json", b)
	if _, err := dashspec.ParseDashboard(d.ID+".json", b); err != nil {
		t.Errorf("%s does not validate as a spec dashboard:\n%v", d.ID, err)
	}
}

// The cost dashboard is in the AI group but is not one of AIDashboards: it
// reads llmprice.TenantTable, which an older tenant lacks, so it never goes
// into a project's spec and the platform serves it where the table exists.
func TestAICostIsServedNotSpecced(t *testing.T) {
	if IsAIDashboard(AICostID) || IsDashboard(AICostID) {
		t.Errorf("IsAIDashboard(%q) %v, IsDashboard %v: it must stay out of every project spec", AICostID, IsAIDashboard(AICostID), IsDashboard(AICostID))
	}
	for _, d := range AIDashboards(Input{Project: "shop"}) {
		if d.ID == AICostID {
			t.Errorf("AIDashboards includes %s", AICostID)
		}
	}
	d := AICost(Input{Project: "shop"})
	if d.Group != dashspec.GroupAI || dashspec.GroupOf(d) != dashspec.GroupAI {
		t.Errorf("group %q", d.Group)
	}
}

// Every cost statement passes the guard, compiles against the console's
// binding, binds only what the console binds, reads otel_traces scoped to
// the environments, and prices from the tenant price table and nothing else.
func TestEveryAICostStatementIsAGuardedScopedRead(t *testing.T) {
	bound := map[string]bool{dashspec.SQLParamFrom: true, dashspec.SQLParamTo: true, dashspec.SQLParamStep: true,
		dashspec.SQLParamEnvIDs: true, dashspec.SQLParamEnvs: true, dashspec.SQLParamServices: true}
	end := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	sc := obsquery.Scope{Database: "t_org_a", Start: end.Add(-24 * time.Hour), End: end, EnvRestricted: true, EnvIDs: []string{"e1"}}
	d := AICost(Input{Project: "shop"})
	for _, p := range d.Panels {
		for name, sql := range map[string]string{p.ID: p.SQL, p.ID + " trend": p.TrendSQL} {
			if sql == "" {
				continue
			}
			if p.Kind != dashspec.KindSQL {
				t.Errorf("%s: kind %s, want sql", name, p.Kind)
			}
			if err := dashspec.CheckSQL(sql); err != nil {
				t.Errorf("%s: refused by the SQL guard: %v", name, err)
			}
			if _, err := (obsquery.SQLPanel{Scope: sc, SQL: sql, StepSeconds: 3600}).Compile(); err != nil {
				t.Errorf("%s: does not compile: %v", name, err)
			}
			for _, m := range param.FindAllStringSubmatch(sql, -1) {
				if !bound[m[1]] {
					t.Errorf("%s: uses {%s}, which the console does not bind", name, m[1])
				}
			}
			if strings.Count(sql, "FROM otel_traces") != 1 || strings.Count(sql, aiEnvScope) < 1 {
				t.Errorf("%s: must read otel_traces once, scoped to the environments", name)
			}
			if strings.Contains(sql, "dictGet") || strings.Contains(sql, "product_events") {
				t.Errorf("%s: reads something besides otel_traces and %s", name, llmprice.TenantTable)
			}
			if !strings.Contains(sql, "joinGet('"+llmprice.TenantTable+"'") {
				t.Errorf("%s: does not price from %s", name, llmprice.TenantTable)
			}
		}
	}
}

func TestAICostLayout(t *testing.T) {
	d := AICost(Input{Project: "shop"})
	row := 0
	for _, p := range d.Panels {
		if p.Width%6 != 0 || p.Description == "" {
			t.Errorf("%s: width %d, description %q", p.ID, p.Width, p.Description)
		}
		row = (row + p.Width) % dashspec.GridColumns
	}
	if row != 0 {
		t.Errorf("the last row is not full (%d of %d)", row, dashspec.GridColumns)
	}
	if !strings.Contains(d.Description, "estimate") || !strings.Contains(d.Description, "never $0") {
		t.Errorf("the description must say what is estimated and that unpriced is never $0: %q", d.Description)
	}
}

// pricesSeedSQL creates db's price table the way the platform does (one row
// per llmprice.Price, every rate Nullable) and fills it from prices.
func pricesSeedSQL(db string, prices []llmprice.Price) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (%s String, %s Nullable(Float64), %s Nullable(Float64), %s Nullable(Float64), %s Nullable(Float64), %s String) ENGINE = Join(ANY, LEFT, %s);\n",
		db, llmprice.TenantTable, llmprice.ColModel, llmprice.ColInputPerToken, llmprice.ColOutputPerToken,
		llmprice.ColCacheReadPerToken, llmprice.ColCacheWritePerToken, llmprice.ColSource, llmprice.ColModel)
	fmt.Fprintf(&b, "INSERT INTO %s.%s VALUES\n", db, llmprice.TenantTable)
	for i, p := range prices {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "(%s, %g, %g, %g, %g, %s)", lit(p.Model), p.InputPerToken, p.OutputPerToken, p.CacheReadPerToken, p.CacheWritePerToken, lit(p.Source))
	}
	b.WriteString(";\n")
	return b.String()
}

// TestAICostOnClickHouse prices the AI fixture (ai_clickhouse_test.go) with a
// two-model price table: gpt-5 at 1e-6 in, 1e-5 out, 1e-7 cache read, and
// claude-sonnet-5 at 2e-6 in, 1e-5 out. The anthropic call r1 made was
// served by claude-sonnet-5-20261001, which the table does not price, and
// reported $0.01: reported cost wins, so it is not unpriced.
//
//	r1 gpt-5      500 in (100 cache read), 100 out  400e-6 + 100e-7 + 100e-5 = 0.00141
//	r2 sonnet-5   300 in                            300 * 2e-6            = 0.0006
//	r3 gpt-5 x2   100 in, 50 out each               2 * (1e-4 + 5e-4)     = 0.0012
//	r4 gpt-5      50 in (cancelled, still billed)   5e-5                  = 0.00005
//	t5 gpt-5      10 in, 5 out                      1e-5 + 5e-5           = 0.00006
//
// Estimated 0.00332, reported 0.01, cost 0.01332.
func TestAICostOnClickHouse(t *testing.T) {
	tb, err := llmprice.Parse([]byte(`{"source":"litellm","version":"vtest","models":{
		"gpt-5":{"provider":"openai","mode":"chat","input":1e-6,"output":1e-5,"cache_read":1e-7},
		"claude-sonnet-5":{"provider":"anthropic","mode":"chat","input":2e-6,"output":1e-5}}}`))
	if err != nil {
		t.Fatal(err)
	}
	db := "forge_obsbuiltin_ai_cost"
	ch := newClickHouse(t, db, aiSeedSQL(db, aiFixture())+pricesSeedSQL(db, tb.Prices()))
	end := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := end.Add(-7 * 24 * time.Hour)
	rows := func(r ...string) string { return strings.Join(r, "\n") }
	d := AICost(Input{Project: "shop"})
	for _, p := range d.Panels {
		for _, sql := range []string{p.SQL, p.TrendSQL} {
			if sql == "" {
				continue
			}
			if _, err := ch.try(sql, start, end, "e1"); err != nil {
				t.Errorf("%s: %v", p.ID, err)
			}
		}
	}
	check := func(id string, trend bool, want string) {
		t.Helper()
		p := panel(t, d, id)
		sql := p.SQL
		if trend {
			sql = p.TrendSQL
		}
		if got := ch.run(sql, start, end, "e1"); got != want {
			t.Errorf("%s (trend %v) =\n%s\nwant\n%s", id, trend, got, want)
		}
	}
	check("cost", false, "0.0133")
	// 03-14's 0.00125 is 0.0012499... in binary floating point.
	check("cost", true, rows("2026-03-13\t0.0001", "2026-03-14\t0.0012", "2026-03-15\t0.012"))
	check("estimated", false, "0.0033")
	check("reported", false, "0.01")
	check("unpriced", false, "0")
	// A model with nothing estimated shows no estimate (NULL), not 0.
	check("by-model", false, rows(
		"anthropic\tclaude-sonnet-5-20261001\t1\t1000\t200\t0.01\t\\N\t0.01\t0\t\t\\N\t\\N\t",
		"openai\tgpt-5\t5\t760\t205\t\\N\t0.0027\t0.0027\t0\tgpt-5\t1\t10\tlitellm@vtest",
		"anthropic\tclaude-sonnet-5\t1\t300\t0\t\\N\t0.0006\t0.0006\t0\tclaude-sonnet-5\t2\t10\tlitellm@vtest"))
	check("by-workflow", false, rows(
		"triage\t2\t3\t0.012\t0.006\t0",
		"digest\t2\t3\t0.0012\t0.0006\t0",
		"(none)\t0\t1\t0.0001\t\\N\t0"))
}
