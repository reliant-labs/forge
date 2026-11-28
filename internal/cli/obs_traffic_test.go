package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/obsbackend"
)

// trafficCaller answers QueryPanel's sql panels by a substring of their SQL.
type trafficCaller struct {
	answers map[string]string
	sqls    []string
}

func (f *trafficCaller) Call(_ context.Context, _ string, req, out any) error {
	b, _ := json.Marshal(req)
	var m struct {
		Panel struct {
			SQL string `json:"sql"`
		} `json:"panel"`
	}
	_ = json.Unmarshal(b, &m)
	f.sqls = append(f.sqls, m.Panel.SQL)
	for k, v := range f.answers {
		if strings.Contains(m.Panel.SQL, k) {
			return json.Unmarshal([]byte(v), out)
		}
	}
	return json.Unmarshal([]byte(`{}`), out)
}

const (
	cliTotalsReply = `{"sqlColumns":[{"name":"site","type":"String"},{"name":"requests","numeric":true},{"name":"cache_hit_ratio","numeric":true},{"name":"status_4xx","numeric":true},{"name":"status_5xx","numeric":true},{"name":"bytes","numeric":true}],
"rows":[{"group":{"site":"web"},"values":[41220,0.38,1410,0,2040109465]}]}`
	cliPagesReply = `{"sqlColumns":[{"name":"key","type":"String"},{"name":"requests","numeric":true},{"name":"humans","numeric":true},{"name":"agents","numeric":true},{"name":"crawlers","numeric":true},{"name":"bytes","numeric":true}],
"rows":[{"group":{"key":"/"},"values":[6112,4980,402,730,1000]},{"group":{"key":"/llms.txt"},"values":[97,0,61,36,10]}]}`
	cliAgentsReply = `{"sqlColumns":[{"name":"agent","type":"String"},{"name":"operator","type":"String"},{"name":"class","type":"String"},{"name":"verified","type":"String"},{"name":"requests","numeric":true},{"name":"markdown","numeric":true},{"name":"llms_txt","numeric":true},{"name":"top_page","type":"String"}],
"rows":[{"group":{"agent":"ClaudeBot","operator":"Anthropic","class":"ai_crawler","verified":"yes","top_page":"/features"},"values":[443,15,2]},
{"group":{"agent":"GPTBot","operator":"OpenAI","class":"ai_crawler","verified":"no","top_page":"/.env"},"values":[87,0,0]}]}`
)

func runTrafficCloud(t *testing.T, args ...string) (string, *trafficCaller, error) {
	t.Helper()
	tc := &trafficCaller{answers: map[string]string{"cache_hit_ratio": cliTotalsReply, "AS humans": cliPagesReply, "AS agent,": cliAgentsReply}}
	old := obsDepsOverride
	t.Cleanup(func() { obsDepsOverride = old })
	obsDepsOverride = &obsDeps{
		Facts: func(context.Context, io.Writer, string) (obsbackend.Facts, error) { return issuesCloudFacts(), nil },
		Cloud: func(_ context.Context, env string) (obsbackend.Cloud, error) {
			return obsbackend.Cloud{Env: env, EnvID: "env-prod", OrgID: "org-1", Caller: tc}, nil
		},
		Now: func() time.Time { return obsNow },
	}
	var out, errb bytes.Buffer
	cmd := newTestRootCmd(t)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{"obs"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), tc, err
}

