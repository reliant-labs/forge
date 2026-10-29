package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/exitcode"
	"github.com/reliant-labs/forge/internal/obsbackend"
)

const (
	cpListDashboards = "controlplane.v1.DashboardService/ListDashboards"
	cpGetDashboard   = "controlplane.v1.DashboardService/GetDashboard"
)

// auditCP is a fake control plane for the audit: it answers by procedure, and
// QueryPanel by what the request asked (the window's width, whether it was
// bounded to one env, the panel id), which is how a probe differs from the run.
type auditCP struct {
	mu         sync.Mutex
	dashboards string
	specs      map[string]string
	// panel answers QueryPanel for (panel id, shape of the request).
	panel func(q auditQuery) (string, error)
	// metrics is the ListMetrics reply.
	metrics string
	errs    map[string]error
	calls   []auditQuery
}

type auditQuery struct {
	Proc    string
	PanelID string
	Wide    bool // the window is wider than the audit's 1h
	AllEnvs bool // not bounded to one env
	Bare    bool // the panel carries no predicates
	Step    int64
	Panel   map[string]any
}

func (c *auditCP) Call(_ context.Context, procedure string, req, out any) error {
	b, _ := json.Marshal(req)
	var in map[string]any
	_ = json.Unmarshal(b, &in)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.errs[procedure]; err != nil {
		c.calls = append(c.calls, auditQuery{Proc: procedure})
		return err
	}
	reply := ""
	switch procedure {
	case cpListDashboards:
		reply = c.dashboards
	case cpGetDashboard:
		key := in["ref"].(map[string]any)["key"].(string)
		reply = c.specs[key]
		if reply == "" {
			return &cloud.Error{Code: cloud.CodeNotFound, HTTPStatus: 404}
		}
	case cpListMetrics:
		reply = c.metrics
	case cpQueryPanel:
		q := auditQuery{Proc: procedure}
		panel, _ := in["panel"].(map[string]any)
		q.Panel = panel
		q.PanelID, _ = panel["id"].(string)
		scope, _ := in["scope"].(map[string]any)
		q.AllEnvs = scope["envIds"] == nil
		tr, _ := scope["timeRange"].(map[string]any)
		start, _ := time.Parse(time.RFC3339, tr["start"].(string))
		end, _ := time.Parse(time.RFC3339, tr["end"].(string))
		q.Wide = end.Sub(start) > 2*time.Hour
		q.Bare = !hasPredicates(panel)
		if s, ok := in["stepSeconds"].(float64); ok {
			q.Step = int64(s)
		}
		c.calls = append(c.calls, q)
		r, err := c.panel(q)
		if err != nil {
			return err
		}
		reply = r
	default:
		return fmt.Errorf("unexpected %s", procedure)
	}
	if procedure != cpQueryPanel {
		c.calls = append(c.calls, auditQuery{Proc: procedure})
	}
	return json.Unmarshal([]byte(reply), out)
}

func hasPredicates(panel map[string]any) bool {
	if w, ok := panel["where"].([]any); ok && len(w) > 0 {
		return true
	}
	if ss, ok := panel["series"].([]any); ok {
		for _, s := range ss {
			if w, ok := s.(map[string]any)["where"].([]any); ok && len(w) > 0 {
				return true
			}
		}
	}
	return false
}

func (c *auditCP) count(match func(auditQuery) bool) int {
	n := 0
	for _, q := range c.calls {
		if match(q) {
			n++
		}
	}
	return n
}

func auditSpec(panels ...string) string {
	return fmt.Sprintf(`{"summary":{"ref":{"source":"DASHBOARD_SOURCE_GENERATED","key":"d1"},"title":"Usage"},"spec":{"id":"d1","title":"Usage","panels":[%s]}}`, strings.Join(panels, ","))
}

const auditIndex = `{"dashboards":[{"ref":{"source":"DASHBOARD_SOURCE_GENERATED","projectId":"proj","key":"d1"},"title":"Usage","source":"DASHBOARD_SOURCE_GENERATED","panelCount":1,"release":{"releaseVersion":"v7"}}]}`

func linePanel(id string) string {
	return fmt.Sprintf(`{"id":%q,"title":%q,"kind":"line","signal":"traces","series":[{"label":"n","agg":"count"}]}`, id, id)
}

