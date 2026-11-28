package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/internal/obsout"
	"github.com/reliant-labs/forge/pkg/obsquery"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

func TestCovDAlertSubject(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   string
	}{
		{nil, ""},
		{map[string]string{"severity": "critical"}, ""},
		{map[string]string{"namespace": "prod", "name": "api"}, "  namespace=prod name=api"},
		{map[string]string{"pod": "p-1", "job": "j", "workload": "w", "machine": "m", "name": "n", "namespace": "ns"}, "  namespace=ns name=n workload=w job=j pod=p-1 machine=m"},
		{map[string]string{"namespace": ""}, ""},
	}
	for _, c := range cases {
		if got := alertSubject(c.labels); got != c.want {
			t.Errorf("alertSubject(%v) = %q, want %q", c.labels, got, c.want)
		}
	}
}

func TestCovDSubjectName(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		name string
		sub  releasehealth.Subject
		want string
	}{
		{"inferred release", releasehealth.Subject{Release: "v1.2.3", Inferred: true}, "release v1.2.3 (inferred marker: first seen in telemetry)"},
		{"recorded release", releasehealth.Subject{Release: "v1.2.3"}, "release v1.2.3"},
		{"promotion only", releasehealth.Subject{PromotionID: "p-9"}, "promotion p-9"},
		{"a bare time", releasehealth.Subject{PromotedAt: at}, "the release started 2026-11-20T09:30:00Z"},
	}
	for _, c := range cases {
		if got := subjectName(releasehealth.Report{Subject: c.sub}); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCovDCheckBaselineWindow(t *testing.T) {
	th := func(name, v string) releasehealth.Threshold { return releasehealth.Threshold{Name: name, Value: v} }
	cases := []struct {
		name string
		ths  []releasehealth.Threshold
		want time.Duration
	}{
		{"declared", []releasehealth.Threshold{th("other", "1m"), th(releasehealth.ThresholdBaselineWindow, "45m")}, 45 * time.Minute},
		{"absent falls back to the default", nil, release.DefaultHealthBaselineWindow},
		{"unparseable falls back", []releasehealth.Threshold{th(releasehealth.ThresholdBaselineWindow, "soon")}, release.DefaultHealthBaselineWindow},
		{"non-positive falls back", []releasehealth.Threshold{th(releasehealth.ThresholdBaselineWindow, "-5m")}, release.DefaultHealthBaselineWindow},
	}
	for _, c := range cases {
		if got := checkBaselineWindow(c.ths); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCovDFiringHealthAlerts_OnlyFiringOnesSurvive(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	got := firingHealthAlerts([]obsbackend.Alert{
		{Rule: "HighErrors", Fingerprint: "f1", State: obsbackend.AlertFiring, FiringAt: at, Labels: map[string]string{"name": "api"}},
		{Rule: "Resolved", State: "resolved"},
		{Rule: "Pending", State: "pending"},
	})
	if len(got) != 1 || got[0].Rule != "HighErrors" || got[0].Fingerprint != "f1" || !got[0].FiringAt.Equal(at) || got[0].Labels["name"] != "api" {
		t.Errorf("got %+v", got)
	}
	if firingHealthAlerts(nil) != nil {
		t.Error("no alerts, no result")
	}
}

func TestCovDCheckOptions(t *testing.T) {
	opts, err := checkOptions(obsCheckOpts{window: 30 * time.Minute})
	if err != nil || opts.Window != 30*time.Minute {
		t.Fatalf("defaults: %+v %v", opts, err)
	}
	for _, c := range []struct {
		name string
		o    obsCheckOpts
	}{
		{"bad --fail-on", obsCheckOpts{failOn: "nonsense"}},
		{"bad --threshold", obsCheckOpts{thresholds: []string{"no-equals-sign"}}},
	} {
		_, err := checkOptions(c.o)
		if err == nil {
			t.Errorf("%s: want an error", c.name)
			continue
		}
		if exitCodeForError(err) != 1 {
			t.Errorf("%s: a bad flag is a wrong-input exit 1, got %d (%v)", c.name, exitCodeForError(err), err)
		}
		if strings.Contains(err.Error(), releasehealth.ErrInvalid.Error()+": ") {
			t.Errorf("%s: the sentinel prefix must be stripped from what the user sees: %v", c.name, err)
		}
	}
}

func TestCovDLocalPromotedAt(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		o       obsCheckOpts
		want    time.Time
		wantErr string
	}{
		{"a duration", obsCheckOpts{since: "20m"}, now.Add(-20 * time.Minute), ""},
		{"an RFC 3339 time", obsCheckOpts{since: "2026-11-20T10:00:00+01:00"}, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), ""},
		{"a zero duration", obsCheckOpts{since: "0s"}, time.Time{}, "--since must be positive"},
		{"a negative duration", obsCheckOpts{since: "-5m"}, time.Time{}, "--since must be positive"},
		{"neither", obsCheckOpts{since: "yesterday"}, time.Time{}, `--since "yesterday" is neither a duration (20m) nor an RFC 3339 time`},
		{"--release on a local env", obsCheckOpts{release: "v1"}, time.Time{}, "no promotions to pick by --release or --promotion"},
		{"--promotion on a local env", obsCheckOpts{promotion: "p1"}, time.Time{}, "no promotions to pick by --release or --promotion"},
	}
	for _, c := range cases {
		got, err := localPromotedAt(now, "dev", c.o)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

func TestCovDLocalPromotedAt_FallsBackToTheLastEnvUp(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	prev := obsLocalSessionStart
	t.Cleanup(func() { obsLocalSessionStart = prev })

	obsLocalSessionStart = func(env string) (time.Time, bool, error) {
		if env != "dev" {
			t.Errorf("asked about %q", env)
		}
		return start, true, nil
	}
	if got, err := localPromotedAt(now, "dev", obsCheckOpts{}); err != nil || !got.Equal(start) {
		t.Errorf("recorded session: %v %v", got, err)
	}

	obsLocalSessionStart = func(string) (time.Time, bool, error) { return time.Time{}, false, nil }
	_, err := localPromotedAt(now, "dev", obsCheckOpts{})
	if err == nil || exitCodeForError(err) != exitUndetermined || !strings.Contains(err.Error(), "no `forge env up dev` is recorded on this machine") {
		t.Errorf("no session: %v (exit %d)", err, exitCodeForError(err))
	}

	obsLocalSessionStart = func(string) (time.Time, bool, error) { return time.Time{}, false, context.DeadlineExceeded }
	_, err = localPromotedAt(now, "dev", obsCheckOpts{})
	if err == nil || exitCodeForError(err) != exitUndetermined || !strings.Contains(err.Error(), "cannot read this machine's ledger for dev's last start") {
		t.Errorf("unreadable ledger: %v", err)
	}
}

func TestCovDWriteCheckText_RendersEverySection(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	share := 0.5
	doc := obsCheckDoc{
		Verdict: "FAIL",
		Scope:   obsout.Scope{Env: "prod", Backend: "clickhouse"},
		Subject: &releasehealth.Subject{Release: "v2", Inferred: true, PromotedAt: t0, LiveAt: t0.Add(5 * time.Minute)},
		Windows: &obsCheckWindows{
			Before: obsCheckRange{From: t0.Add(-time.Hour), To: t0},
			After:  obsCheckRange{From: t0.Add(5 * time.Minute), To: t0.Add(time.Hour)},
		},
		EarlyPass: "no errors for 10m",
		Reason:    "too little traffic",
		Services: []releasehealth.ServiceHealth{
			{Service: "api", Before: releasehealth.WindowStats{Requests: 100, ErrorRate: 0.01, P95Ms: 120}, After: releasehealth.WindowStats{Requests: 90, ErrorRate: 0.1, P95Ms: 300}, VersionShare: &share},
			{Service: "edge", Signal: releasehealth.SignalIngress},
		},
		Findings: []releasehealth.Finding{
			{Policy: "fail", Code: "error_rate", Message: "api error rate rose"},
			{Policy: "warn", Code: "changed_op_latency_regression", Message: "slower", Unattributed: "also slow before"},
		},
		StandingCritical:  []releasehealth.Alert{{Rule: "DiskFull", FiringAt: t0.Add(-time.Hour), Labels: map[string]string{"machine": "m1"}}},
		ExcludedAlerts:    []releasehealth.Alert{{Rule: "Noise", Labels: map[string]string{"job": "batch"}}},
		IssueAlerts:       []releasehealth.Alert{{Rule: releasehealth.AlertNewIssue, Labels: map[string]string{releasehealth.LabelIssueID: "iss-1"}}},
		BaselineAlerts:    []releasehealth.Alert{{Rule: "Flappy", BaselineFirings: 3}},
		PreExistingAlerts: []releasehealth.Alert{{Rule: "OldBug", Issue: &releasehealth.PreExistingIssue{ID: "iss-0", FirstSeenAt: t0.Add(-48 * time.Hour), FirstVersion: "v1"}}},
		NotEvaluated:      []string{"browser", "logs"},
		Thresholds: []releasehealth.Threshold{
			{Name: "error_rate", Value: "0.05", Source: releasehealth.SourceRequest},
			{Name: "latency", Value: "2", Source: "default"},
		},
	}
	var b bytes.Buffer
	writeCheckText(&b, doc)
	out := b.String()
	for _, want := range []string{
		"FAIL  v2 on prod (clickhouse backend)",
		"marker INFERRED: no deploy of v2 was recorded",
		"before 2026-11-20T09:00:00Z .. 2026-11-20T10:00:00Z",
		"after  2026-11-20T10:05:00Z .. 2026-11-20T11:00:00Z  (still open)",
		"early pass (--early-pass): no errors for 10m",
		"inconclusive: too little traffic",
		"SERVICE", "api", "100 -> 90", "1.0% -> 10.0%", "120ms -> 300ms", "50%",
		"edge (" + releasehealth.SignalIngress + ")",
		"live   2026-11-20T10:05:00Z (promoted 2026-11-20T10:00:00Z; its deploy gates say when it shipped; the after window starts there)",
		"fail error_rate",
		"warn changed_op_latency_regression slower",
		"not this release's: also slow before",
		"critical alert DiskFull firing since 2026-11-20T09:00:00Z (before the promotion)  machine=m1",
		"alert Noise firing  job=batch: not counted",
		"alert " + releasehealth.AlertNewIssue + " firing for issue iss-1: judged as the issue",
		"alert Flappy firing: not counted (it also fired 3 time(s) in the baseline window",
		"alert OldBug firing: not counted (its issue iss-0 was first seen v1,",
		"not evaluated: browser, logs",
		"threshold error_rate=0.05 (from this call)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "threshold latency") {
		t.Errorf("only thresholds set by this call are echoed:\n%s", out)
	}

	b.Reset()
	doc.Windows.AfterComplete = true
	doc.Subject.Inferred = false
	writeCheckText(&b, doc)
	if strings.Contains(b.String(), "(still open)") || strings.Contains(b.String(), "marker INFERRED") {
		t.Errorf("a complete window on a recorded deploy has neither note:\n%s", b.String())
	}
}

func TestCovDWriteCheckText_WithoutASubjectPrintsTheExplainedStatements(t *testing.T) {
	var b bytes.Buffer
	writeCheckText(&b, obsCheckDoc{Statements: []obsbackend.Statement{{Purpose: "before window", SQL: "SELECT 1\n", Params: map[string]string{"env": "prod"}}}})
	if got := b.String(); !strings.Contains(got, "-- before window\nSELECT 1\n--param_env=prod") {
		t.Errorf("explain output:\n%s", got)
	}
}

func TestCovDCheckErrText(t *testing.T) {
	if checkErrText(nil) != "" {
		t.Error("nil error is empty text")
	}
	if checkErrText(context.Canceled) != "context canceled" {
		t.Error("an error is its message")
	}
}

func TestCovDExplainRunnerReturnsNoRows(t *testing.T) {
	res, err := explainRunner{}.Run(context.Background(), obsquery.Stmt{}, obsbackend.Limits{})
	if err != nil || len(res.Rows) != 0 {
		t.Errorf("explain must run nothing: %+v %v", res, err)
	}
}
