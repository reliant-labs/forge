package dashspec

// SpecVersion identifies the serialized shape of Spec. Bump it on a breaking
// change so a consumer can refuse a spec it does not understand.
const SpecVersion = "forge.dashboards/v1"

// GridColumns is the width of a dashboard's grid: a panel's Width is in
// columns out of it.
const GridColumns = 24

// Signal is the telemetry table a panel reads.
type Signal string

// The signals a panel can read.
const (
	Traces  Signal = "traces"
	Metrics Signal = "metrics"
	Logs    Signal = "logs"
)

// PanelKind is how a panel is drawn.
type PanelKind string

const (
	// KindNumber is one headline value over the selected time range.
	KindNumber PanelKind = "number"
	// KindLine is a time series; with GroupBy, one line per group.
	KindLine PanelKind = "line"
	// KindTable is one row per GroupBy value, one column per Series.
	KindTable PanelKind = "table"
	// KindSearch is a log or span search (no aggregate): the rows themselves.
	KindSearch PanelKind = "search"

	// The kinds below are drawn by the Reliant console from its own services,
	// not from a query over the panel's series, so they take no series,
	// columns or group-by. They cannot be drawn by HyperDX, whose renderer
	// skips them (see IsConsoleKind).

	// KindIssues is the grouped-errors list: new and regressed issues first,
	// then the most frequent. Served by RumService ListIssues.
	KindIssues PanelKind = "issues"
	// KindSessions is a list of browser sessions, with replay links. Served by
	// RumService ListSessions; HasError narrows it to sessions with an error.
	KindSessions PanelKind = "sessions"
	// KindFunnel is a product-event funnel over Panel.Funnel. Served from the
	// product_events table with ClickHouse windowFunnel.
	KindFunnel PanelKind = "funnel"
)

// IsConsoleKind reports whether k is drawn only by the Reliant console, so the
// HyperDX renderer has nothing to emit for it.
func IsConsoleKind(k PanelKind) bool {
	switch k {
	case KindIssues, KindSessions, KindFunnel:
		return true
	}
	return false
}

// FunnelBy is what a funnel counts: how events are tied into one journey.
type FunnelBy string

// The funnel journeys.
const (
	FunnelBySession FunnelBy = "session"
	FunnelByUser    FunnelBy = "user"
)

// Funnel is the definition a KindFunnel panel draws: the ordered event names
// a journey passes through, within Window, counted per session or per user.
type Funnel struct {
	// Steps are product event names (the names passed to track()), in order.
	Steps []string `json:"steps"`
	// Window is how long a journey may take from its first step to its last:
	// a number and a unit of s, m, h or d ("1h", "7d"). Empty is "1h".
	Window string `json:"window,omitempty"`
	// By is the journey's identity. Empty is FunnelBySession.
	By FunnelBy `json:"by,omitempty"`
}

// Funnel defaults, applied where a panel leaves a field empty.
const (
	DefaultFunnelWindow = "1h"
	DefaultFunnelBy     = FunnelBySession
)

// Funnel bounds.
const (
	minFunnelSteps = 2
	maxFunnelSteps = 10
)

// Thresholds colour a series' values the way Core Web Vitals do: at or below
// Good is good, above Poor is poor, between is "needs improvement". Renderers
// that cannot colour ignore it.
type Thresholds struct {
	Good float64 `json:"good"`
	Poor float64 `json:"poor"`
}

// Agg is an aggregate function.
type Agg string

// The aggregate functions a series can apply.
const (
	AggCount         Agg = "count"
	AggCountDistinct Agg = "count_distinct"
	AggSum           Agg = "sum"
	AggAvg           Agg = "avg"
	AggMax           Agg = "max"
	AggQuantile      Agg = "quantile"
	AggLast          Agg = "last"
)

// Unit is how a panel's values are formatted.
type Unit string

// The units a panel's values are formatted in.
const (
	UnitNone    Unit = ""
	UnitCount   Unit = "count"
	UnitMillis  Unit = "ms"
	UnitBytes   Unit = "bytes"
	UnitPercent Unit = "percent" // values are fractions in [0,1]
	UnitSeconds Unit = "s"
)

// Op is a predicate operator.
type Op string

