package releasehealth

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/siteaudit"
)

func ip(n int) *int { return &n }

// audit is a finished audit of v2 by the current rules, compared with v1's.
func audit(seo, prevSEO, a11y, prevA11y int, routes ...SiteAuditRoute) SiteAuditFacts {
	return SiteAuditFacts{Frontend: "web", Release: "v2", State: SiteAuditDone, RulesVersion: siteaudit.RulesVersion,
		SEO: ip(seo), A11y: ip(a11y), Routes: routes,
		Previous: &SiteAuditBaseline{Release: "v1", RulesVersion: siteaudit.RulesVersion, SEO: ip(prevSEO), A11y: ip(prevA11y)}}
}

func withNewErrors(a SiteAuditFacts, errs ...SiteAuditFinding) SiteAuditFacts {
	a.NewErrors = errs
	return a
}

// siteInput is a passing release (one service, equal traffic) with audits.
func siteInput(t *testing.T, audits ...SiteAuditFacts) Input {
	in := input(t, true, svc("api", 1000, 1000, 0.01, 0.01, 100, 100))
	in.SiteAudits = audits
	return in
}

func codesRefs(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code+"/"+f.Ref)
	}
	return out
}

func TestEvaluateSiteAudits(t *testing.T) {
	seoErr := SiteAuditFinding{Code: "indexing_mismatch", Category: siteaudit.CategorySEO, Route: "/"}
	a11yErr := SiteAuditFinding{Code: "a11y_violation", Category: siteaudit.CategoryAccessibility, Route: "/pricing", Ref: "button-name"}
	tests := []struct {
		name         string
		audits       []SiteAuditFacts
		want         []string // code/ref of each finding, in order
		notEvaluated bool
	}{
		{"no audits judges nothing", nil, nil, false},
		{"unchanged scores", []SiteAuditFacts{audit(90, 90, 95, 95)}, nil, false},
		{"a drop of exactly the limit does not fire", []SiteAuditFacts{audit(80, 90, 85, 95)}, nil, false},
		{"a site drop over the limit fires", []SiteAuditFacts{audit(79, 90, 95, 95)}, []string{"seo_regressed/score"}, false},
		{"an a11y drop over the limit fires", []SiteAuditFacts{audit(90, 90, 70, 95)}, []string{"a11y_regressed/score"}, false},
		{"a route drop fires while the site mean holds", []SiteAuditFacts{audit(88, 90, 95, 95,
			SiteAuditRoute{Route: "/", SEO: ip(95), PreviousSEO: ip(95)},
			SiteAuditRoute{Route: "/pricing", SEO: ip(60), PreviousSEO: ip(92)})}, []string{"seo_regressed/score"}, false},
		{"a route the previous audit did not crawl has no baseline score", []SiteAuditFacts{audit(90, 90, 95, 95,
			SiteAuditRoute{Route: "/new", SEO: ip(20)})}, nil, false},
		{"an improvement is no finding", []SiteAuditFacts{audit(99, 60, 99, 40)}, nil, false},
		{"a new SEO error fires without a score drop", []SiteAuditFacts{withNewErrors(audit(90, 90, 95, 95), seoErr)}, []string{"seo_regressed/new_errors"}, false},
		{"a new critical axe violation fires a11y_regressed", []SiteAuditFacts{withNewErrors(audit(90, 90, 95, 95), a11yErr)}, []string{"a11y_regressed/new_errors"}, false},
		{"both kinds, both codes", []SiteAuditFacts{withNewErrors(audit(50, 90, 50, 95), seoErr, a11yErr)},
			[]string{"seo_regressed/new_errors", "seo_regressed/score", "a11y_regressed/new_errors", "a11y_regressed/score"}, false},
		{"no previous audit means no finding", []SiteAuditFacts{func() SiteAuditFacts {
			a := withNewErrors(audit(10, 90, 10, 95), seoErr)
			a.Previous = nil
			return a
		}()}, nil, false},
		{"a previous audit by other rules is not compared", []SiteAuditFacts{func() SiteAuditFacts {
			a := withNewErrors(audit(10, 90, 10, 95), seoErr)
			a.Previous.RulesVersion = "0"
			return a
		}()}, nil, false},
		{"a pending audit is not evaluated", []SiteAuditFacts{{Frontend: "web", Release: "v2", State: SiteAuditPending}}, nil, true},
		{"a failed audit is not evaluated", []SiteAuditFacts{{Frontend: "web", Release: "v2", State: SiteAuditFailed}}, nil, true},
		{"a missing audit is not evaluated", []SiteAuditFacts{{Frontend: "web", State: SiteAuditMissing}}, nil, true},
		{"an audit with no state is not evaluated", []SiteAuditFacts{{Frontend: "web", SEO: ip(1)}}, nil, true},
		{"an audit of another release is not evaluated", []SiteAuditFacts{func() SiteAuditFacts {
			a := audit(10, 90, 10, 95)
			a.Release = "v3"
			return a
		}()}, nil, true},
		{"one frontend audited, one pending", []SiteAuditFacts{audit(10, 90, 95, 95), {Frontend: "docs", State: SiteAuditPending}},
			[]string{"seo_regressed/score"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Evaluate(siteInput(t, tc.audits...))
			if got := codesRefs(r.Findings); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("findings = %v, want %v", got, tc.want)
			}
			for _, f := range r.Findings {
				if f.Policy != PolicyWarn || f.Service == "" {
					t.Errorf("finding %+v: want a warn naming its frontend", f)
				}
			}
			// Warn-only by default: the verdict is the traffic's (D2).
			if r.Verdict != Pass {
				t.Errorf("verdict = %s, want PASS: a site-audit finding warns and a pending audit never holds a release", r.Verdict)
			}
			if got := slices.Contains(r.NotEvaluated, NotEvaluatedSiteAudit); got != tc.notEvaluated {
				t.Errorf("not_evaluated = %v, want site_audit %t", r.NotEvaluated, tc.notEvaluated)
			}
		})
	}
}

