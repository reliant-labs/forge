package releasehealth

// AI FINDINGS (llm-agent-observability §8, slice 7, decision 9). A release
// that makes an agent's model calls fail, slow down, or one of its tools
// break is judged from the GenAI spans its services emit (pkg/observe/genai,
// or any SDK's spans normalised onto the same names at ingest), spans only:
// no content, no scores.
//
// The rule is the service rule made per AI call, with its own thresholds
// because model and tool calls are a thinner sample than requests:
//
//   - A service is compared only with ai_min_calls (default 50) model and
//     tool calls in BOTH windows; a tool only with ai_min_calls calls of it.
//     A thinner one is not a finding: it is named in one not_evaluated entry
//     ([NotEvaluatedAI]).
//   - ai_error_rate: the share of the service's model and tool calls that
//     failed rose by more than ai_max_error_rate_increase (default 0.05,
//     five percentage points), with at least ai_min_errors (default 5)
//     failed calls after the promotion. A cancelled call (error.type
//     canceled) is no failure.
//   - ai_latency: the p95 of the service's model calls grew by more than
//     ai_max_p95_ratio (default 1.5, as latency's), with a before p95 above 0.
//   - ai_tool_errors: one tool's error rate rose by more than
//     ai_tool_max_error_rate_increase (default 0.1: one tool's window is
//     thinner still), with at least ai_min_errors failed calls of it after
//     the promotion. The finding names the tool.
//
// All three warn by default (release.DefaultReleaseHealth's fail_on names
// none): nobody's gate turns red on the release that ships them. An env that
// trusts them lists them in fail_on. They never prove a release good.
//
// Like the changed ops, the figures are version-scoped: when the subject
// names a release, a service is judged only on after-window calls of that
// release's own service.version ([AIHealth.AfterVersion]), so an old version
// still in rotation is not the release's evidence. A subject with no release
// (a local env) judges every call.
//
// No release shape carries the AI thresholds: they are the defaults until a
// request overrides one (--threshold ai_min_calls=20), as the changed-op
// thresholds are, and `forge obs check` applies them to either backend's
// answer with [WithAI].

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/pkg/release"
)

// Defaults of the AI thresholds (above).
const (
	DefaultAIMinCalls                 = 50
	DefaultAIMaxErrorRateIncrease     = 0.05
	DefaultAIMinErrors                = 5
	DefaultAIMaxP95Ratio              = 1.5
	DefaultAIToolMaxErrorRateIncrease = 0.1
)

// The AI threshold names.
const (
	ThresholdAIMinCalls                 = "ai_min_calls"
	ThresholdAIMaxErrorRateIncrease     = "ai_max_error_rate_increase"
	ThresholdAIMinErrors                = "ai_min_errors"
	ThresholdAIMaxP95Ratio              = "ai_max_p95_ratio"
	ThresholdAIToolMaxErrorRateIncrease = "ai_tool_max_error_rate_increase"
)

// AIThresholds are the AI threshold names. No backend's judge knows them
// (Resolve refuses them): a caller keeps them back from the backend and
// applies them with [WithAI].
var AIThresholds = []string{ThresholdAIMinCalls, ThresholdAIMaxErrorRateIncrease, ThresholdAIMinErrors,
	ThresholdAIMaxP95Ratio, ThresholdAIToolMaxErrorRateIncrease}

// NotEvaluatedAI prefixes the not_evaluated entries that name the services
// and tools whose AI calls were not judged.
const NotEvaluatedAI = "ai:"

// AICalls are one window's model or tool calls.
type AICalls struct {
	// Calls is how many there were; Errors how many failed (a cancellation
	// is not a failure).
	Calls  int64 `json:"calls"`
	Errors int64 `json:"errors"`
}

// ErrorRate is Errors over Calls, 0 with no calls.
func (c AICalls) ErrorRate() float64 {
	if c.Calls <= 0 {
		return 0
	}
	return float64(c.Errors) / float64(c.Calls)
}

// AIWindow is one service's AI calls over one window.
type AIWindow struct {
	// Model is its model calls (chat, text_completion, generate_content,
	// embeddings); Tool its tool calls (execute_tool).
	Model AICalls `json:"model"`
	Tool  AICalls `json:"tool"`
	// ModelP95Ms is the p95 duration of its model calls, 0 with none.
	ModelP95Ms float64 `json:"model_p95_ms"`
}

// All is the window's model and tool calls together: what ai_error_rate
// judges.
func (w AIWindow) All() AICalls {
	return AICalls{Calls: w.Model.Calls + w.Tool.Calls, Errors: w.Model.Errors + w.Tool.Errors}
}

// AITool is one tool's calls in both windows.
type AITool struct {
	Tool   string  `json:"tool"`
	Before AICalls `json:"before"`
	After  AICalls `json:"after"`
}

// AIHealth is one service's GenAI calls before and after the promotion.
type AIHealth struct {
	Service string   `json:"service"`
	Before  AIWindow `json:"before"`
	After   AIWindow `json:"after"`
	// Tools are its tools' calls, by tool name.
	Tools []AITool `json:"tools,omitempty"`
	// AfterVersion is the service.version the After figures are restricted
	// to; "" is every version's calls. When the report's subject names a
	// release, a service is judged only when AfterVersion is that release.
	AfterVersion string `json:"after_version,omitempty"`
}