// The predicate operators.
const (
	OpEq     Op = "eq"
	OpNeq    Op = "neq"
	OpIn     Op = "in"
	OpNotIn  Op = "not_in"
	OpPrefix Op = "prefix"
	OpMatch  Op = "match" // RE2
	OpExists Op = "exists"
	OpGTE    Op = "gte"
)

// Semantic field names. A field is one of:
//
//	service.name, deployment.environment.name        (every signal)
//	span.name, span.kind, span.status,
//	span.status_message, span.duration               (traces)
//	span.attr.<key>                                  (traces)
//	log.severity, log.body, log.attr.<key>           (logs)
//	metric.attr.<key>                                (metrics)
//	resource.attr.<key>                              (every signal)
//
// span.kind is "server" or "client"; span.status and log.severity compare
// against "error". span.duration is milliseconds.
const (
	FieldService = "service.name"
	FieldEnv     = "deployment.environment.name"
)

// Predicate is one condition. Values are literals; Field is a semantic field.
type Predicate struct {
	Field  string   `json:"field"`
	Op     Op       `json:"op"`
	Value  string   `json:"value,omitempty"`
	Values []string `json:"values,omitempty"`
}

// Series is one aggregate shown by a panel.
type Series struct {
	Label string `json:"label"`
	Agg   Agg    `json:"agg"`
	// Quantile is the level for AggQuantile, e.g. 0.95.
	Quantile float64 `json:"quantile,omitempty"`
	// Field is the aggregated value: a semantic field for traces and logs
	// (span.duration, span.attr.lcp), the metric NAME for metrics. Empty for
	// AggCount.
	Field string `json:"field,omitempty"`
	// MetricType is "sum" or "gauge" (metrics only).
	MetricType string `json:"metricType,omitempty"`
	// Where narrows this series alone (e.g. only the failed requests).
	Where []Predicate `json:"where,omitempty"`
	// Thresholds grade the series' values good / needs improvement / poor.
	Thresholds *Thresholds `json:"thresholds,omitempty"`
}

// Panel is one tile.
type Panel struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Kind        PanelKind `json:"kind"`
	Signal      Signal    `json:"signal"`
	Series      []Series  `json:"series,omitempty"`
	// Ratio makes a two-series line panel plot Series[0]/Series[1]
	// (an error rate is failed requests over all requests).
	Ratio bool `json:"ratio,omitempty"`
	// Where narrows every series of the panel.
	Where   []Predicate `json:"where,omitempty"`
	GroupBy []string    `json:"groupBy,omitempty"`
	// Columns are the fields a KindSearch panel shows.
	Columns []string `json:"columns,omitempty"`
	// Funnel is what a KindFunnel panel draws; only that kind takes it.
	Funnel *Funnel `json:"funnel,omitempty"`
	// HasError narrows a KindSessions panel to sessions that hit an error;
	// only that kind takes it.
	HasError bool `json:"hasError,omitempty"`
	Unit     Unit `json:"unit,omitempty"`
	// Width is in grid columns out of 24; Height in rows. Zero takes the
	// default for the kind.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// Filter is a dashboard-level dropdown that narrows every panel.
type Filter struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Field string `json:"field"`
	// Signal is the telemetry the dropdown reads its values from. Empty is
	// Traces, which every dashboard but the metrics ones draws.
	Signal Signal `json:"signal,omitempty"`
}

// AnnotationSource names where a dashboard's time-series markers come from.
type AnnotationSource string

// The annotation sources. The spec only DECLARES a source; the renderer draws
// it from data it owns (the Reliant console reads the release ledger).
const (
	// AnnotationReleases marks every promotion of a release into the
	// environment on the dashboard's time-series panels.
	AnnotationReleases AnnotationSource = "releases"
)

// Annotation is a marker series drawn over every KindLine panel of a dashboard.
// HyperDX has no such concept, so RenderHyperDX ignores it.
type Annotation struct {
	Source AnnotationSource `json:"source"`
	Label  string           `json:"label,omitempty"`
}

// Dashboard is an ordered list of panels under a shared set of filters.
type Dashboard struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	Filters     []Filter     `json:"filters,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
	Panels      []Panel      `json:"panels"`
}