// The score finding carries the largest drop and the limit, names the
// release it compares with, and lists the worst routes first.
func TestSiteAuditFindingFigures(t *testing.T) {
	a := audit(80, 92, 95, 95,
		SiteAuditRoute{Route: "/blog", SEO: ip(70), PreviousSEO: ip(85)},
		SiteAuditRoute{Route: "/pricing", SEO: ip(60), PreviousSEO: ip(95)})
	a = withNewErrors(a,
		SiteAuditFinding{Code: "link_broken", Category: siteaudit.CategorySEO, Route: "/blog", Ref: "https://web.example/gone"},
		SiteAuditFinding{Code: "canonical_foreign_host", Category: siteaudit.CategorySEO, Route: "/", Ref: "https://other.example/"})
	r := Evaluate(siteInput(t, a))
	if len(r.Findings) != 2 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	errs, score := r.Findings[0], r.Findings[1]
	if score.Ref != SiteAuditRefScore || score.Value != 35 || score.Threshold != 10 || score.Service != "web" {
		t.Errorf("score finding = %+v", score)
	}
	if want := "web: SEO score dropped since v1: site 92 -> 80 (-12), /pricing 95 -> 60 (-35), /blog 85 -> 70 (-15) (limit 10 points)"; score.Message != want {
		t.Errorf("message = %q\nwant      %q", score.Message, want)
	}
	if errs.Ref != SiteAuditRefNewErrors || errs.Value != 2 || errs.Threshold != 1 {
		t.Errorf("new-errors finding = %+v", errs)
	}
	if want := "web: 2 new SEO errors since v1: canonical_foreign_host https://other.example/ on /; link_broken https://web.example/gone on /blog"; errs.Message != want {
		t.Errorf("message = %q\nwant      %q", errs.Message, want)
	}
}

// A message names at most five items, then counts the rest.
func TestSiteAuditFindingMessageIsBounded(t *testing.T) {
	a := audit(90, 90, 95, 95)
	for _, route := range []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g"} {
		a.NewErrors = append(a.NewErrors, SiteAuditFinding{Code: "a11y_violation", Category: siteaudit.CategoryAccessibility, Route: route, Ref: "image-alt"})
	}
	r := Evaluate(siteInput(t, a))
	if len(r.Findings) != 1 || r.Findings[0].Value != 7 || !strings.HasSuffix(r.Findings[0].Message, "image-alt on /e; and 2 more") ||
		!strings.Contains(r.Findings[0].Message, "7 new accessibility critical violations since v1") {
		t.Errorf("findings = %+v", r.Findings)
	}
}