func metricPanel(id, metric string) string {
	return fmt.Sprintf(`{"id":%q,"title":%q,"kind":"line","signal":"metrics","series":[{"label":"n","agg":"rate","field":%q,"metricType":"sum"}]}`, id, id, metric)
}

func filteredPanel(id string) string {
	return fmt.Sprintf(`{"id":%q,"title":%q,"kind":"line","signal":"traces","series":[{"label":"n","agg":"count"}],"where":[{"field":"span.name","op":"eq","value":"x"}]}`, id, id)
}

const (
	respEmpty  = `{"kind":"line","stepSeconds":30}`
	respData   = `{"kind":"line","stepSeconds":30,"series":[{"label":"n","points":[{"time":"2026-10-22T11:30:00Z","value":3},{"time":"2026-10-22T11:31:00Z","value":4}]}]}`
	respAllZro = `{"kind":"line","stepSeconds":30,"series":[{"label":"n","points":[{"time":"2026-10-22T11:30:00Z"},{"time":"2026-10-22T11:31:00Z"}]}]}`
	respOne    = `{"kind":"line","stepSeconds":3600,"series":[{"label":"n","points":[{"time":"2026-10-22T00:00:00Z","value":53}]}]}`
)

func auditRun(t *testing.T, cp *auditCP, args ...string) (string, string, error) {
	t.Helper()
	old := auditSleep
	auditSleep = func(context.Context, time.Duration) {}
	t.Cleanup(func() { auditSleep = old })
	oldProject := hostedProjectName
	hostedProjectName = func() string { return "proj" }
	t.Cleanup(func() { hostedProjectName = oldProject })
	return mxRun(t, mxCloud(cp), append([]string{"dashboards", "audit", "prod", "--json"}, args...)...)
}

type auditDocT struct {
	ExitCode int `json:"exit_code"`
	Summary  struct {
		Panels, OK, Partial, Empty, Error int
		Dashboards                        int
	} `json:"summary"`
	Dashboards []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct{ Class string }
		Panels []struct {
			ID            string `json:"id"`
			Status        string `json:"status"`
			Reason        string `json:"reason"`
			ProbableCause string `json:"probable_cause"`
			Note          string
			StepSeconds   int64 `json:"step_seconds"`
			Attempts      int
			Warnings      []string
			Error         *struct {
				Class        string
				HTTPStatus   int    `json:"http_status"`
				MissingScope string `json:"missing_scope"`
			}
			Probe *struct {
				Verdict string
				Steps   []struct {
					Name    string
					HasData bool `json:"has_data"`
				}
				Catalog []struct {
					Metric string
					Seen   bool
				}
			}
		}
	}
}

func decodeAudit(t *testing.T, out string) auditDocT {
	t.Helper()
	var d auditDocT
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	return d
}

