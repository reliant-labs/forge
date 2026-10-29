package cli

// `forge obs dashboards audit <env>` answers two questions about a dashboard
// that looks empty: do we have the data, and why is it not showing if we do.
//
// REPLAY, NEVER RE-IMPLEMENT. The audit lists the dashboards the console lists
// for the env (DashboardService), reads each spec, and sends each panel to the
// same server-side QueryPanel the console calls, over the window the console
// opens with (its default is the last hour; --since moves it) and with the step
// left to the control plane (the console sends 0). No panel SQL is compiled
// here, so the audit cannot drift from what the console is told. The step the
// control plane chose is read back from the answer; the window is what was
// sent, because the control plane refuses a range over a signal's maximum
// instead of clamping it.
//
// PROBE. A panel that comes back empty is asked again, over a wider window, in
// every environment of the project, and without its filters, so the verdict
// separates "no data at all" from "data exists outside this window / scope /
// filter". A metrics panel also looks its metric up in the catalog.
//
// A refused or failed read is a failure with a class (auth, quota, in flight,
// storage, ...), never "no data".
//
// EXIT CODES: 0 every panel is ok (or not applicable); 2 any panel is empty or
// partial; 1 any panel could not be read. The head's exit_code is the process
// status.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/internal/obsout"
	"github.com/reliant-labs/forge/pkg/dashspec"
	"github.com/reliant-labs/forge/pkg/obsquery"
)

const schemaObsDashboardsAudit = "forge.obs.dashboards_audit/v1"

var obsDashboardsAuditSchema = obsout.Schema{
	Name: schemaObsDashboardsAudit,
	Fields: []string{
		"scope", "scope.env", "scope.backend", "scope.env_id", "scope.from", "scope.to", "scope.services",
		"query_mode", "project", "window", "window.since", "window.from", "window.to", "window.probe_since",
		"summary", "summary.dashboards", "summary.panels", "summary.ok", "summary.partial", "summary.empty",
		"summary.error", "summary.not_applicable", "summary.all_zero", "summary.dashboards_unread",
		"dashboards", "dashboards.id", "dashboards.title", "dashboards.source", "dashboards.release_version",
		"dashboards.status", "dashboards.error", "dashboards.error.class", "dashboards.error.code",
		"dashboards.error.reason", "dashboards.error.http_status", "dashboards.error.message",
		"dashboards.error.missing_scope", "dashboards.error.retry_after_seconds", "dashboards.error.resets_at",
		"dashboards.panels", "dashboards.panels.id", "dashboards.panels.title", "dashboards.panels.kind",
		"dashboards.panels.signal", "dashboards.panels.metrics", "dashboards.panels.status",
		"dashboards.panels.reason", "dashboards.panels.probable_cause", "dashboards.panels.note",
		"dashboards.panels.series", "dashboards.panels.rows", "dashboards.panels.points",
		"dashboards.panels.non_null_points", "dashboards.panels.non_zero_points", "dashboards.panels.buckets",
		"dashboards.panels.items", "dashboards.panels.last_data_at", "dashboards.panels.from", "dashboards.panels.to",
		"dashboards.panels.step_seconds", "dashboards.panels.truncated", "dashboards.panels.series_total",
		"dashboards.panels.warnings", "dashboards.panels.attempts",
		"dashboards.panels.error", "dashboards.panels.error.class", "dashboards.panels.error.code",
		"dashboards.panels.error.reason", "dashboards.panels.error.http_status", "dashboards.panels.error.message",
		"dashboards.panels.error.missing_scope", "dashboards.panels.error.retry_after_seconds",
		"dashboards.panels.error.resets_at",
		"dashboards.panels.probe", "dashboards.panels.probe.from", "dashboards.panels.probe.to",
		"dashboards.panels.probe.verdict", "dashboards.panels.probe.steps", "dashboards.panels.probe.steps.name",
		"dashboards.panels.probe.steps.has_data", "dashboards.panels.probe.steps.non_zero_points",
		"dashboards.panels.probe.steps.last_data_at", "dashboards.panels.probe.steps.error",
		"dashboards.panels.probe.steps.error.class", "dashboards.panels.probe.steps.error.code", "dashboards.panels.probe.steps.error.reason",
		"dashboards.panels.probe.steps.error.http_status", "dashboards.panels.probe.steps.error.message", "dashboards.panels.probe.steps.error.missing_scope",
		"dashboards.panels.probe.steps.error.retry_after_seconds", "dashboards.panels.probe.steps.error.resets_at",
		"dashboards.panels.probe.catalog", "dashboards.panels.probe.catalog.metric", "dashboards.panels.probe.catalog.seen",
		"dashboards.panels.probe.catalog.type", "dashboards.panels.probe.catalog.last_seen",
		"dashboards.panels.probe.catalog.services", "dashboards.panels.probe.catalog.declared_type",
		"dashboards.panels.probe.catalog.error",
		"notes", "suggested",
	},
}

// Panel statuses.
const (
	auditOK            = "ok"
	auditPartial       = "partial"
	auditEmpty         = "empty"
	auditError         = "error"
	auditNotApplicable = "not_applicable"
)

// Probe verdicts.
const (
	verdictOutsideWindow  = "data_outside_window"
	verdictOtherEnv       = "data_in_other_env"
	verdictExcludedFilter = "data_excluded_by_filters"
	verdictNoneAnywhere   = "no_data_anywhere"
	verdictMetricNever    = "metric_never_seen"
	verdictProbeFailed    = "probe_failed"
	verdictNotProbed      = "not_probed"
)

