package obsbackend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/pkg/obsquery"
)

// sqlPanelCaller answers QueryPanel's sql panels by a substring of their SQL,
// in the control plane's sqlColumns shape, and records each request.
type sqlPanelCaller struct {
	answers map[string]string
	err     error
	reqs    []map[string]any
}

func (f *sqlPanelCaller) Call(_ context.Context, procedure string, req, out any) error {
	if procedure != procQueryPanel {
		return errors.New("unexpected " + procedure)
	}
	b, _ := json.Marshal(req)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.reqs = append(f.reqs, m)
	if f.err != nil {
		return f.err
	}
	sql, _ := m["panel"].(map[string]any)["sql"].(string)
	for k, v := range f.answers {
		if strings.Contains(sql, k) {
			return json.Unmarshal([]byte(v), out)
		}
	}
	return json.Unmarshal([]byte(`{}`), out)
}

func trafficNarrow() Narrow {
	end := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	return Narrow{Range: Range{From: end.Add(-24 * time.Hour), To: end}}
}

const (
	totalsReply = `{"sqlColumns":[{"name":"site","type":"String"},{"name":"requests","type":"UInt64","numeric":true},{"name":"cache_hit_ratio","type":"Float64","numeric":true},{"name":"status_4xx","type":"UInt64","numeric":true},{"name":"status_5xx","type":"UInt64","numeric":true},{"name":"bytes","type":"UInt64","numeric":true}],
"rows":[{"group":{"site":"web"},"values":[41220,0.38,1410,0,2040109465]}]}`
	pagesReply = `{"sqlColumns":[{"name":"key","type":"String"},{"name":"requests","type":"UInt64","numeric":true},{"name":"humans","type":"UInt64","numeric":true},{"name":"agents","type":"UInt64","numeric":true},{"name":"crawlers","type":"UInt64","numeric":true},{"name":"bytes","type":"UInt64","numeric":true}],
"rows":[{"group":{"key":"/"},"values":[6112,4980,402,730,1000]},{"group":{"key":"/llms.txt"},"values":[97,0,61,36,10]}]}`
	agentsReply = `{"sqlColumns":[{"name":"agent","type":"String"},{"name":"operator","type":"String"},{"name":"class","type":"String"},{"name":"verified","type":"String"},{"name":"requests","type":"UInt64","numeric":true},{"name":"markdown","type":"UInt64","numeric":true},{"name":"llms_txt","type":"UInt64","numeric":true},{"name":"top_page","type":"String"}],
"rows":[{"group":{"agent":"ClaudeBot","operator":"Anthropic","class":"ai_crawler","verified":"yes","top_page":"/features"},"values":[443,15,2]},
{"group":{"agent":"GPTBot","operator":"OpenAI","class":"ai_crawler","verified":"no","top_page":"/.env"},"values":[87,0,0]}]}`
)

func TestCloudTrafficRunsTheStatementsThroughQueryPanel(t *testing.T) {
	f := &sqlPanelCaller{answers: map[string]string{"cache_hit_ratio": totalsReply, "AS humans": pagesReply}}
	c := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: f}
	res, err := c.Traffic(context.Background(), TrafficRequest{Narrow: trafficNarrow(), Site: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("ran %d statements, want totals and pages", len(f.reqs))
	}
	for _, r := range f.reqs {
		scope := r["scope"].(map[string]any)
		if ids, _ := scope["envIds"].([]any); len(ids) != 1 || ids[0] != "env-1" || scope["orgId"] != "org-1" {
			t.Errorf("not bound to the env: %v", scope)
		}
		if p := r["panel"].(map[string]any); p["kind"] != "sql" || !strings.Contains(p["sql"].(string), "FROM edge_requests") {
			t.Errorf("panel = %v", p)
		}
	}
	if res.Table != obsquery.TrafficTable || len(res.Totals) != 1 || res.Totals[0].Requests != 41220 || res.Totals[0].CacheHitRatio != 0.38 || res.Totals[0].Site != "web" {
		t.Errorf("totals = %+v (%s)", res.Totals, res.Table)
	}
	if len(res.Rows) != 2 || res.Rows[1].Key != "/llms.txt" || res.Rows[1].Agents != 61 || res.Rows[1].Crawlers != 36 {
		t.Errorf("pages = %+v", res.Rows)
	}
}

func TestCloudTrafficByAgent(t *testing.T) {
	f := &sqlPanelCaller{answers: map[string]string{"AS agent": agentsReply}}
	c := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: f}
	res, err := c.Traffic(context.Background(), TrafficRequest{Narrow: trafficNarrow(), By: obsquery.TrafficByAgent, Class: "ai"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Agents) != 2 || !res.Agents[0].Verified || res.Agents[1].Verified || res.Agents[0].Markdown != 15 || res.Agents[1].TopPage != "/.env" {
		t.Errorf("agents = %+v", res.Agents)
	}
}

func TestCloudTrafficNamesAMissingEdgeTable(t *testing.T) {
	f := &sqlPanelCaller{err: &cloud.Error{Code: "invalid_argument", Message: "code: 60, message: Unknown table expression identifier 'edge_requests' (UNKNOWN_TABLE)"}}
	c := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: f}
	_, err := c.Traffic(context.Background(), TrafficRequest{Narrow: trafficNarrow()})
	var be *Error
	if !errors.As(err, &be) || be.Code != ExitUndetermined || !strings.Contains(be.Msg, "no edge request logs") {
		t.Errorf("err = %v, want could-not-look naming the missing edge logs", err)
	}
}

func TestCloudTrafficExplainRunsNothing(t *testing.T) {
	f := &sqlPanelCaller{answers: map[string]string{"edge_requests": `{"compiledSql":[{"sql":"SELECT 1","params":{"env_ids":"['env-1']"}}]}`}}
	c := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: f}
	n := trafficNarrow()
	n.Explain = true
	res, err := c.Traffic(context.Background(), TrafficRequest{Narrow: n, By: obsquery.TrafficByStatus})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Statements) != 1 || res.Statements[0].Purpose != "traffic status" || res.Rows != nil {
		t.Errorf("explain = %+v", res)
	}
	if f.reqs[0]["explain"] != true {
		t.Errorf("the request did not ask for explain: %v", f.reqs[0])
	}
}

func TestLocalTrafficSaysEdgeLogsAreHostedOnly(t *testing.T) {
	_, err := Local{Env: "dev"}.Traffic(context.Background(), TrafficRequest{Narrow: trafficNarrow()})
	var be *Error
	if !errors.As(err, &be) || be.Code != ExitUndetermined || !strings.Contains(be.Msg, "only on Reliant hosting") {
		t.Errorf("err = %v", err)
	}
}

var (
	_ TrafficReader = Local{}
	_ TrafficReader = Cloud{}
)