func TestAuditPanelClasses(t *testing.T) {
	absentResp := `{"kind":"line","stepSeconds":30,"warnings":["metric_absent: rpc.server.calls"]}`
	someAbsent := `{"kind":"line","stepSeconds":30,"warnings":["metric_absent: rpc.server.errors"],"series":[{"label":"n","points":[{"time":"2026-10-22T11:30:00Z","value":3},{"time":"2026-10-22T11:31:00Z","value":4}]}]}`
	mismatch := `{"kind":"line","stepSeconds":30,"warnings":["metric_kind_mismatch: rpc.server.calls declared sum, stored gauge"],"series":[{"label":"n","points":[{"time":"2026-10-22T11:30:00Z","value":3},{"time":"2026-10-22T11:31:00Z","value":4}]}]}`
	catalogHas := `{"metrics":[{"name":"rpc.server.calls","type":"METRIC_TYPE_SUM","seen":true,"queryable":true,"services":["api"],"lastSeen":"2026-10-20T00:00:00Z"}]}`
	catalogNone := `{"metrics":[]}`

	cases := []struct {
		name       string
		panel      string
		answer     func(q auditQuery) (string, error)
		metrics    string
		wantStatus string
		wantReason string
		wantExit   int
		wantCause  string
	}{
		{
			name: "ok", panel: linePanel("p"), wantStatus: "ok", wantExit: 0,
			answer: func(auditQuery) (string, error) { return respData, nil },
		},
		{
			name: "ok but all zero is noted, not failed", panel: linePanel("p"), wantStatus: "ok", wantExit: 0,
			answer: func(auditQuery) (string, error) { return respAllZro, nil },
		},
		{
			name: "empty with no data anywhere", panel: linePanel("p"), wantStatus: "empty", wantReason: verdictNoneAnywhere, wantExit: 2,
			wantCause: "no data in any env",
			answer:    func(auditQuery) (string, error) { return respEmpty, nil },
		},
		{
			name: "data exists outside the window", panel: linePanel("p"), wantStatus: "empty", wantReason: verdictOutsideWindow, wantExit: 2,
			wantCause: "the data exists",
			answer: func(q auditQuery) (string, error) {
				if q.Wide && !q.AllEnvs {
					return respData, nil
				}
				return respEmpty, nil
			},
		},
		{
			name: "data exists in another env", panel: linePanel("p"), wantStatus: "empty", wantReason: verdictOtherEnv, wantExit: 2,
			wantCause: "other environments",
			answer: func(q auditQuery) (string, error) {
				if q.AllEnvs {
					return respData, nil
				}
				return respEmpty, nil
			},
		},
		{
			name: "data excluded by the panel's filters", panel: filteredPanel("p"), wantStatus: "empty", wantReason: verdictExcludedFilter, wantExit: 2,
			wantCause: "without the panel's filters",
			answer: func(q auditQuery) (string, error) {
				if q.Wide && q.Bare && !q.AllEnvs {
					return respData, nil
				}
				return respEmpty, nil
			},
		},
		{
			name: "absent metric, never seen", panel: metricPanel("p", "rpc.server.calls"), wantStatus: "empty", wantReason: verdictMetricNever, wantExit: 2,
			wantCause: "no such metric", metrics: catalogNone,
			answer: func(auditQuery) (string, error) { return absentResp, nil },
		},
		{
			name: "absent metric but seen elsewhere reads as no data anywhere", panel: metricPanel("p", "rpc.server.calls"), wantStatus: "empty", wantReason: verdictNoneAnywhere, wantExit: 2,
			metrics: catalogHas,
			answer:  func(auditQuery) (string, error) { return absentResp, nil },
		},
		{
			name: "one series absent", panel: metricPanel("p", "rpc.server.calls"), wantStatus: "partial", wantReason: "metric_absent_some_series", wantExit: 2,
			wantCause: "rpc.server.errors", answer: func(auditQuery) (string, error) { return someAbsent, nil },
		},
		{
			name: "metric stored as another kind", panel: metricPanel("p", "rpc.server.calls"), wantStatus: "partial", wantReason: "metric_kind_mismatch", wantExit: 2,
			wantCause: "different kind", answer: func(auditQuery) (string, error) { return mismatch, nil },
		},
		{
			name: "one bucket cannot draw a line", panel: linePanel("p"), wantStatus: "partial", wantReason: "single_bucket", wantExit: 2,
			wantCause: "needs two", answer: func(auditQuery) (string, error) { return respOne, nil },
		},
		{
			name:  "403 is auth, never no data",
			panel: linePanel("p"), wantStatus: "error", wantReason: "auth", wantExit: 1, wantCause: "an auth/scope problem, not evidence",
			answer: func(auditQuery) (string, error) {
				return "", &cloud.Error{HTTPStatus: http.StatusForbidden, Code: cloud.CodePermissionDenied, Message: "obs:read scope is required"}
			},
		},
		{
			name:  "quota",
			panel: linePanel("p"), wantStatus: "error", wantReason: "quota", wantExit: 1, wantCause: "allowance is spent",
			answer: func(auditQuery) (string, error) {
				return "", &cloud.Error{HTTPStatus: 429, Code: cloud.CodeResourceExhausted, Reason: "obs_query_quota"}
			},
		},
		{
			name:  "in flight is retried, then reported",
			panel: linePanel("p"), wantStatus: "error", wantReason: "in_flight", wantExit: 1, wantCause: "in flight",
			answer: func(auditQuery) (string, error) {
				return "", &cloud.Error{HTTPStatus: 429, Code: cloud.CodeResourceExhausted, Reason: "in_flight_limit"}
			},
		},
		{
			name:  "storage down",
			panel: linePanel("p"), wantStatus: "error", wantReason: "storage", wantExit: 1, wantCause: "storage",
			answer: func(auditQuery) (string, error) {
				return "", &cloud.Error{HTTPStatus: 503, Code: cloud.CodeUnavailable}
			},
		},
		{
			name:  "timeout",
			panel: linePanel("p"), wantStatus: "error", wantReason: "timeout", wantExit: 1, wantCause: "timed out",
			answer: func(auditQuery) (string, error) {
				return "", &cloud.Error{HTTPStatus: 504, Code: cloud.CodeDeadlineExceeded}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(tc.panel)}, panel: tc.answer, metrics: tc.metrics}
			out, _, err := auditRun(t, cp)
			doc := decodeAudit(t, out)
			if got := exitcode.Of(err); got != tc.wantExit || doc.ExitCode != tc.wantExit {
				t.Fatalf("process exit %d, document exit_code %d, want %d (%v)\n%s", got, doc.ExitCode, tc.wantExit, err, out)
			}
			if len(doc.Dashboards) != 1 || len(doc.Dashboards[0].Panels) != 1 {
				t.Fatalf("shape: %s", out)
			}
			p := doc.Dashboards[0].Panels[0]
			if p.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q\n%s", p.Status, tc.wantStatus, out)
			}
			if tc.wantReason != "" && p.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", p.Reason, tc.wantReason)
			}
			if tc.wantCause != "" && !strings.Contains(p.ProbableCause, tc.wantCause) {
				t.Errorf("probable_cause = %q, want it to contain %q", p.ProbableCause, tc.wantCause)
			}
			if tc.wantStatus == "ok" && p.StepSeconds == 0 {
				t.Errorf("the step the control plane chose is echoed: %+v", p)
			}
		})
	}
}