// aiLimits are the AI thresholds, each filled in.
type aiLimits struct {
	minCalls        int
	maxIncrease     float64
	minErrors       int
	maxRatio        float64
	toolMaxIncrease float64
}

// aiFindings judges hs under l for the judged subject s: the findings (no
// policy yet), and the not_evaluated entries of the services with no
// after-window figures of s's release and of the services and tools too thin
// to compare.
func aiFindings(hs []AIHealth, l aiLimits, s Subject) ([]Finding, []string) {
	rel := s.Release
	floor := int64(l.minCalls)
	var out []Finding
	var noAfter, thin, thinTools []string
	for _, h := range hs {
		if rel != "" && h.AfterVersion != rel {
			noAfter = append(noAfter, h.Service)
			continue
		}
		label := h.Service
		before, after := h.Before.All(), h.After.All()
		if before.Calls < floor || after.Calls < floor {
			thin = append(thin, label)
		} else {
			if inc := after.ErrorRate() - before.ErrorRate(); inc > l.maxIncrease && after.Errors >= int64(l.minErrors) {
				out = append(out, Finding{Code: release.HealthAIErrorRate, Service: h.Service, Ref: h.Service, Value: inc, Threshold: l.maxIncrease,
					Message: fmt.Sprintf("%s: AI call error rate %s -> %s (+%s, limit +%s; %d of %d model and tool calls failed)",
						label, pct(before.ErrorRate()), pct(after.ErrorRate()), pct(inc), pct(l.maxIncrease), after.Errors, after.Calls)})
			}
			if h.Before.Model.Calls >= floor && h.After.Model.Calls >= floor && h.Before.ModelP95Ms > 0 {
				if ratio := h.After.ModelP95Ms / h.Before.ModelP95Ms; ratio > l.maxRatio {
					out = append(out, Finding{Code: release.HealthAILatency, Service: h.Service, Ref: h.Service, Value: ratio, Threshold: l.maxRatio,
						Message: fmt.Sprintf("%s: model call p95 %s -> %s (%.2fx, limit %.2gx)", label, ms(h.Before.ModelP95Ms), ms(h.After.ModelP95Ms), ratio, l.maxRatio)})
				}
			}
		}
		for _, t := range h.Tools {
			if t.Before.Calls < floor || t.After.Calls < floor {
				thinTools = append(thinTools, t.Tool)
				continue
			}
			if inc := t.After.ErrorRate() - t.Before.ErrorRate(); inc > l.toolMaxIncrease && t.After.Errors >= int64(l.minErrors) {
				out = append(out, Finding{Code: release.HealthAIToolErrors, Service: h.Service, Ref: t.Tool, Value: inc, Threshold: l.toolMaxIncrease,
					Message: fmt.Sprintf("tool %s (%s): error rate %s -> %s (+%s, limit +%s; %d errors in %d calls)",
						t.Tool, h.Service, pct(t.Before.ErrorRate()), pct(t.After.ErrorRate()), pct(inc), pct(l.toolMaxIncrease), t.After.Errors, t.After.Calls)})
			}
		}
	}
	var entries []string
	if len(noAfter) > 0 {
		names, list := fewNames(noAfter)
		entries = append(entries, fmt.Sprintf("%s %d service(s) have no after-window AI calls of %s (their spans carry no service.version): %s",
			NotEvaluatedAI, len(names), rel, list))
	}
	if len(thin) > 0 {
		names, list := fewNames(thin)
		entries = append(entries, fmt.Sprintf("%s %d service(s) below ai_min_calls (%d model and tool calls in each window): %s",
			NotEvaluatedAI, len(names), l.minCalls, list))
	}
	if len(thinTools) > 0 {
		names, list := fewNames(thinTools)
		entries = append(entries, fmt.Sprintf("%s %d tool(s) below ai_min_calls (%d calls in each window): %s",
			NotEvaluatedAI, len(names), l.minCalls, list))
	}
	return out, entries
}

