package releasehealth

// ALERTS ON AN ISSUE THAT PREDATES THE RELEASE. An alert that names an issue
// (label issue_id) and is not one of the platform's NewIssue or
// IssueRegressed alerts (issue_alerts.go) — IssueSpike, or an env's own rule
// over an issue — is about that issue's condition, and the condition can be
// older than the alert instance. control-plane v1.23.0-rc.9 (2026-11-19)
// failed prod on one alert_firing finding: IssueSpike for an issue first seen
// at 08:43 in v1.22.0-rc.2, which fired ~28 times an hour all morning, 3.5h
// before the promotion, and kept the same rate under rc.9. The IssueSpike
// instance first fired at 12:15, after the noon promotion, because the
// spike detector compares the last one to two hours with the 24h before
// them, and that ratio crossed its factor then; it had never fired before,
// so the baseline-alert rule (alert_baseline.go) had nothing to discount.
//
// So such an alert counts against the release only when its condition is
// new. Its issue is read ([AlertIssue]), and the alert is PRE-EXISTING when:
//
//   - the issue was first seen before the promotion, in a version other than
//     the release (first_build), and
//   - under the release it does not fire materially faster than it did
//     before: the release's version fired fewer than new_issue_min_events of
//     its events, or its rate is at most [PreExistingIssueMaxRateRatio] times
//     the other versions' rate, or the events above that rate number fewer
//     than new_issue_min_events. The rates are per version over the
//     version's own first and last event of the issue in the range
//     [P - baseline_window, after window's end), so they are what the issue
//     did while each version ran, however coarsely the backend buckets its
//     counts (the control plane counts by hour).
//
// A pre-existing alert's alert_firing finding is kept with policy warn,
// pre_existing set and a reason in [Finding.Unattributed], and the alert
// moves from FiringAlerts to [Report.PreExistingAlerts], so it is visible
// and never fails the release. It still fails, as before:
//
//   - an alert whose issue was first seen after the promotion, or in the
//     release (the issue rule's new_issue judges it as well);
//   - an issue the issue rule already judged (a new or regressed issue);
//   - an issue that fires faster under the release, beyond the ratio and by
//     at least new_issue_min_events events;
//   - an issue with no events of another version in the range, so no rate
//     before the release to compare with, when the release fired at least
//     new_issue_min_events of them (a dormant issue the release woke up);
//   - a critical alert (severity=critical): an env that made a rule critical
//     means it to page;
//   - an alert whose issue could not be read: without the issue nothing
//     proves the condition is old.
//
// An alert that names no issue is judged by the baseline-alert rule alone.

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// PreExistingIssueMaxRateRatio is how many times faster than before the
// release an issue may fire under it while an alert on it is still the
// issue's old condition rather than the release's doing. Event counts over
// minutes are noisy, so the bar is a doubling, and the excess must also reach
// new_issue_min_events events.
const PreExistingIssueMaxRateRatio = 2.0

// AlertIssue is the issue an alert names (label issue_id), as the
// pre-existing-alert rule reads it.
type AlertIssue struct {
	ID string
	// FirstSeenAt and FirstVersion are the issue's first event ever and the
	// version it carried; an empty FirstVersion is read from the alert's
	// first_build label.
	FirstSeenAt  time.Time
	FirstVersion string
	// Versions are the issue's events by service.version over
	// [P - baseline_window, the after window's end), each with the first and
	// last of them in that range.
	Versions []IssueVersionEvents
}