// A 403 on a panel is an error carrying the status and the missing scope; it is
// never an empty panel, and the probe never runs on it.
func TestAuditAuthRefusalNamesTheScopeAndRunsNoProbe(t *testing.T) {
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(linePanel("p"))},
		panel: func(auditQuery) (string, error) {
			return "", &cloud.Error{HTTPStatus: http.StatusForbidden, Code: cloud.CodePermissionDenied, Message: "this credential does not carry the obs:read scope"}
		}}
	out, _, _ := auditRun(t, cp)
	p := decodeAudit(t, out).Dashboards[0].Panels[0]
	if p.Error == nil || p.Error.Class != "auth" || p.Error.HTTPStatus != 403 || p.Error.MissingScope != "obs:read" {
		t.Errorf("error = %+v", p.Error)
	}
	if p.Probe != nil {
		t.Errorf("a refused read is probed: %+v", p.Probe)
	}
	if n := cp.count(func(q auditQuery) bool { return q.Wide }); n != 0 {
		t.Errorf("%d wide reads after a refusal", n)
	}
}

// An unreadable index is an auth problem when the credential was refused, and
// reports exit 2 (could not look), not an empty list.
func TestAuditIndexRefusal(t *testing.T) {
	cp := &auditCP{errs: map[string]error{cpListDashboards: &cloud.Error{HTTPStatus: 403, Code: cloud.CodePermissionDenied, Message: "forbidden"}}}
	out, _, err := auditRun(t, cp)
	if exitcode.Of(err) != 2 {
		t.Fatalf("exit = %d, want 2 (%v)", exitcode.Of(err), err)
	}
	if !strings.Contains(out, "auth/scope problem") {
		t.Errorf("%s", out)
	}
}

// Transient refusals are retried after the wait the control plane asked for,
// and the panel then reads.
func TestAuditRetriesTransientRefusals(t *testing.T) {
	var n int
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(linePanel("p"))},
		panel: func(auditQuery) (string, error) {
			n++
			if n < 3 {
				return "", &cloud.Error{HTTPStatus: 429, Code: cloud.CodeResourceExhausted, Message: "too many observability queries in flight; retry shortly"}
			}
			return respData, nil
		}}
	out, _, err := auditRun(t, cp)
	if err != nil {
		t.Fatal(err)
	}
	p := decodeAudit(t, out).Dashboards[0].Panels[0]
	if p.Status != "ok" || p.Attempts != 3 {
		t.Errorf("status %q attempts %d, want ok after 3", p.Status, p.Attempts)
	}
}