// The thresholds come from the env's policy: a stricter one fires on a
// smaller drop, and 0 on any drop.
func TestSiteAuditThresholdsFollowThePolicy(t *testing.T) {
	shape := release.DefaultReleaseHealth()
	shape.SEOMaxScoreDrop, shape.A11yMaxScoreDrop = ip(0), ip(30)
	th, err := Resolve(Options{Shape: &shape})
	if err != nil {
		t.Fatal(err)
	}
	if th.SEOMaxScoreDrop != 0 || th.A11yMaxScoreDrop != 30 || th.Sources[ThresholdSEOMaxScoreDrop] != SourceReleaseShape {
		t.Fatalf("thresholds = %+v", th)
	}
	in := siteInput(t, audit(89, 90, 70, 95))
	in.Thresholds = th
	r := Evaluate(in)
	if got := codesRefs(r.Findings); !reflect.DeepEqual(got, []string{"seo_regressed/score"}) {
		t.Errorf("findings = %v: a 1-point SEO drop fires at 0, a 25-point a11y drop does not at 30", got)
	}
	l := map[string]Threshold{}
	for _, x := range r.Thresholds {
		l[x.Name] = x
	}
	if l[ThresholdSEOMaxScoreDrop].Value != "0" || l[ThresholdA11yMaxScoreDrop].Value != "30" || l[ThresholdA11yMaxScoreDrop].Source != SourceReleaseShape {
		t.Errorf("listed thresholds = %+v", r.Thresholds)
	}
	def := defaults(t)
	if def.SEOMaxScoreDrop != release.DefaultSiteAuditMaxScoreDrop || def.A11yMaxScoreDrop != release.DefaultSiteAuditMaxScoreDrop {
		t.Errorf("defaults = %d, %d", def.SEOMaxScoreDrop, def.A11yMaxScoreDrop)
	}
	// Not a --threshold override: a site audit is judged from the shape.
	if _, err := ParseOverrides([]string{"seo_max_score_drop=5"}); err == nil {
		t.Error("seo_max_score_drop is accepted as a --threshold override")
	}
}

// fail_on opts in: the env's policy, or --fail-on through Rejudge.
func TestSiteAuditFailOn(t *testing.T) {
	in := siteInput(t, audit(50, 90, 95, 95))
	in.Thresholds.FailOn = append(slices.Clone(in.Thresholds.FailOn), release.HealthSEORegressed)
	if r := Evaluate(in); r.Verdict != Fail || r.Findings[0].Policy != PolicyFail {
		t.Errorf("fail_on seo_regressed: verdict %s, findings %+v", r.Verdict, r.Findings)
	}

	warned := Evaluate(siteInput(t, audit(50, 90, 50, 95)))
	if warned.Verdict != Pass {
		t.Fatalf("verdict = %s", warned.Verdict)
	}
	failOn, err := ParseFailOn("a11y_regressed,error_rate")
	if err != nil {
		t.Fatal(err)
	}
	r := Rejudge(warned, failOn)
	if r.Verdict != Fail {
		t.Errorf("--fail-on a11y_regressed: verdict %s", r.Verdict)
	}
	for _, f := range r.Findings {
		if want := map[string]string{release.HealthA11yRegressed: PolicyFail, release.HealthSEORegressed: PolicyWarn}[f.Code]; f.Policy != want {
			t.Errorf("%s policy = %s, want %s", f.Code, f.Policy, want)
		}
	}
	if back := Rejudge(r, nil); back.Verdict != Pass {
		t.Errorf("an empty --fail-on: verdict %s", back.Verdict)
	}
}

// A site audit judges the release's own bytes, not a traffic window: a
// window that is not the live release's, or a rollout it barely served,
// takes nothing off it.
func TestSiteAuditFindingsAreAlwaysTheReleases(t *testing.T) {
	in := siteInput(t, audit(50, 90, 95, 95))
	in.Thresholds.FailOn = append(slices.Clone(in.Thresholds.FailOn), release.HealthSEORegressed)
	r := Evaluate(in)
	r.WindowNotLive = "the release went live after the window"
	r = Attribute(r)
	if f := r.Findings[0]; f.Code != release.HealthSEORegressed || f.Policy != PolicyFail || f.Unattributed != "" {
		t.Errorf("finding = %+v: a window that is not live must not move a site-audit finding", f)
	}
}

func score(seo, a11y *int) siteaudit.RouteScore { return siteaudit.RouteScore{SEO: seo, A11y: a11y} }

func report(site siteaudit.RouteScore, routes []siteaudit.RouteReport, findings ...siteaudit.Finding) *siteaudit.Report {
	return &siteaudit.Report{RulesVersion: siteaudit.RulesVersion, Site: siteaudit.SiteSummary{Score: site}, Routes: routes, Findings: findings}
}