// IssueVersionEvents is one version's events of an issue over a range.
type IssueVersionEvents struct {
	Version     string
	Events      int64
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// PreExistingIssue is set on an alert of [Report.PreExistingAlerts]: the
// issue it fired for, when it was first seen, and its rate under the release
// and before it (0 when there were too few events to measure one).
type PreExistingIssue struct {
	ID              string    `json:"id"`
	FirstSeenAt     time.Time `json:"first_seen_at,omitzero"`
	FirstVersion    string    `json:"first_version,omitempty"`
	ReleaseEvents   int64     `json:"release_events"`
	ReleasePerHour  float64   `json:"release_per_hour,omitempty"`
	BaselineEvents  int64     `json:"baseline_events"`
	BaselinePerHour float64   `json:"baseline_per_hour,omitempty"`
}

// WithPreExistingAlerts applies the pre-existing-alert rule (above) to a
// judged report: each alert_firing finding of an alert whose issue predates
// the release and fires no faster under it becomes a warn finding saying so,
// and the alert moves from FiringAlerts to PreExistingAlerts. issues are the
// issues the alerts name, read by the caller; nil applies nothing. live is the
// env's alert state read now, or nil: it names the alerts behind a finding
// whose answer lists none. new_issue_min_events is read from the report, an
// override (the request's --threshold pairs; nil for none) winning. The
// verdict is recomputed when a finding changed, so it can improve: an issue's
// old condition was never the release's evidence. [Evaluate] judges by the
// same rule when given [Input.AlertIssues].
func WithPreExistingAlerts(r Report, issues []AlertIssue, live []Alert, overrides map[string]string) Report {
	minEvents, _, _ := issueMinEvents(withOverrides(r.Thresholds, overrides))
	return discountPreExistingAlerts(r, issues, live, minEvents)
}

// AlertIssueIDs are the issues the rule would read for r: the issue each
// counted alert_firing alert names (of r's FiringAlerts, then live), once
// each, in order. A caller reads these and passes them to
// [WithPreExistingAlerts].
func AlertIssueIDs(r Report, live []Alert) []string {
	if !slices.ContainsFunc(r.Findings, countedAlertFinding) {
		return nil
	}
	var out []string
	for _, a := range slices.Concat(r.FiringAlerts, live) {
		if id := issueOf(a); id != "" && !a.Critical() && !a.Excluded() && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func countedAlertFinding(f Finding) bool {
	return f.Code == release.HealthAlertFiring && f.Unattributed == ""
}

// issueOf is the issue an alert names, or "" for none or for a platform issue
// alert, which the issue rule judges (issue_alerts.go).
func issueOf(a Alert) string {
	if a.IssueCode() != "" {
		return ""
	}
	return strings.TrimSpace(a.Labels[LabelIssueID])
}

// discountPreExistingAlerts is the rule itself, with new_issue_min_events
// given.
func discountPreExistingAlerts(r Report, issues []AlertIssue, live []Alert, minEvents int) Report {
	p := r.Subject.PromotedAt
	if len(issues) == 0 || p.IsZero() {
		return r
	}
	byID := make(map[string]AlertIssue, len(issues))
	for _, is := range issues {
		if is.ID != "" {
			byID[is.ID] = is
		}
	}
	judged := map[string]bool{}
	for _, is := range slices.Concat(r.NewIssues, r.RegressedIssues) {
		judged[is.ID] = true
	}

	// Each rule's alerts behind its counted alert_firing findings, as
	// Evaluate counts them; the baseline-alert rule's already discounted.
	type tally struct {
		counted int
		pre     []Alert
		why     []string
	}
	rules := map[string]*tally{}
	var order []string
	seen := map[string]bool{}
	for _, a := range slices.Concat(r.BaselineAlerts, r.PreExistingAlerts) {
		seen[alertKey(a)] = true
	}
	consider := func(a Alert) {
		k := alertKey(a)
		if seen[k] || a.Excluded() || mirrorsJudgedIssue(a, r.NotEvaluated) || afterWindow(a, r.After.To) ||
			(!a.FiringAt.IsZero() && a.FiringAt.Before(p)) {
			return
		}
		seen[k] = true
		t := rules[a.Rule]
		if t == nil {
			t = &tally{}
			rules[a.Rule] = t
			order = append(order, a.Rule)
		}
		id := issueOf(a)
		is, read := byID[id]
		if id == "" || !read || judged[id] || a.Critical() {
			t.counted++
			return
		}
		pe, why, ok := preExisting(is, a, r.Subject, minEvents)
		if !ok {
			t.counted++
			return
		}
		a.Issue = &pe
		t.pre, t.why = append(t.pre, a), append(t.why, why)
	}
	for _, a := range r.FiringAlerts {
		consider(a)
	}
	for _, a := range live {
		consider(a)
	}

	out := r
	out.Findings = slices.Clone(r.Findings)
	moved := map[string]bool{}
	for _, rule := range order {
		t := rules[rule]
		var idx []int
		for i, f := range out.Findings {
			if countedAlertFinding(f) && findingNamesRule(f, rule) {
				idx = append(idx, i)
			}
		}
		take := min(len(t.pre), len(idx)-t.counted)
		for j := 0; j < take; j++ {
			a := t.pre[j]
			i := idx[len(idx)-1-j]
			out.Findings[i].Policy = PolicyWarn
			out.Findings[i].PreExisting = true
			out.Findings[i].Unattributed = t.why[j]
			out.Findings[i].Message += fmt.Sprintf("; its issue %s predates the release, so it is not counted against the release", a.Issue.ID)
			out.PreExistingAlerts = append(slices.Clip(out.PreExistingAlerts), a)
			moved[alertKey(a)] = true
		}
	}
	if len(moved) == 0 {
		return r
	}
	out.FiringAlerts = slices.DeleteFunc(slices.Clone(r.FiringAlerts), func(a Alert) bool { return moved[alertKey(a)] })
	if len(out.FiringAlerts) == 0 {
		out.FiringAlerts = nil
	}
	sort.SliceStable(out.PreExistingAlerts, func(i, j int) bool { return out.PreExistingAlerts[i].Rule < out.PreExistingAlerts[j].Rule })
	out.Verdict = verdictOf(out.Findings, out.AfterComplete, out.Services, gaugeOf(out.Thresholds), out.unproven())
	return out
}

// preExisting reports whether an alert's issue predates the release and fires
// no faster under it (above), with the figures and the sentence that say so.
func preExisting(is AlertIssue, a Alert, s Subject, minEvents int) (PreExistingIssue, string, bool) {
	firstVersion := firstNonEmpty(is.FirstVersion, strings.TrimSpace(a.Labels["first_build"]))
	pe := PreExistingIssue{ID: is.ID, FirstSeenAt: is.FirstSeenAt, FirstVersion: firstVersion}
	if s.Release == "" || is.FirstSeenAt.IsZero() || !is.FirstSeenAt.Before(s.PromotedAt) || firstVersion == s.Release {
		return pe, "", false // new with the release, or nothing to tell its version by
	}
	var rel, base IssueVersionEvents
	for _, v := range is.Versions {
		into := &base
		if v.Version == s.Release {
			into = &rel
		}
		into.Events += v.Events
		if !v.FirstSeenAt.IsZero() && (into.FirstSeenAt.IsZero() || v.FirstSeenAt.Before(into.FirstSeenAt)) {
			into.FirstSeenAt = v.FirstSeenAt
		}
		if v.LastSeenAt.After(into.LastSeenAt) {
			into.LastSeenAt = v.LastSeenAt
		}
	}
	pe.ReleaseEvents, pe.BaselineEvents = rel.Events, base.Events
	baseRate, baseOK := perHour(base)
	pe.BaselinePerHour = baseRate
	first := firstNonEmpty(firstVersion, is.FirstSeenAt.UTC().Format(time.RFC3339))
	since := fmt.Sprintf("issue %s was first seen %s in %s, before the promotion", is.ID, is.FirstSeenAt.UTC().Format(time.RFC3339), firstNonEmpty(firstVersion, "an earlier version"))
	if rel.Events < int64(minEvents) {
		return pe, fmt.Sprintf("pre-existing alert (first seen %s, too few events under the release to have risen): %s, and fired %d time(s) under %s (new_issue_min_events %d)",
			first, since, rel.Events, s.Release, minEvents), true
	}
	relRate, relOK := perHour(rel)
	pe.ReleasePerHour = relRate
	if !baseOK || !relOK {
		return pe, "", false // no rate before it to compare with, or a burst too short to measure
	}
	excess := float64(rel.Events) - baseRate*rel.LastSeenAt.Sub(rel.FirstSeenAt).Hours()
	if relRate > PreExistingIssueMaxRateRatio*baseRate && excess >= float64(minEvents) {
		return pe, "", false // it got worse under the release
	}
	return pe, fmt.Sprintf("pre-existing alert (first seen %s, rate unchanged): %s, and fires %.1f/h under %s against %.1f/h before it (limit %.2gx)",
		first, since, relRate, s.Release, baseRate, PreExistingIssueMaxRateRatio), true
}

// perHour is a version's events per hour over its first to last event: n
// events span n-1 intervals. Fewer than two events, or no time between them,
// measure no rate.
func perHour(v IssueVersionEvents) (float64, bool) {
	span := v.LastSeenAt.Sub(v.FirstSeenAt).Hours()
	if v.Events < 2 || span <= 0 {
		return 0, false
	}
	return float64(v.Events-1) / span, true
}