// Once the org's allowance is spent, the remaining reads are not sent: they are
// reported as quota errors, not as empty panels.
func TestAuditStopsReadingWhenTheQuotaIsSpent(t *testing.T) {
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(linePanel("a"), linePanel("b"), linePanel("c"), linePanel("d"), linePanel("e"), linePanel("f"))},
		panel: func(auditQuery) (string, error) {
			return "", &cloud.Error{HTTPStatus: 429, Code: cloud.CodeResourceExhausted, Reason: "obs_query_quota"}
		}}
	out, _, err := auditRun(t, cp, "--no-probe")
	if exitcode.Of(err) != 1 {
		t.Fatalf("exit = %d", exitcode.Of(err))
	}
	doc := decodeAudit(t, out)
	if doc.Summary.Error != 6 || doc.Summary.Empty != 0 {
		t.Errorf("summary %+v", doc.Summary)
	}
	if sent := cp.count(func(q auditQuery) bool { return q.Proc == cpQueryPanel }); sent >= 6 {
		t.Errorf("%d QueryPanel calls after the allowance ran out", sent)
	}
}

// The audit sends the panel as the console does: stepSeconds 0 and no filters
// the dashboard did not declare, over the console's default hour.
func TestAuditReplaysTheConsoleRequest(t *testing.T) {
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(filteredPanel("p"))},
		panel: func(auditQuery) (string, error) { return respData, nil }}
	if _, _, err := auditRun(t, cp); err != nil {
		t.Fatal(err)
	}
	var q auditQuery
	for _, c := range cp.calls {
		if c.Proc == cpQueryPanel {
			q = c
		}
	}
	if q.Step != 0 {
		t.Errorf("stepSeconds = %d, the console sends 0", q.Step)
	}
	if q.Wide || q.AllEnvs {
		t.Errorf("the first read is the console's: one env, the default hour: %+v", q)
	}
	if !hasPredicates(q.Panel) {
		t.Errorf("the panel's own predicates are sent as declared: %v", q.Panel)
	}
	if q.PanelID != "p" {
		t.Errorf("panel id %q", q.PanelID)
	}
}

func TestAuditSelectsDashboardAndPanel(t *testing.T) {
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(linePanel("a"), linePanel("b"))},
		panel: func(auditQuery) (string, error) { return respData, nil }}
	out, _, err := auditRun(t, cp, "--dashboard", "d1", "--panel", "b")
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeAudit(t, out)
	if len(doc.Dashboards[0].Panels) != 1 || doc.Dashboards[0].Panels[0].ID != "b" {
		t.Errorf("%s", out)
	}
	if _, _, err := auditRun(t, cp, "--dashboard", "nope"); exitcode.Of(err) != 1 || !strings.Contains(err.Error(), "d1") {
		t.Errorf("an unknown dashboard lists the real ones: %v", err)
	}
	if _, _, err := auditRun(t, cp, "--panel", "zzz"); exitcode.Of(err) != 1 {
		t.Errorf("an unknown panel is a wrong request: %v", err)
	}
}

// Failures sort first, in the document and in the text table.
func TestAuditFailuresFirstAndTextTable(t *testing.T) {
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(linePanel("good"), linePanel("empty"), linePanel("boom"))},
		panel: func(q auditQuery) (string, error) {
			switch q.PanelID {
			case "good":
				return respData, nil
			case "boom":
				return "", &cloud.Error{HTTPStatus: 503, Code: cloud.CodeUnavailable}
			}
			return respEmpty, nil
		}}
	out, _, err := auditRun(t, cp)
	if exitcode.Of(err) != 1 {
		t.Fatalf("exit = %d, want 1: any unreadable panel outranks an empty one", exitcode.Of(err))
	}
	ids := []string{}
	for _, p := range decodeAudit(t, out).Dashboards[0].Panels {
		ids = append(ids, p.ID+":"+p.Status)
	}
	if got := strings.Join(ids, " "); got != "boom:error empty:empty good:ok" {
		t.Errorf("order = %s", got)
	}

	var stdout string
	stdout, _, _ = mxRun(t, mxCloud(cp), "dashboards", "audit", "prod")
	iBoom, iEmpty := strings.Index(stdout, "d1/boom"), strings.Index(stdout, "d1/empty")
	if iBoom < 0 || iEmpty < 0 || iBoom > iEmpty || strings.Contains(stdout, "d1/good") {
		t.Errorf("text table: failures first, ok hidden:\n%s", stdout)
	}
	if !strings.Contains(stdout, "STATUS") || !strings.Contains(stdout, "1 error, 1 empty") {
		t.Errorf("text table:\n%s", stdout)
	}
}