func TestCompareSiteAudits(t *testing.T) {
	errAt := func(code string, cat siteaudit.Category, route, ref string) siteaudit.Finding {
		return siteaudit.Finding{Code: code, Severity: siteaudit.SeverityError, Category: cat, Route: route, Ref: ref}
	}
	prev := report(score(ip(95), ip(98)), []siteaudit.RouteReport{
		{Route: "/", Status: 200, Score: score(ip(95), ip(98))},
		{Route: "/docs", Status: 200, Score: score(ip(90), nil)}, // axe did not run on /docs
	},
		errAt("link_broken", siteaudit.CategorySEO, "/", "https://web.example/old"),
		siteaudit.Finding{Code: "og_tag_missing", Severity: siteaudit.SeverityWarn, Category: siteaudit.CategorySEO, Route: "/", Ref: "og:image"})
	cur := report(score(ip(70), ip(90)), []siteaudit.RouteReport{
		{Route: "/", Status: 200, Score: score(ip(60), ip(90))},
		{Route: "/docs", Status: 200, Score: score(ip(80), ip(85))},
		{Route: "/new", Status: 200, Score: score(ip(90), ip(90))},
	},
		errAt("link_broken", siteaudit.CategorySEO, "/", "https://web.example/old"),                                                 // had it: not new
		errAt("indexing_mismatch", siteaudit.CategorySEO, "/", ""),                                                                  // new
		errAt("robots_unreachable", siteaudit.CategorySEO, "", ""),                                                                  // new, site-wide
		errAt("a11y_violation", siteaudit.CategoryAccessibility, "/docs", "color-contrast"),                                         // no a11y baseline on /docs
		errAt("a11y_violation", siteaudit.CategoryAccessibility, "/new", "button-name"),                                             // a page the release added
		errAt("a11y_violation", siteaudit.CategoryAccessibility, "/", "image-alt"),                                                  // new
		siteaudit.Finding{Code: "og_tag_missing", Severity: siteaudit.SeverityWarn, Category: siteaudit.CategorySEO, Route: "/new"}) // a warn: never new

	f := CompareSiteAudits("web", "v2", cur, "v1", prev)
	if f.State != SiteAuditDone || f.Frontend != "web" || f.Release != "v2" || f.RulesVersion != siteaudit.RulesVersion ||
		*f.SEO != 70 || *f.A11y != 90 || f.Previous == nil || f.Previous.Release != "v1" || *f.Previous.SEO != 95 {
		t.Fatalf("facts = %+v", f)
	}
	wantRoutes := []SiteAuditRoute{
		{Route: "/", SEO: ip(60), A11y: ip(90), PreviousSEO: ip(95), PreviousA11y: ip(98)},
		{Route: "/docs", SEO: ip(80), A11y: ip(85), PreviousSEO: ip(90)},
		{Route: "/new", SEO: ip(90), A11y: ip(90)},
	}
	if !reflect.DeepEqual(f.Routes, wantRoutes) {
		t.Errorf("routes = %s", mustJSON(t, f.Routes))
	}
	wantNew := []SiteAuditFinding{
		{Code: "indexing_mismatch", Category: siteaudit.CategorySEO, Route: "/"},
		{Code: "robots_unreachable", Category: siteaudit.CategorySEO},
		{Code: "a11y_violation", Category: siteaudit.CategoryAccessibility, Route: "/new", Ref: "button-name"},
		{Code: "a11y_violation", Category: siteaudit.CategoryAccessibility, Route: "/", Ref: "image-alt"},
	}
	if !reflect.DeepEqual(f.NewErrors, wantNew) {
		t.Errorf("new errors = %s", mustJSON(t, f.NewErrors))
	}

	// The facts judge as Evaluate's input: both codes, both kinds.
	r := Evaluate(siteInput(t, f))
	if got := codesRefs(r.Findings); !reflect.DeepEqual(got, []string{"seo_regressed/new_errors", "seo_regressed/score", "a11y_regressed/new_errors"}) {
		t.Errorf("findings = %v", got)
	}

	// No current report: missing. No previous: nothing is new.
	if m := CompareSiteAudits("web", "v2", nil, "v1", prev); m.State != SiteAuditMissing || m.Previous != nil {
		t.Errorf("no current report = %+v", m)
	}
	if first := CompareSiteAudits("web", "v2", cur, "", nil); first.Previous != nil || len(first.NewErrors) != 0 || first.Routes[0].PreviousSEO != nil {
		t.Errorf("no previous report = %+v", first)
	}
	// Different rules: named, not compared.
	old := *prev
	old.RulesVersion = "0"
	if d := CompareSiteAudits("web", "v2", cur, "v1", &old); d.Previous == nil || d.Previous.RulesVersion != "0" || len(d.NewErrors) != 0 || d.Routes[0].PreviousSEO != nil {
		t.Errorf("other rules = %+v", d)
	} else if r := Evaluate(siteInput(t, d)); len(r.Findings) != 0 {
		t.Errorf("an audit by other rules was compared: %+v", r.Findings)
	}
	// The facts are copies: the reports stay the caller's.
	*f.SEO = 0
	if *cur.Site.Score.SEO != 70 {
		t.Error("CompareSiteAudits aliases the report's scores")
	}
}