// WithAI judges the services' GenAI calls against a judged report: how the
// AI rule reaches either backend's answer (`forge obs check` reads the
// figures itself, from the same panels on both). It drops the report's AI
// findings and its AI not_evaluated entries and re-derives both from hs, so
// it is idempotent; nil hs re-judges the report's own AI. The thresholds are
// the report's AI rows, an override (the request's --threshold pairs, by
// name; nil for none) winning, else the defaults; every AI row is listed.
// Each finding's policy follows the report's fail_on, it is attributed as
// [Attribute] attributes one, and the verdict is recomputed as [Rejudge]
// recomputes it, so a fail_on that names an AI code can fail the release.
// A report service absent from hs has no AI evidence in either window and
// is named in not_evaluated, preventing a healthy report from passing.
func WithAI(r Report, hs []AIHealth, overrides map[string]string) Report {
	if hs == nil {
		hs = r.AI
	}
	ths, l := aiThresholdsOf(r.Thresholds, overrides)
	out := r
	out.Thresholds = ths
	out.AI = hs
	isAI := func(f Finding) bool { return slices.Contains(release.AIFindings, f.Code) }
	out.Findings = slices.DeleteFunc(slices.Clone(r.Findings), isAI)
	if out.Findings == nil {
		out.Findings = []Finding{}
	}
	out.NotEvaluated = slices.DeleteFunc(slices.Clone(r.NotEvaluated), func(e string) bool { return strings.HasPrefix(e, NotEvaluatedAI) })

	fs, entries := aiFindings(hs, l, r.Subject)
	// The panels only return services with observations. Compare that set
	// with the report's services so empty panels (or an omitted service)
	// cannot silently count as complete AI evidence.
	var missing []string
	for _, service := range r.Services {
		if !slices.ContainsFunc(hs, func(h AIHealth) bool { return h.Service == service.Service }) {
			missing = append(missing, service.Service)
		}
	}
	if len(missing) > 0 {
		names, list := fewNames(missing)
		entries = append(entries, fmt.Sprintf("%s %d service(s) have no AI calls in either window: %s", NotEvaluatedAI, len(names), list))
	}
	failOn := failOnOf(ths)
	for _, f := range fs {
		f.Policy = policyOf(f.Code, failOn)
		out.Findings = append(out.Findings, f)
		out.attribute(len(out.Findings) - 1)
	}
	out.NotEvaluated = withNotEvaluated(out.NotEvaluated, entries)
	if len(out.NotEvaluated) == 0 {
		out.NotEvaluated = nil
	}
	sortFindings(out.Findings)
	if slices.Equal(out.Findings, r.Findings) && slices.Equal(out.NotEvaluated, r.NotEvaluated) && out.RolloutIncomplete == r.RolloutIncomplete {
		return out // nothing the verdict rests on changed: the verdict as judged
	}
	out.Verdict = verdictOf(out.Findings, out.AfterComplete, out.Services, gaugeOf(out.Thresholds), out.unproven())
	return out
}

// aiThresholdsOf reads the AI thresholds from a judged report's list, each
// valid override winning (tagged SourceRequest), and returns the list with
// every AI row present, each one the report lacks added before fail_on.
func aiThresholdsOf(ths []Threshold, overrides map[string]string) ([]Threshold, aiLimits) {
	l := aiLimits{}
	out := slices.Clone(ths)
	value := func(name, def string) {
		v, src := def, SourceDefault
		i := slices.IndexFunc(out, func(t Threshold) bool { return t.Name == name })
		if i >= 0 && ValidAIThreshold(name, out[i].Value) {
			v, src = out[i].Value, out[i].Source
		}
		if o, ok := overrides[name]; ok && ValidAIThreshold(name, strings.TrimSpace(o)) {
			v, src = strings.TrimSpace(o), SourceRequest
		}
		th := Threshold{name, v, src}
		if i >= 0 {
			out[i] = th
		} else {
			at := slices.IndexFunc(out, func(t Threshold) bool { return t.Name == ThresholdFailOn })
			if at < 0 {
				at = len(out)
			}
			out = slices.Insert(out, at, th)
		}
		switch name {
		case ThresholdAIMinCalls:
			l.minCalls, _ = strconv.Atoi(v)
		case ThresholdAIMinErrors:
			l.minErrors, _ = strconv.Atoi(v)
		case ThresholdAIMaxErrorRateIncrease:
			l.maxIncrease, _ = strconv.ParseFloat(v, 64)
		case ThresholdAIMaxP95Ratio:
			l.maxRatio, _ = strconv.ParseFloat(v, 64)
		case ThresholdAIToolMaxErrorRateIncrease:
			l.toolMaxIncrease, _ = strconv.ParseFloat(v, 64)
		}
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	value(ThresholdAIMinCalls, strconv.Itoa(DefaultAIMinCalls))
	value(ThresholdAIMaxErrorRateIncrease, f(DefaultAIMaxErrorRateIncrease))
	value(ThresholdAIMinErrors, strconv.Itoa(DefaultAIMinErrors))
	value(ThresholdAIMaxP95Ratio, f(DefaultAIMaxP95Ratio))
	value(ThresholdAIToolMaxErrorRateIncrease, f(DefaultAIToolMaxErrorRateIncrease))
	return out, l
}

// ValidAIThreshold reports whether v is in range for AI threshold name:
// counts at least 1, error-rate increases a fraction in (0, 1], the ratio
// above 1.
func ValidAIThreshold(name, v string) bool {
	switch name {
	case ThresholdAIMinCalls, ThresholdAIMinErrors:
		n, err := strconv.Atoi(v)
		return err == nil && n >= 1
	case ThresholdAIMaxErrorRateIncrease, ThresholdAIToolMaxErrorRateIncrease:
		f, err := strconv.ParseFloat(v, 64)
		return err == nil && f > 0 && f <= 1
	case ThresholdAIMaxP95Ratio:
		f, err := strconv.ParseFloat(v, 64)
		return err == nil && f > 1
	}
	return false
}