// Search panels are not queries; a typed view has no spec: neither is a failure.
func TestAuditNotApplicable(t *testing.T) {
	index := `{"dashboards":[
	 {"ref":{"source":"DASHBOARD_SOURCE_GENERATED","key":"d1"},"title":"Usage","source":"DASHBOARD_SOURCE_GENERATED"},
	 {"ref":{"source":"DASHBOARD_SOURCE_BUILTIN","key":"costs"},"title":"Costs","source":"DASHBOARD_SOURCE_BUILTIN","viewKind":"DASHBOARD_VIEW_KIND_ORGANIZATION_COSTS"}]}`
	cp := &auditCP{dashboards: index, specs: map[string]string{"d1": auditSpec(`{"id":"s","title":"s","kind":"search","signal":"logs","columns":["body"]}`)},
		panel: func(auditQuery) (string, error) { return "", errors.New("must not be called") }}
	out, _, err := auditRun(t, cp)
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeAudit(t, out)
	if doc.Summary.OK != 0 || doc.Summary.Error != 0 || cp.count(func(q auditQuery) bool { return q.Proc == cpQueryPanel }) != 0 {
		t.Errorf("%s", out)
	}
}

// The JSON document is a stable contract: the field set of a run is pinned.
func TestAuditJSONIsStable(t *testing.T) {
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(linePanel("empty"), linePanel("ok"))},
		panel: func(q auditQuery) (string, error) {
			if q.PanelID == "ok" {
				return respData, nil
			}
			if q.Wide && !q.AllEnvs {
				return respData, nil
			}
			return respEmpty, nil
		}}
	out, _, _ := auditRun(t, cp)
	obsGolden(t, "dashboards.audit.json", out)

	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema", "ok", "exit_code", "scope", "query_mode", "window", "summary", "dashboards"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("the document lacks %q", k)
		}
	}
	if doc["schema"] != "forge.obs.dashboards_audit/v1" {
		t.Errorf("schema %v", doc["schema"])
	}
	// every key the document emits is declared, so --fields can select it
	declared := map[string]bool{}
	for _, f := range obsDashboardsAuditSchema.Fields {
		declared[f] = true
	}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				if !declared[p] && !alwaysKeptKey(k, prefix) {
					t.Errorf("document field %q is not in the schema", p)
				}
				walk(p, c)
			}
		case []any:
			for _, c := range x {
				walk(prefix, c)
			}
		}
	}
	walk("", doc)
}

func alwaysKeptKey(k, prefix string) bool {
	if prefix != "" {
		return false
	}
	switch k {
	case "schema", "ok", "exit_code", "error", "truncated", "omitted", "next_page_token":
		return true
	}
	return false
}

// A local env has no dashboards to replay: could not look, with a pointer to
// the check that does apply.
func TestAuditLocalEnvCannotBeReplayed(t *testing.T) {
	out, _, err := mxRun(t, mxLocal(mxFixture()), "dashboards", "audit", "dev", "--json")
	if exitcode.Of(err) != 2 {
		t.Fatalf("exit = %d (%v)", exitcode.Of(err), err)
	}
	if !strings.Contains(out, "local ClickStack") {
		t.Errorf("%s", out)
	}
}