// The facts are a wire shape (a control plane may store or log them): pin
// their JSON.
func TestSiteAuditFactsJSON(t *testing.T) {
	a := withNewErrors(audit(80, 92, 95, 95, SiteAuditRoute{Route: "/", SEO: ip(80), PreviousSEO: ip(92)}),
		SiteAuditFinding{Code: "indexing_mismatch", Category: siteaudit.CategorySEO, Route: "/"})
	want := `{"frontend":"web","release":"v2","state":"done","rules_version":"1","seo":80,"a11y":95,` +
		`"routes":[{"route":"/","seo":80,"previous_seo":92}],` +
		`"previous":{"release":"v1","rules_version":"1","seo":92,"a11y":95},` +
		`"new_errors":[{"code":"indexing_mismatch","category":"seo","route":"/"}]}`
	if got := mustJSON(t, a); got != want {
		t.Errorf("json = %s\nwant   %s", got, want)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A category the previous audit judged nowhere (no site-level score: it ran
// no axe, or scored no page for SEO) is no baseline: a site-wide error of
// it, or one on a page the release added, is not new (review B1).
func TestCompareSiteAuditsNeedsABaselineForTheCategory(t *testing.T) {
	siteWide := func(code string, cat siteaudit.Category) siteaudit.Finding {
		return siteaudit.Finding{Code: code, Severity: siteaudit.SeverityError, Category: cat}
	}
	cur := report(score(ip(80), ip(80)), []siteaudit.RouteReport{
		{Route: "/", Status: 200, Score: score(ip(80), ip(80))},
		{Route: "/new", Status: 200, Score: score(ip(80), ip(80))},
	},
		siteWide("robots_unreachable", siteaudit.CategorySEO),
		siteWide("a11y_violation", siteaudit.CategoryAccessibility),
		siteaudit.Finding{Code: "a11y_violation", Severity: siteaudit.SeverityError, Category: siteaudit.CategoryAccessibility, Route: "/new", Ref: "button-name"},
		siteaudit.Finding{Code: "indexing_mismatch", Severity: siteaudit.SeverityError, Category: siteaudit.CategorySEO, Route: "/new"},
		siteaudit.Finding{Code: "made_up", Severity: siteaudit.SeverityError, Category: "performance"})
	for _, tc := range []struct {
		name string
		prev siteaudit.RouteScore
		want []string // codes of the new errors, by category
	}{
		{"no a11y score anywhere: only SEO errors are new", score(ip(95), nil), []string{"seo:robots_unreachable", "seo:indexing_mismatch"}},
		{"no SEO score anywhere: only a11y errors are new", score(nil, ip(95)), []string{"accessibility:a11y_violation", "accessibility:a11y_violation"}},
		{"neither: nothing is new", score(nil, nil), nil},
		{"both: every known category's errors are new", score(ip(95), ip(95)),
			[]string{"seo:robots_unreachable", "accessibility:a11y_violation", "accessibility:a11y_violation", "seo:indexing_mismatch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := report(tc.prev, []siteaudit.RouteReport{{Route: "/", Status: 200, Score: tc.prev}})
			f := CompareSiteAudits("web", "v2", cur, "v1", prev)
			var got []string
			for _, e := range f.NewErrors {
				got = append(got, string(e.Category)+":"+e.Code)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("new errors = %v, want %v", got, tc.want)
			}
		})
	}
}

// Evaluate holds facts built by hand to the same rule: a new error of a
// category with no previous site-level score is no finding.
func TestSiteAuditNewErrorsNeedAPreviousScore(t *testing.T) {
	seoErr := SiteAuditFinding{Code: "robots_unreachable", Category: siteaudit.CategorySEO}
	a11yErr := SiteAuditFinding{Code: "a11y_violation", Category: siteaudit.CategoryAccessibility, Ref: "region"}
	for _, tc := range []struct {
		name            string
		prevSEO, prA11y *int
		want            []string
	}{
		{"no previous a11y score", ip(90), nil, []string{"seo_regressed/new_errors"}},
		{"no previous SEO score", nil, ip(90), []string{"a11y_regressed/new_errors"}},
		{"no previous score at all", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := withNewErrors(audit(90, 90, 95, 95), seoErr, a11yErr)
			a.Previous.SEO, a.Previous.A11y = tc.prevSEO, tc.prA11y
			r := Evaluate(siteInput(t, a))
			if got := codesRefs(r.Findings); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("findings = %v, want %v", got, tc.want)
			}
		})
	}
}