// The wider window an empty panel is probed over.
const auditProbeWindow = 7 * 24 * time.Hour

const (
	// auditConcurrency is how many control-plane reads run at once. A CLI
	// credential has a request budget of its own (about 60 a minute), and the
	// console's per-user cap of concurrent reads is six.
	auditConcurrency = 4
	// auditAttempts is how many times a transient refusal (in flight, rate
	// limited) is retried before it is reported.
	auditAttempts = 3
	// auditMaxWait caps one retry wait.
	auditMaxWait = 10 * time.Second
)

// auditSleep waits d or until ctx ends; tests replace it.
var auditSleep = func(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

type auditFailure struct {
	Class             string    `json:"class"`
	Code              string    `json:"code,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	HTTPStatus        int       `json:"http_status,omitempty"`
	Message           string    `json:"message"`
	MissingScope      string    `json:"missing_scope,omitempty"`
	RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
	ResetsAt          time.Time `json:"resets_at,omitzero"`
}

func auditFailureOf(f obsbackend.Failure) *auditFailure {
	return &auditFailure{
		Class: string(f.Class), Code: f.Code, Reason: f.Reason, HTTPStatus: f.HTTPStatus, Message: f.Message,
		MissingScope: f.MissingScope, RetryAfterSeconds: int(f.RetryAfter / time.Second), ResetsAt: f.ResetsAt,
	}
}

type auditProbeStep struct {
	Name       string        `json:"name"`
	HasData    bool          `json:"has_data"`
	NonZero    int           `json:"non_zero_points"`
	LastDataAt time.Time     `json:"last_data_at,omitzero"`
	Error      *auditFailure `json:"error,omitempty"`
}

type auditCatalog struct {
	Metric       string    `json:"metric"`
	Seen         bool      `json:"seen"`
	Type         string    `json:"type,omitempty"`
	LastSeen     time.Time `json:"last_seen,omitzero"`
	Services     []string  `json:"services,omitempty"`
	DeclaredType string    `json:"declared_type,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type auditProbe struct {
	From    time.Time        `json:"from"`
	To      time.Time        `json:"to"`
	Verdict string           `json:"verdict"`
	Steps   []auditProbeStep `json:"steps"`
	Catalog []auditCatalog   `json:"catalog,omitempty"`
}

type auditPanel struct {
	ID            string        `json:"id"`
	Title         string        `json:"title"`
	Kind          string        `json:"kind"`
	Signal        string        `json:"signal,omitempty"`
	Metrics       []string      `json:"metrics,omitempty"`
	Status        string        `json:"status"`
	Reason        string        `json:"reason,omitempty"`
	ProbableCause string        `json:"probable_cause,omitempty"`
	Note          string        `json:"note,omitempty"`
	Series        int           `json:"series"`
	Rows          int           `json:"rows"`
	Points        int           `json:"points"`
	NonNull       int           `json:"non_null_points"`
	NonZero       int           `json:"non_zero_points"`
	Buckets       int           `json:"buckets"`
	Items         int           `json:"items"`
	LastDataAt    time.Time     `json:"last_data_at,omitzero"`
	From          time.Time     `json:"from"`
	To            time.Time     `json:"to"`
	StepSeconds   int64         `json:"step_seconds,omitempty"`
	Truncated     bool          `json:"truncated,omitempty"`
	SeriesTotal   int           `json:"series_total,omitempty"`
	Warnings      []string      `json:"warnings,omitempty"`
	Attempts      int           `json:"attempts,omitempty"`
	Error         *auditFailure `json:"error,omitempty"`
	Probe         *auditProbe   `json:"probe,omitempty"`

	// firstColumnType is a sql panel's first result column type.
	firstColumnType string
}

type auditDashboard struct {
	ID             string        `json:"id"`
	Title          string        `json:"title"`
	Source         string        `json:"source"`
	ReleaseVersion string        `json:"release_version,omitempty"`
	Status         string        `json:"status"`
	Error          *auditFailure `json:"error,omitempty"`
	Panels         []auditPanel  `json:"panels"`
}

type auditSummary struct {
	Dashboards       int `json:"dashboards"`
	Panels           int `json:"panels"`
	OK               int `json:"ok"`
	Partial          int `json:"partial"`
	Empty            int `json:"empty"`
	Error            int `json:"error"`
	NotApplicable    int `json:"not_applicable"`
	AllZero          int `json:"all_zero"`
	DashboardsUnread int `json:"dashboards_unread"`
}

type auditWindow struct {
	Since      string    `json:"since"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	ProbeSince string    `json:"probe_since,omitempty"`
}

type obsDashboardsAuditDoc struct {
	obsout.Head
	Scope     obsout.Scope     `json:"scope"`
	QueryMode string           `json:"query_mode"`
	Project   string           `json:"project,omitempty"`
	Window    auditWindow      `json:"window"`
	Summary   auditSummary     `json:"summary"`
	Dashboard []auditDashboard `json:"dashboards"`
	Notes     []string         `json:"notes,omitempty"`
	Suggested []string         `json:"suggested,omitempty"`
}

type obsDashboardsAuditOpts struct {
	out        obsout.Flags
	backend    string
	dashboard  string
	panel      string
	since      time.Duration
	probeSince time.Duration
	noProbe    bool
	all        bool
}

func newObsDashboardsAuditCmd() *cobra.Command {
	var o obsDashboardsAuditOpts
	cmd := &cobra.Command{
		Use:   "audit [env]",
		Short: "Why is a dashboard empty? Replay each panel as the console asks and probe the empty ones",
		Long: `Run every panel of [env]'s dashboards (default dev) through the control plane's
QueryPanel, exactly as the console does, and say for each one whether it has
data, and if not, why not.

What is replayed: the dashboards the console lists for the env, each panel sent
as the console sends it (no viewer filters), over the last --since (default 1h,
the console's default range) with the step left to the control plane. The
control plane's own warnings (metric_absent, step_raised, ...) and the step it
chose are reported as returned. No panel SQL is rebuilt here.

Each panel is one of:

  ok              it returned data the console can draw
  partial         data came back, but not all of it: a series whose metric is
                  absent, a metric stored as another kind, or one bucket where a
                  line needs two (a day-wide bucket under a short window)
  empty           nothing came back; the probe says whether the data exists
  error           the read failed; the class says auth, quota, in flight,
                  storage, ...: a failed read is never reported as no data
  not_applicable  not a query the console runs (search panels, typed views)

An empty panel is probed (--no-probe skips it): the same panel over --probe-window
(default 168h), over every environment of the project, and without its filters, and
a metrics panel is looked up in the metric catalog. The verdict is one of
data_outside_window, data_in_other_env, data_excluded_by_filters,
metric_never_seen or no_data_anywhere.

Failures are listed first. EXIT: 0 every panel ok; 2 any panel empty or partial;
1 any panel could not be read. The env's telemetry must be stored by the control
plane (a local ClickStack has no dashboards to replay).

  ` + Name() + ` obs dashboards audit prod
  ` + Name() + ` obs dashboards audit prod --dashboard product-usage --since 24h
  ` + Name() + ` obs dashboards audit prod --dashboard product-usage --panel workflows-by-outcome --json

` + obsDashboardsAuditSchema.Help(),
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runObsDashboardsAudit(cmd.Context(), defaultObsDeps(cmd), obsEnvArg(args, 0), o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.out.JSON, "json", false, "print one JSON document on stdout (see the schema below; progress goes to stderr)")
	f.StringVar(&o.out.Fields, "fields", "", "with --json: keep only these comma-separated dotted fields (the head is always kept)")
	f.StringVar(&o.backend, "backend", "", "local | cloud (default: from the env's declaration)")
	f.StringVar(&o.dashboard, "dashboard", "", "only this dashboard (its id)")
	f.StringVar(&o.panel, "panel", "", "only this panel (its id)")
	f.DurationVar(&o.since, "since", time.Hour, "the window each panel is run over (the console opens on 1h)")
	f.DurationVar(&o.probeSince, "probe-window", auditProbeWindow, "the wider window an empty panel is probed over")
	f.BoolVar(&o.noProbe, "no-probe", false, "do not probe empty panels")
	f.BoolVar(&o.all, "all", false, "list the ok panels too (text output; --json always lists every panel)")
	return cmd
}

func runObsDashboardsAudit(ctx context.Context, d obsDeps, env string, o obsDashboardsAuditOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}
	fields, err := o.out.Parsed(obsDashboardsAuditSchema)
	if err != nil {
		return obsFail(d, o.out.JSON, schemaObsDashboardsAudit, err)
	}
	fail := func(err error) error { return obsFail(d, o.out.JSON, schemaObsDashboardsAudit, err) }
	rng, err := resolveRange(d.Now(), o.since, "", "")
	if err != nil {
		return fail(err)
	}
	b, _, err := (&obsCommon{backend: o.backend}).open(ctx, d, env, "dashboards audit")
	if err != nil {
		return fail(err)
	}
	dr, ok := b.(obsbackend.DashboardReader)
	if !ok {
		return fail(obsbackend.Undetermined("env %s keeps its telemetry in a local ClickStack; dashboards are stored by the control plane, so there is nothing to replay", env).
			With(obsbackend.Cmd("forge", "obs", "dashboards", "check", "--against", env)))
	}

	project := hostedProjectName()
	narrow := obsbackend.Narrow{Range: rng}
	doc := obsDashboardsAuditDoc{
		Scope: obsScope(b.Info(), narrow), QueryMode: QueryModeStructured, Project: project,
		Window:    auditWindow{Since: durText(o.since), From: rng.From, To: rng.To},
		Dashboard: []auditDashboard{},
	}
	if !o.noProbe {
		doc.Window.ProbeSince = durText(o.probeSince)
	}

	index, err := dr.ListDashboards(ctx, project)
	if err != nil {
		return fail(auditListFailure(env, err))
	}
	picked, err := pickDashboards(index, o.dashboard)
	if err != nil {
		return fail(err)
	}

	a := &dashAudit{dr: dr, b: b, ctx: ctx, rng: rng, opts: o, project: project, sem: make(chan struct{}, auditConcurrency), catalog: map[string]*auditCatalog{}}
	doc.Dashboard = a.run(picked)
	if o.panel != "" && doc.panelCount() == 0 {
		return fail(obsbackend.Wrong("no panel %q in the selected dashboard(s)", o.panel).
			With(obsbackend.Cmd("forge", "obs", "dashboards", "audit", env, "--json", "--fields", "dashboards.id,dashboards.panels.id")))
	}
	doc.sortFailuresFirst()
	doc.summarize()
	doc.Notes = a.notes()
	doc.Suggested = auditSuggestions(env, doc)

	verdict := doc.verdict(env)
	doc.Stamp(schemaObsDashboardsAudit, verdict)
	if done, err := o.out.Emit(d.Stdout, obsDashboardsAuditSchema, doc, fields); done || err != nil {
		if err != nil {
			return err
		}
		return verdict
	}
	writeAuditText(d.Stdout, env, doc, o.all)
	return verdict
}

// auditListFailure explains why the index could not be read. A refused
// credential is an auth problem and says so, whatever the status.
func auditListFailure(env string, err error) error {
	f := obsbackend.FailureOf(err)
	if f.Class != obsbackend.ClassAuth {
		return err
	}
	msg := fmt.Sprintf("the control plane refused the credential listing env %s's dashboards (HTTP %d)", env, f.HTTPStatus)
	if f.MissingScope != "" {
		msg += ": it lacks the " + f.MissingScope + " scope"
	}
	return obsbackend.Undetermined("%s: an auth/scope problem, not missing data", msg).Cause(err)
}

func pickDashboards(index []obsbackend.DashboardInfo, id string) ([]obsbackend.DashboardInfo, error) {
	if id == "" {
		return index, nil
	}
	var out []obsbackend.DashboardInfo
	for _, di := range index {
		if di.Ref.Key == id {
			out = append(out, di)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	var have []string
	for _, di := range index {
		have = append(have, di.Ref.Key)
	}
	sort.Strings(have)
	return nil, obsbackend.Wrong("no dashboard %q in this env's index; it lists: %s", id, strings.Join(have, ", "))
}

// ── the run ─────────────────────────────────────────────────────────────

type dashAudit struct {
	dr      obsbackend.DashboardReader
	b       obsbackend.Backend
	ctx     context.Context
	rng     obsbackend.Range
	opts    obsDashboardsAuditOpts
	project string
	sem     chan struct{}

	mu      sync.Mutex
	quota   *obsbackend.Failure
	catalog map[string]*auditCatalog
	skipped int
}

func (a *dashAudit) notes() []string {
	var out []string
	if a.skipped > 0 {
		out = append(out, fmt.Sprintf("%d read(s) were skipped after the query allowance ran out; they are reported as quota errors", a.skipped))
	}
	return out
}

func (a *dashAudit) run(picked []obsbackend.DashboardInfo) []auditDashboard {
	out := make([]auditDashboard, len(picked))
	var wg sync.WaitGroup
	for i, di := range picked {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = a.dashboard(di)
		}()
	}
	wg.Wait()
	return out
}

func (a *dashAudit) dashboard(di obsbackend.DashboardInfo) auditDashboard {
	ad := auditDashboard{ID: di.Ref.Key, Title: di.Title, Source: di.Source, ReleaseVersion: di.ReleaseVersion, Panels: []auditPanel{}}
	if di.Typed || di.Source == "builtin" {
		ad.Status = auditNotApplicable
		return ad
	}
	var doc obsbackend.DashboardDocument
	_, err := a.guarded(func() error {
		var err error
		doc, err = a.dr.GetDashboard(a.ctx, di.Ref)
		return err
	})
	if err != nil {
		ad.Error = auditFailureOf(obsbackend.FailureOf(err))
		ad.Status = auditError
		return ad
	}
	var spec dashspec.Dashboard
	if err := json.Unmarshal(doc.Spec, &spec); err != nil {
		ad.Error = &auditFailure{Class: string(obsbackend.ClassInvalid), Message: "the stored spec is not a dashboard: " + err.Error()}
		ad.Status = auditError
		return ad
	}
	if ad.Title == "" {
		ad.Title = spec.Title
	}
	var wg sync.WaitGroup
	panels := make([]auditPanel, 0, len(spec.Panels))
	for _, p := range spec.Panels {
		if a.opts.panel != "" && p.ID != a.opts.panel {
			continue
		}
		panels = append(panels, auditPanel{})
		i := len(panels) - 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			panels[i] = a.panel(p)
		}()
	}
	wg.Wait()
	ad.Panels = panels
	ad.Status = rollup(panels)
	return ad
}

// guarded runs fn on a read slot, retrying a transient refusal (too many reads
// in flight, a spent request budget) after the wait the control plane asked
// for. A spent query allowance is not retried: it ends the run's reads, so the
// remaining panels say so instead of hammering a closed door.
func (a *dashAudit) guarded(fn func() error) (attempts int, err error) {
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-a.ctx.Done():
		return 0, a.ctx.Err()
	}
	for attempts = 1; ; attempts++ {
		a.mu.Lock()
		q := a.quota
		a.mu.Unlock()
		if q != nil {
			a.mu.Lock()
			a.skipped++
			a.mu.Unlock()
			return attempts - 1, &skippedError{f: *q}
		}
		err = fn()
		if err == nil {
			return attempts, nil
		}
		f := obsbackend.FailureOf(err)
		if f.Class == obsbackend.ClassQuota {
			a.mu.Lock()
			if a.quota == nil {
				a.quota = &f
			}
			a.mu.Unlock()
			return attempts, err
		}
		if !f.Transient() || attempts >= auditAttempts || a.ctx.Err() != nil {
			return attempts, err
		}
		wait := f.RetryAfter
		if wait <= 0 {
			wait = time.Duration(attempts) * time.Second
		}
		auditSleep(a.ctx, min(wait, auditMaxWait))
	}
}

// skippedError is a read not made because the query allowance was already
// spent; FailureOf-style classification reads it as that quota failure.
type skippedError struct{ f obsbackend.Failure }

func (e *skippedError) Error() string {
	return "not run: " + e.f.Message
}

func (a *dashAudit) failureOf(err error) obsbackend.Failure {
	if s, ok := err.(*skippedError); ok {
		f := s.f
		f.Message = s.Error()
		return f
	}
	return obsbackend.FailureOf(err)
}

func (a *dashAudit) panel(p dashspec.Panel) auditPanel {
	ap := auditPanel{
		ID: p.ID, Title: p.Title, Kind: string(p.Kind), Signal: string(p.Signal), From: a.rng.From, To: a.rng.To,
	}
	for _, s := range p.Series {
		if p.Signal == dashspec.Metrics && s.Field != "" && !slices.Contains(ap.Metrics, s.Field) {
			ap.Metrics = append(ap.Metrics, s.Field)
		}
	}
	if p.Kind == dashspec.KindSearch {
		ap.Status, ap.Reason = auditNotApplicable, "search_panel"
		ap.ProbableCause = "a search panel is a link into the logs or traces explorer; the console runs no query for it"
		return ap
	}
	op := panelOf(p)
	var ro obsbackend.PanelReadout
	attempts, err := a.guarded(func() error {
		var err error
		ro, err = a.dr.QueryPanelReadout(a.ctx, obsbackend.PanelReadoutRequest{Narrow: obsbackend.Narrow{Range: a.rng}, Panel: op, ProjectID: a.project})
		return err
	})
	ap.Attempts = attempts
	if err != nil {
		f := a.failureOf(err)
		ap.Status, ap.Reason, ap.Error = auditError, string(f.Class), auditFailureOf(f)
		ap.ProbableCause = failureCause(f, a.rng)
		return ap
	}
	ap.fill(ro)
	a.judge(&ap, op, ro)
	return ap
}

func (ap *auditPanel) fill(ro obsbackend.PanelReadout) {
	ap.Series, ap.Rows, ap.Points, ap.NonNull, ap.NonZero = ro.Series, ro.Rows, ro.Points, ro.NonNull, ro.NonZero
	ap.Buckets, ap.Items, ap.LastDataAt, ap.StepSeconds = ro.Buckets, ro.Items, ro.LastDataAt, ro.StepSeconds
	ap.Truncated, ap.SeriesTotal, ap.Warnings = ro.Truncated, ro.SeriesTotal, ro.Warnings
	if len(ro.SQLColumns) > 0 {
		ap.firstColumnType = ro.SQLColumns[0].Type
	}
}

// judge sets the status of a panel whose read succeeded, probing it when it is
// empty.
func (a *dashAudit) judge(ap *auditPanel, op obsquery.Panel, ro obsbackend.PanelReadout) {
	window := a.rng.To.Sub(a.rng.From)
	absent := warningDetails(ro.Warnings, "metric_absent")
	mismatch := append(warningDetails(ro.Warnings, "metric_kind_mismatch"), warningDetails(ro.Warnings, "metric_stored_as_n_kinds")...)
	switch {
	case !ro.HasData:
		ap.Status = auditEmpty
		if len(absent) > 0 {
			ap.Reason = "metric_absent"
		} else {
			ap.Reason = "empty"
		}
		if a.opts.noProbe {
			ap.ProbableCause = emptyCause(ap, nil, window, a.opts.probeSince)
			return
		}
		ap.Probe = a.probe(ap, op)
		ap.Reason, ap.ProbableCause = ap.Probe.Verdict, emptyCause(ap, ap.Probe, window, a.opts.probeSince)
		if ap.Probe.Verdict == verdictProbeFailed || ap.Probe.Verdict == verdictNotProbed {
			ap.Reason = "empty"
		}
	case len(absent) > 0:
		ap.Status, ap.Reason = auditPartial, "metric_absent_some_series"
		ap.ProbableCause = fmt.Sprintf("some series have no data in this window (%s); the control plane found no data for those metrics in the env over %s", strings.Join(absent, ", "), durText(window))
	case len(mismatch) > 0:
		ap.Status, ap.Reason = auditPartial, "metric_kind_mismatch"
		ap.ProbableCause = "a metric is stored as a different kind than the panel declares (" + strings.Join(mismatch, "; ") + "); the panel reads the wrong table or refuses its aggregation"
	case singleBucket(ap, ro):
		ap.Status, ap.Reason = auditPartial, "single_bucket"
		ap.ProbableCause = singleBucketCause(ap, window)
	case ro.NonZero == 0 && (ro.Points > 0 || ro.Rows > 0 || ro.Series > 0):
		ap.Status = auditOK
		ap.Note = "all_zero: every value in the window is zero (a counter that did not move, or a rate with no failures)"
	default:
		ap.Status = auditOK
	}
}

// singleBucket: a time-series panel whose data is one bucket cannot draw a
// line, and the bucket is usually a coarser grain than the window.
func singleBucket(ap *auditPanel, ro obsbackend.PanelReadout) bool {
	timeSeries := ap.Kind == "line" || ap.Kind == "heatmap" || (ap.Kind == "sql" && ro.Series > 0)
	return timeSeries && ro.Buckets == 1
}

func singleBucketCause(ap *auditPanel, window time.Duration) string {
	if ap.Kind == "sql" && strings.HasPrefix(ap.firstColumnType, "Date") && !strings.HasPrefix(ap.firstColumnType, "DateTime") {
		return fmt.Sprintf("the panel buckets by calendar day (its first column is %s) but the window is %s, so it can hold one bucket, which a line cannot draw and the console places at UTC midnight; widen the window, or bucket by the {step} parameter", ap.firstColumnType, durText(window))
	}
	return fmt.Sprintf("the answer holds one bucket (step %s over %s), and a line needs two to draw; widen the window or lower the step", stepText(ap.StepSeconds), durText(window))
}

// warningDetails returns the detail of every warning with the given code
// ("metric_absent: rpc.server.calls" -> "rpc.server.calls").
func warningDetails(warnings []string, code string) []string {
	var out []string
	for _, w := range warnings {
		c, detail, _ := strings.Cut(w, ": ")
		if c == code {
			if detail == "" {
				detail = code
			}
			out = append(out, detail)
		}
	}
	return out
}

// ── the probe ───────────────────────────────────────────────────────────

func (a *dashAudit) probe(ap *auditPanel, op obsquery.Panel) *auditProbe {
	to := a.rng.To
	from := to.Add(-a.opts.probeSince)
	if mr, err := op.MaxRange(); err == nil && to.Sub(from) > mr {
		from = to.Add(-mr)
	}
	pr := &auditProbe{From: from, To: to, Steps: []auditProbeStep{}}
	if !from.Before(a.rng.From) {
		// Nothing wider than the window the panel already ran over.
		pr.Verdict = verdictNotProbed
		pr.Steps = append(pr.Steps, auditProbeStep{Name: "wider_window", Error: &auditFailure{Class: "skipped", Message: "the probe window is not wider than the audit window (or than the signal allows); pass --probe-window larger than --since"}})
		return pr
	}
	wide := obsbackend.Narrow{Range: obsbackend.Range{From: from, To: to}}
	step := func(name string, req obsbackend.PanelReadoutRequest) auditProbeStep {
		var ro obsbackend.PanelReadout
		_, err := a.guarded(func() error {
			var err error
			ro, err = a.dr.QueryPanelReadout(a.ctx, req)
			return err
		})
		if err != nil {
			return auditProbeStep{Name: name, Error: auditFailureOf(a.failureOf(err))}
		}
		return auditProbeStep{Name: name, HasData: ro.HasData && (ro.NonZero > 0 || ro.Items > 0), NonZero: ro.NonZero, LastDataAt: ro.LastDataAt}
	}

	var catalogMissing bool
	if len(ap.Metrics) > 0 {
		for _, m := range ap.Metrics {
			c := a.metricCatalog(m, wide, op)
			pr.Catalog = append(pr.Catalog, c)
			if c.Error == "" && !c.Seen {
				catalogMissing = true
			}
		}
	}

	w := step("wider_window", obsbackend.PanelReadoutRequest{Narrow: wide, Panel: op, ProjectID: a.project})
	pr.Steps = append(pr.Steps, w)
	switch {
	case w.Error != nil:
		pr.Verdict = verdictProbeFailed
		return pr
	case w.HasData:
		pr.Verdict = verdictOutsideWindow
		return pr
	}
	var other, filtered auditProbeStep
	if a.project != "" {
		other = step("all_envs", obsbackend.PanelReadoutRequest{Narrow: wide, Panel: op, AllEnvs: true, ProjectID: a.project})
		pr.Steps = append(pr.Steps, other)
	}
	if bare, changed := withoutFilters(op); changed {
		filtered = step("without_filters", obsbackend.PanelReadoutRequest{Narrow: wide, Panel: bare, ProjectID: a.project})
		pr.Steps = append(pr.Steps, filtered)
	}
	switch {
	case other.HasData:
		pr.Verdict = verdictOtherEnv
	case filtered.HasData:
		pr.Verdict = verdictExcludedFilter
	case other.Error != nil || filtered.Error != nil:
		// A follow-up that could not be read cannot rule its scope out.
		pr.Verdict = verdictProbeFailed
	case catalogMissing:
		pr.Verdict = verdictMetricNever
	default:
		pr.Verdict = verdictNoneAnywhere
	}
	return pr
}

// withoutFilters is op with every panel and series predicate removed, and
// whether there was anything to remove.
func withoutFilters(op obsquery.Panel) (obsquery.Panel, bool) {
	changed := len(op.Where) > 0
	bare := op
	bare.Where = nil
	bare.Series = make([]obsquery.PanelSeries, len(op.Series))
	for i, s := range op.Series {
		if len(s.Where) > 0 {
			changed = true
		}
		s.Where = nil
		bare.Series[i] = s
	}
	return bare, changed
}

// metricCatalog looks one metric up in the env's catalog over the probe window.
// One lookup per metric per run.
func (a *dashAudit) metricCatalog(name string, wide obsbackend.Narrow, op obsquery.Panel) auditCatalog {
	a.mu.Lock()
	if c, ok := a.catalog[name]; ok {
		a.mu.Unlock()
		return *c
	}
	a.mu.Unlock()
	c := auditCatalog{Metric: name}
	for _, s := range op.Series {
		if s.Field == name && s.MetricType != "" {
			c.DeclaredType = s.MetricType
		}
	}
	var res obsbackend.MetricsListResult
	_, err := a.guarded(func() error {
		var err error
		res, err = a.b.MetricsList(a.ctx, obsbackend.MetricsListRequest{Narrow: wide, NameContains: name, Limit: obsquery.ListMetricsMax})
		return err
	})
	if err != nil {
		c.Error = string(a.failureOf(err).Class) + ": " + a.failureOf(err).Message
	}
	for _, m := range res.Metrics {
		if m.Name == name && m.Seen {
			c.Seen, c.Type, c.LastSeen, c.Services = true, m.Type, m.LastSeen, m.Services
			break
		}
	}
	a.mu.Lock()
	a.catalog[name] = &c
	a.mu.Unlock()
	return c
}

// ── probable cause ──────────────────────────────────────────────────────

func failureCause(f obsbackend.Failure, rng obsbackend.Range) string {
	switch f.Class {
	case obsbackend.ClassAuth:
		need := "obs:read"
		if f.MissingScope != "" {
			need = f.MissingScope
		}
		return fmt.Sprintf("the control plane refused the credential (HTTP %d; needs %s): an auth/scope problem, not evidence the data is missing; ask an org admin for the scope", f.HTTPStatus, need)
	case obsbackend.ClassQuota:
		when := ""
		if !f.ResetsAt.IsZero() {
			when = " until " + f.ResetsAt.Format(time.RFC3339)
		}
		return "the org's hourly telemetry query allowance is spent" + when + ": reads are paused, which says nothing about the data; retry after it refills"
	case obsbackend.ClassRateLimited:
		return "this credential's own request budget is spent (retried; still refused): wait a minute and audit fewer panels at a time (--dashboard)"
	case obsbackend.ClassInFlight:
		return "too many telemetry reads were in flight for this caller (retried; still refused): another session is reading at once; run it again shortly"
	case obsbackend.ClassTimeout:
		return fmt.Sprintf("the query timed out over %s: the window is too wide for this panel; narrow --since", durText(rng.To.Sub(rng.From)))
	case obsbackend.ClassTooMuchData:
		return "the query touched more rows or bytes than a read may: narrow --since or add a filter"
	case obsbackend.ClassStorage:
		return "the telemetry store is unavailable or not set up for this organization: a storage fault, not missing data"
	case obsbackend.ClassInvalid:
		return "the control plane refused the panel or the window as invalid (" + f.Message + "): a spec or range problem; a window over the signal's maximum is refused, not clamped"
	case obsbackend.ClassNotFound:
		return "the control plane does not know the thing asked for (" + f.Message + ")"
	case obsbackend.ClassUnsupported:
		return "this control plane is older than the call and does not serve it"
	case obsbackend.ClassUnreachable:
		return "no answer came back from the control plane: a network or availability problem"
	case obsbackend.ClassServer:
		return "the control plane failed internally (" + f.Message + ")"
	}
	return "the read failed (" + f.Message + ")"
}

func emptyCause(ap *auditPanel, pr *auditProbe, window, probeWindow time.Duration) string {
	if pr == nil {
		if len(ap.Metrics) > 0 {
			return "nothing came back and the control plane reports the metric(s) absent in this window; re-run without --no-probe to see whether they exist outside it"
		}
		return "nothing came back in this window; re-run without --no-probe to see whether the data exists outside it"
	}
	span := durText(pr.To.Sub(pr.From))
	switch pr.Verdict {
	case verdictOutsideWindow:
		last := ""
		for _, s := range pr.Steps {
			if s.Name == "wider_window" && !s.LastDataAt.IsZero() {
				last = ", newest " + s.LastDataAt.Format(time.RFC3339)
			}
		}
		return fmt.Sprintf("the data exists: the panel has points over the last %s%s, but none in the last %s; the window is shorter than the signal's cadence, or the emitter went quiet", span, last, durText(window))
	case verdictOtherEnv:
		return fmt.Sprintf("the data exists in the project's other environments over %s but not in this env: the emitter tags another env, or no env id at all (an env-scoped panel drops untagged rows)", span)
	case verdictExcludedFilter:
		return fmt.Sprintf("the data exists over %s without the panel's filters (where predicates): the filter value matches nothing the emitter writes", span)
	case verdictMetricNever:
		return fmt.Sprintf("no data in any env over %s, and the metric catalog has no such metric (%s): a misspelt name, a metric the collector drops, or an emitter that has not shipped", span, strings.Join(missingMetrics(pr), ", "))
	case verdictNoneAnywhere:
		return fmt.Sprintf("no data in any env over %s, and the panel's own filters are not the cause: the emitter is not shipping this signal, the name it reads is wrong, or its rows carry no env id (an env-scoped read cannot see those; query the table directly with `forge obs query`). A metric that only exists once something fails reads this way too", span)
	case verdictNotProbed:
		return "nothing came back, and the probe could not look wider: its window is not wider than the audit window or than the signal allows; raise --probe-window above --since"
	case verdictProbeFailed:
		for _, s := range pr.Steps {
			if s.Error != nil {
				return fmt.Sprintf("nothing came back, and the follow-up read failed (%s: %s), so whether the data exists is not known", s.Error.Class, s.Error.Message)
			}
		}
	}
	return "nothing came back"
}

func missingMetrics(pr *auditProbe) []string {
	var out []string
	for _, c := range pr.Catalog {
		if !c.Seen && c.Error == "" {
			out = append(out, c.Metric)
		}
	}
	return out
}

// ── document ────────────────────────────────────────────────────────────

var statusRank = map[string]int{auditError: 0, auditEmpty: 1, auditPartial: 2, auditOK: 3, auditNotApplicable: 4}

func rollup(panels []auditPanel) string {
	worst := auditNotApplicable
	for _, p := range panels {
		if statusRank[p.Status] < statusRank[worst] {
			worst = p.Status
		}
	}
	return worst
}

func (d *obsDashboardsAuditDoc) panelCount() int {
	n := 0
	for _, db := range d.Dashboard {
		n += len(db.Panels)
	}
	return n
}

// sortFailuresFirst orders dashboards, then the panels in each, worst first.
// Ties keep the dashboard's own order, so the document is deterministic.
func (d *obsDashboardsAuditDoc) sortFailuresFirst() {
	for i := range d.Dashboard {
		ps := d.Dashboard[i].Panels
		sort.SliceStable(ps, func(a, b int) bool { return statusRank[ps[a].Status] < statusRank[ps[b].Status] })
	}
	sort.SliceStable(d.Dashboard, func(a, b int) bool {
		ra, rb := statusRank[d.Dashboard[a].Status], statusRank[d.Dashboard[b].Status]
		if ra != rb {
			return ra < rb
		}
		return d.Dashboard[a].ID < d.Dashboard[b].ID
	})
}

func (d *obsDashboardsAuditDoc) summarize() {
	s := &d.Summary
	*s = auditSummary{Dashboards: len(d.Dashboard)}
	for _, db := range d.Dashboard {
		if db.Error != nil {
			s.DashboardsUnread++
		}
		for _, p := range db.Panels {
			s.Panels++
			switch p.Status {
			case auditOK:
				s.OK++
			case auditPartial:
				s.Partial++
			case auditEmpty:
				s.Empty++
			case auditError:
				s.Error++
			case auditNotApplicable:
				s.NotApplicable++
			}
			if strings.HasPrefix(p.Note, "all_zero") {
				s.AllZero++
			}
		}
	}
}

// verdict is the error the verb returns: its exit status is the document's.
func (d *obsDashboardsAuditDoc) verdict(env string) error {
	s := d.Summary
	switch {
	case s.Error > 0 || s.DashboardsUnread > 0:
		return obsbackend.Wrong("%d panel(s) and %d dashboard(s) of %s could not be read; %d more are empty or partial (see each panel's error)", s.Error, s.DashboardsUnread, env, s.Empty+s.Partial).
			With(auditSuggestions(env, *d)...)
	case s.Empty+s.Partial > 0:
		return obsbackend.Undetermined("%d of %d panel(s) of %s are empty and %d partial (see probable_cause)", s.Empty, s.Panels, env, s.Partial)
	}
	return nil
}

func auditSuggestions(env string, d obsDashboardsAuditDoc) []string {
	var out []string
	for _, db := range d.Dashboard {
		for _, p := range db.Panels {
			if p.Status != auditEmpty && p.Status != auditPartial && p.Status != auditError {
				continue
			}
			if p.Status == auditError && p.Error != nil && p.Error.Class == string(obsbackend.ClassAuth) {
				continue
			}
			if len(p.Metrics) > 0 {
				out = append(out, obsout.Command("forge", "obs", "metrics", "describe", env, p.Metrics[0], "--since", "168h"))
			}
			if p.Probe != nil && p.Probe.Verdict == verdictOutsideWindow {
				out = append(out, obsout.Command("forge", "obs", "dashboards", "audit", env, "--dashboard", db.ID, "--panel", p.ID, "--since", "168h"))
			}
		}
	}
	return obsout.Suggest(out...)
}

// ── text ────────────────────────────────────────────────────────────────

func writeAuditText(w io.Writer, env string, doc obsDashboardsAuditDoc, all bool) {
	s := doc.Summary
	fmt.Fprintf(w, "%s dashboards audit  window %s (%s .. %s)", env, doc.Window.Since, doc.Window.From.Format(time.RFC3339), doc.Window.To.Format(time.RFC3339))
	if doc.Window.ProbeSince != "" {
		fmt.Fprintf(w, "  probe %s", doc.Window.ProbeSince)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d dashboard(s), %d panel(s): %d error, %d empty, %d partial, %d ok, %d n/a", s.Dashboards, s.Panels, s.Error, s.Empty, s.Partial, s.OK, s.NotApplicable)
	if s.AllZero > 0 {
		fmt.Fprintf(w, " (%d ok panel(s) are all zero)", s.AllZero)
	}
	fmt.Fprintln(w)

	var rows [][]string
	var why []string
	hidden := 0
	for _, db := range doc.Dashboard {
		if db.Error != nil {
			rows = append(rows, []string{auditError, db.ID + " (dashboard)", "-", "-", "-", db.Error.Class})
			why = append(why, fmt.Sprintf("%s (dashboard): %s", db.ID, db.Error.Message))
		}
		for _, p := range db.Panels {
			if (p.Status == auditOK || p.Status == auditNotApplicable) && !all {
				hidden++
				continue
			}
			name := db.ID + "/" + p.ID
			rows = append(rows, []string{p.Status, name, p.Kind, dataText(p), stepText(p.StepSeconds), p.Reason})
			if c := causeText(p); c != "" {
				why = append(why, name+": "+c)
			}
		}
	}
	if len(rows) > 0 {
		fmt.Fprintln(w)
		obsout.Table(w, []string{"STATUS", "PANEL", "KIND", "DATA", "STEP", "REASON"}, rows, 60)
	}
	if len(why) > 0 {
		fmt.Fprintln(w, "\nwhy:")
		for _, l := range why {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	if hidden > 0 {
		fmt.Fprintf(w, "\n%d ok or n/a panel(s) not listed (--all lists them)\n", hidden)
	}
	for _, n := range doc.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	for _, c := range doc.Suggested {
		fmt.Fprintf(w, "suggested: %s\n", c)
	}
}

func dataText(p auditPanel) string {
	if p.Status == auditError || p.Status == auditNotApplicable {
		return "-"
	}
	switch {
	case p.Kind == "funnel" || p.Kind == "issues" || p.Kind == "sessions" || p.Kind == "heatmap":
		return fmt.Sprintf("%d item(s)", p.Items)
	case p.Points > 0:
		return fmt.Sprintf("%d series, %d/%d pts non-zero", p.Series, p.NonZero, p.Points)
	}
	return fmt.Sprintf("%d series, %d row(s)", p.Series, p.Rows)
}

func causeText(p auditPanel) string {
	switch {
	case p.ProbableCause != "":
		return p.ProbableCause
	case p.Note != "":
		return p.Note
	}
	return ""
}

func stepText(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	return durText(time.Duration(sec) * time.Second)
}

// durText spells a duration the way the console's range presets do (15m, 1h, 7d).
func durText(d time.Duration) string {
	switch {
	case d > 0 && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d > 0 && d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