func TestAuditFailureClasses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want obsbackend.FailureClass
	}{
		{"403", &cloud.Error{HTTPStatus: 403}, obsbackend.ClassAuth},
		{"401", &cloud.Error{HTTPStatus: 401, Code: cloud.CodeUnauthenticated}, obsbackend.ClassAuth},
		{"quota reason", &cloud.Error{Code: cloud.CodeResourceExhausted, Reason: "obs_query_quota"}, obsbackend.ClassQuota},
		{"soft quota reason", &cloud.Error{Code: cloud.CodeResourceExhausted, Reason: "obs_query_quota_soft"}, obsbackend.ClassQuota},
		{"in flight reason", &cloud.Error{Code: cloud.CodeResourceExhausted, Reason: "in_flight_limit"}, obsbackend.ClassInFlight},
		{"in flight wording only", &cloud.Error{Code: cloud.CodeResourceExhausted, Message: "too many observability queries in flight; retry shortly"}, obsbackend.ClassInFlight},
		{"rate limited", &cloud.Error{Code: cloud.CodeResourceExhausted, Reason: "rate_limited"}, obsbackend.ClassRateLimited},
		{"too much data", &cloud.Error{Code: cloud.CodeResourceExhausted, Message: "the query touched too much data; narrow the time range"}, obsbackend.ClassTooMuchData},
		{"unavailable", &cloud.Error{Code: cloud.CodeUnavailable}, obsbackend.ClassStorage},
		{"not set up", &cloud.Error{Code: cloud.CodeFailedPrecondition}, obsbackend.ClassStorage},
		{"deadline", &cloud.Error{Code: cloud.CodeDeadlineExceeded}, obsbackend.ClassTimeout},
		{"504", &cloud.Error{HTTPStatus: 504}, obsbackend.ClassTimeout},
		{"invalid", &cloud.Error{Code: cloud.CodeInvalidArgument}, obsbackend.ClassInvalid},
		{"unimplemented", &cloud.Error{Code: cloud.CodeUnimplemented}, obsbackend.ClassUnsupported},
		{"transport", errors.New("connection refused"), obsbackend.ClassUnreachable},
		{"500", &cloud.Error{HTTPStatus: 500}, obsbackend.ClassServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := obsbackend.FailureOf(tc.err).Class; got != tc.want {
				t.Errorf("class = %q, want %q", got, tc.want)
			}
		})
	}
	if !obsbackend.FailureOf(&cloud.Error{Code: cloud.CodeResourceExhausted, Reason: "in_flight_limit"}).Transient() {
		t.Error("in flight is transient")
	}
	if obsbackend.FailureOf(&cloud.Error{Code: cloud.CodeResourceExhausted, Reason: "obs_query_quota"}).Transient() {
		t.Error("a spent quota is not transient")
	}
}

// The probe is never wider than the signal allows, and never narrower than the
// window it follows.
func TestAuditProbeIsBoundedByTheWindow(t *testing.T) {
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(linePanel("p"))},
		panel: func(auditQuery) (string, error) { return respEmpty, nil }}
	out, _, _ := auditRun(t, cp, "--since", "24h", "--probe-window", "1h")
	p := decodeAudit(t, out).Dashboards[0].Panels[0]
	if p.Status != "empty" || p.Probe == nil || p.Probe.Steps[0].HasData {
		t.Errorf("%+v", p)
	}
	if p.Probe.Verdict != "not_probed" || !strings.Contains(out, "not wider than the audit window") {
		t.Errorf("a probe that cannot be wider says so:\n%s", out)
	}
}

// The text form is failures first, one line per panel, with the cause.
func TestAuditTextGolden(t *testing.T) {
	cp := &auditCP{dashboards: auditIndex, specs: map[string]string{"d1": auditSpec(
		linePanel("ok-panel"), linePanel("outside-window"), metricPanel("metric-gone", "rpc.server.calls"),
		linePanel("denied"), linePanel("daily-bucket"))},
		metrics: `{"metrics":[]}`,
		panel: func(q auditQuery) (string, error) {
			switch q.PanelID {
			case "ok-panel":
				return respData, nil
			case "outside-window":
				if q.Wide && !q.AllEnvs {
					return respData, nil
				}
				return respEmpty, nil
			case "metric-gone":
				return `{"kind":"line","stepSeconds":30,"warnings":["metric_absent: rpc.server.calls"]}`, nil
			case "denied":
				return "", &cloud.Error{HTTPStatus: http.StatusForbidden, Code: cloud.CodePermissionDenied, Message: "this credential does not carry the obs:read scope"}
			}
			return respOne, nil
		}}
	out, _, err := mxRun(t, mxCloud(cp), "dashboards", "audit", "prod")
	if exitcode.Of(err) != 1 {
		t.Fatalf("exit = %d, want 1 (a refused panel outranks the empty ones)", exitcode.Of(err))
	}
	obsGolden(t, "dashboards.audit.txt", out)
}