func TestObsTrafficDefaultReadsTotalsAndTopPages(t *testing.T) {
	out, _, err := runTrafficCloud(t, "traffic", "prod", "--since", "7d")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SITE", "VISITORS*", "CACHE HIT", "web", "41,220", "38%", "1,410", "1.9 GiB", "TOP PAGES", "HUMANS", "AGENTS", "CRAWLERS", "/llms.txt", "6,112"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestObsTrafficJSONDocument(t *testing.T) {
	out, tc, err := runTrafficCloud(t, "traffic", "prod", "--json", "--site", "web")
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, out)
	for k, want := range map[string]any{"schema": "forge.obs.traffic/v1", "ok": true, "query_mode": "structured", "by": "", "site": "web", "table": "edge_requests"} {
		if doc[k] != want {
			t.Errorf("%s = %v, want %v", k, doc[k], want)
		}
	}
	totals := doc["totals"].([]any)[0].(map[string]any)
	for k, want := range map[string]any{"site": "web", "requests": float64(41220), "cache_hit_ratio": 0.38, "status_4xx": float64(1410), "status_5xx": float64(0), "bytes": float64(2040109465)} {
		if totals[k] != want {
			t.Errorf("totals.%s = %v, want %v", k, totals[k], want)
		}
	}
	row := doc["rows"].([]any)[1].(map[string]any)
	for k, want := range map[string]any{"key": "/llms.txt", "requests": float64(97), "humans": float64(0), "agents": float64(61), "crawlers": float64(36), "bytes": float64(10)} {
		if row[k] != want {
			t.Errorf("rows.%s = %v, want %v", k, row[k], want)
		}
	}
	if _, ok := doc["agents"]; ok {
		t.Error("the default read carries no agents section")
	}
	for _, s := range tc.sqls {
		if !strings.Contains(s, "Site = 'web'") {
			t.Errorf("--site did not narrow:\n%s", s)
		}
	}
}

func TestObsTrafficByAgent(t *testing.T) {
	out, tc, err := runTrafficCloud(t, "traffic", "prod", "--by", "agent", "--class", "ai")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AGENT", "OPERATOR", "VERIFIED", ".md", "llms.txt", "TOP PAGE", "ClaudeBot", "Anthropic", "/features", "GPTBot"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SITE") {
		t.Errorf("--by agent prints no totals:\n%s", out)
	}
	if len(tc.sqls) != 1 || !strings.Contains(tc.sqls[0], "UaClass IN ('ai_crawler', 'ai_search', 'ai_user')") {
		t.Errorf("statements = %v", tc.sqls)
	}
	jout, _, err := runTrafficCloud(t, "traffic", "prod", "--by", "agent", "--json")
	if err != nil {
		t.Fatal(err)
	}
	a := decode(t, jout)["agents"].([]any)[1].(map[string]any)
	if a["agent"] != "GPTBot" || a["verified"] != false || a["top_page"] != "/.env" || a["llms_txt"] != float64(0) {
		t.Errorf("agents[1] = %v", a)
	}
}

func TestObsTrafficRefusesBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--by", "path"},
		{"--class", "robots"},
		{"--filter", "a=b"},
		{"--service", "web"},
		{"--limit", "0"},
	} {
		_, _, err := runTrafficCloud(t, append([]string{"traffic", "prod"}, args...)...)
		if exitCodeForError(err) != 1 {
			t.Errorf("%v: exit %d (%v), want 1", args, exitCodeForError(err), err)
		}
	}
}

func TestObsTrafficLocalSaysHostedOnly(t *testing.T) {
	out, _, err := obsRun(t, devFacts(), &scriptedRunner{}, "traffic", "dev", "--json")
	if exitCodeForError(err) != 2 {
		t.Fatalf("exit = %d, want 2: %v", exitCodeForError(err), err)
	}
	doc := decode(t, out)
	if msg, _ := doc["error"].(string); !strings.Contains(msg, "only on Reliant hosting") {
		t.Errorf("error = %q", msg)
	}
}

// The dashboards' edge groups follow the frontends some env binds to hosting.
func TestHostedStaticFrontendsAreTheOnHostedOnes(t *testing.T) {
	got := hostedStaticFrontends([]renderedEnvFrontend{
		{Env: "dev", Frontend: FrontendEntity{Name: "web", Runtime: FrontendRuntime{Type: FrontendRuntimeHost}}},
		{Env: "prod", Frontend: FrontendEntity{Name: "web", Runtime: FrontendRuntime{Type: FrontendRuntimeHosted}}},
		{Env: "staging", Frontend: FrontendEntity{Name: "web", Runtime: FrontendRuntime{Type: FrontendRuntimeHosted}}},
		{Env: "prod", Frontend: FrontendEntity{Name: "docs", Runtime: FrontendRuntime{Type: FrontendRuntimeBucket}}},
	})
	if len(got) != 1 || got[0] != "web" {
		t.Errorf("hosted = %v, want [web]", got)
	}
}
