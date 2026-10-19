package obsdash

import (
	"encoding/json"
	"strings"
	"testing"
)

func frontendDashboard(t *testing.T, in Input) Dashboard {
	t.Helper()
	for _, d := range Build(in).Dashboards {
		if d.ID == "frontend" {
			return d
		}
	}
	t.Fatal("no frontend dashboard")
	return Dashboard{}
}

func panelByID(d Dashboard) map[string]Panel {
	m := map[string]Panel{}
	for _, p := range d.Panels {
		m[p.ID] = p
	}
	return m
}

func TestFrontendDashboardLeadsWithIssuesAndSessions(t *testing.T) {
	d := frontendDashboard(t, fixture())
	by := panelByID(d)

	if _, ok := by["top-js-errors"]; ok {
		t.Error("top-js-errors groups by raw message; Issues replaces it")
	}
	if p := by["issues"]; p.Kind != KindIssues || len(p.Series) != 0 {
		t.Errorf("issues = %+v, want a console-drawn issues panel with no series", p)
	}
	if p := by["sessions-with-errors"]; p.Kind != KindSessions || !p.HasError {
		t.Errorf("sessions-with-errors = %+v, want a sessions panel narrowed to errors", p)
	}
	// The generated dashboard keeps its 15 non-funnel panels: 14 old minus top
	// errors, plus issues, sessions-with-errors, vitals-by-route, errors-by-release.
	if got := len(d.Panels); got != 18 {
		t.Errorf("frontend dashboard has %d panels, want 18", got)
	}
}

func TestVitalsByRouteGradesP75AgainstCoreWebVitals(t *testing.T) {
	p := panelByID(frontendDashboard(t, fixture()))["vitals-by-route"]
	if p.Kind != KindTable || len(p.GroupBy) != 1 || p.GroupBy[0] != "span.attr.location.href" {
		t.Fatalf("vitals-by-route = %+v", p)
	}
	want := map[string]Thresholds{
		"lcp p75": {Good: 2500, Poor: 4000},
		"inp p75": {Good: 200, Poor: 500},
		"cls p75": {Good: 0.1, Poor: 0.25},
	}
	graded := 0
	for _, s := range p.Series {
		w, ok := want[s.Label]
		if !ok {
			continue
		}
		graded++
		if s.Agg != AggQuantile || s.Quantile != 0.75 {
			t.Errorf("%s = %s %v, want the p75", s.Label, s.Agg, s.Quantile)
		}
		if s.Thresholds == nil || *s.Thresholds != w {
			t.Errorf("%s thresholds = %+v, want %+v", s.Label, s.Thresholds, w)
		}
		// One webvitals span carries one metric: a series must keep the others' spans out.
		if len(s.Where) != 1 || s.Where[0].Op != OpExists || s.Where[0].Field != s.Field {
			t.Errorf("%s where = %+v, want exists(%s)", s.Label, s.Where, s.Field)
		}
	}
	if graded != 3 {
		t.Errorf("graded %d vitals, want LCP, INP and CLS", graded)
	}
}

func TestErrorsByReleaseSplitsTheErrorSpansByVersion(t *testing.T) {
	d := frontendDashboard(t, fixture())
	p := panelByID(d)["errors-by-release"]
	if p.Kind != KindLine || len(p.GroupBy) != 1 || p.GroupBy[0] != fieldVersion {
		t.Fatalf("errors-by-release = %+v", p)
	}
	js := panelByID(d)["js-errors"]
	if len(p.Where) != len(js.Where) {
		t.Errorf("errors-by-release counts a different set of spans than js-errors: %+v vs %+v", p.Where, js.Where)
	}
	// A line chart: the console draws the release markers on it.
	if len(d.Annotations) != 1 || d.Annotations[0].Source != AnnotationReleases {
		t.Errorf("annotations = %+v, want the releases markers", d.Annotations)
	}
}

func TestEachDeclaredFunnelBecomesAPanelWithDefaults(t *testing.T) {
	in := fixture()
	in.Funnels = []FrontendFunnel{
		{ID: "onboarding", Title: "Onboarding", Funnel: Funnel{Steps: []string{"signup_started", "workspace_created", "first_chat"}, Window: "7d", By: FunnelByUser}},
		{ID: "checkout", Funnel: Funnel{Steps: []string{"cart_viewed", "paid"}}},
	}
	by := panelByID(frontendDashboard(t, in))

	ob := by["funnel-onboarding"]
	if ob.Kind != KindFunnel || ob.Title != "Onboarding" || ob.Funnel == nil || ob.Funnel.Window != "7d" || ob.Funnel.By != FunnelByUser {
		t.Errorf("onboarding = %+v funnel %+v", ob, ob.Funnel)
	}
	co := by["funnel-checkout"]
	if co.Funnel == nil || co.Funnel.Window != DefaultFunnelWindow || co.Funnel.By != DefaultFunnelBy || co.Title != "checkout" {
		t.Errorf("checkout = %+v funnel %+v, want the defaults filled in and the id as its title", co, co.Funnel)
	}
	// The declaration the build was handed is not mutated by the defaults.
	if in.Funnels[1].Window != "" {
		t.Error("Build wrote its defaults into the caller's funnel")
	}

	if got := len(frontendDashboard(t, fixture()).Panels); got != 18 {
		t.Errorf("no funnels declared, %d panels; want none added", got)
	}
}

func TestConsoleKindsAreSkippedByTheHyperDXRenderer(t *testing.T) {
	in := fixture()
	in.Funnels = []FrontendFunnel{{ID: "f", Funnel: Funnel{Steps: []string{"a", "b"}}}}
	sp := Build(in)
	var d Dashboard
	for _, x := range sp.Dashboards {
		if x.ID == "frontend" {
			d = x
		}
	}
	console := 0
	for _, p := range d.Panels {
		if IsConsoleKind(p.Kind) {
			console++
		}
	}
	if console != 3 {
		t.Fatalf("%d console panels, want issues, sessions and one funnel", console)
	}

	files, err := RenderHyperDX(sp)
	if err != nil {
		t.Fatalf("a console kind must not break the HyperDX render: %v", err)
	}
	for _, f := range files {
		if f.File != "shop-frontend_gen.json" {
			continue
		}
		var hd struct {
			Tiles []struct{ ID string } `json:"tiles"`
		}
		if err := json.Unmarshal(f.Content, &hd); err != nil {
			t.Fatal(err)
		}
		if len(hd.Tiles) != len(d.Panels)-console {
			t.Errorf("%d tiles for %d panels of which %d are console-only", len(hd.Tiles), len(d.Panels), console)
		}
		for _, tile := range hd.Tiles {
			for _, id := range []string{"issues", "sessions-with-errors", "funnel-f"} {
				if tile.ID == "t-"+id {
					t.Errorf("HyperDX was given a tile for the console-only panel %s", id)
				}
			}
		}
		if errs := validateHyperDXDashboard(f.Content); len(errs) > 0 {
			t.Errorf("rendered frontend dashboard fails the schema mirror: %v", errs)
		}
	}
}

func TestEveryGeneratedFrontendPanelValidatesAndRoundTrips(t *testing.T) {
	in := fixture()
	in.Funnels = []FrontendFunnel{{ID: "f", Funnel: Funnel{Steps: []string{"a", "b"}, Window: "30m"}}}
	d := frontendDashboard(t, in)
	b, err := marshalDashboardFile(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseDashboard("frontend.json", b)
	if err != nil {
		t.Fatalf("the generated frontend dashboard does not validate:\n%v", err)
	}
	by := panelByID(got)
	if by["funnel-f"].Funnel == nil || by["funnel-f"].Funnel.Window != "30m" || !by["sessions-with-errors"].HasError {
		t.Error("funnel or hasError lost in the round trip")
	}
	if th := by["vitals-by-route"].Series[0].Thresholds; th == nil || th.Poor != 4000 {
		t.Errorf("thresholds lost in the round trip: %+v", th)
	}
}

func TestValidationKnowsTheConsoleKindsAndRejectsUnknownOnes(t *testing.T) {
	panel := func(extra string) string {
		return `{"id":"x","title":"X","panels":[{"id":"p","title":"P",` + extra + `}]}`
	}
	ok := []struct{ name, src string }{
		{"issues", panel(`"kind":"issues","signal":"traces"`)},
		{"issues with a scope", panel(`"kind":"issues","signal":"traces","where":[{"field":"resource.attr.rum.sessionId","op":"exists"}]`)},
		{"sessions", panel(`"kind":"sessions","signal":"traces"`)},
		{"sessions with errors", panel(`"kind":"sessions","signal":"traces","hasError":true`)},
		{"funnel", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a","b"]}`)},
		{"funnel full", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a","b","c"],"window":"7d","by":"user"}`)},
		{"graded series", panel(`"kind":"table","signal":"traces","series":[{"label":"lcp","agg":"quantile","quantile":0.75,"field":"span.attr.lcp","thresholds":{"good":2500,"poor":4000}}]`)},
	}
	for _, tc := range ok {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			if _, err := ParseDashboard("f.json", []byte(tc.src)); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}

	bad := []struct{ name, src, want string }{
		{"unknown kind", panel(`"kind":"replay","signal":"traces"`), `"replay" is not one of`},
		{"heatmap is still unknown", panel(`"kind":"heatmap","signal":"traces"`), `"heatmap" is not one of`},
		{"issues with series", panel(`"kind":"issues","signal":"traces","series":[{"label":"a","agg":"count"}]`), `takes no "series"`},
		{"sessions with columns", panel(`"kind":"sessions","signal":"traces","columns":["timestamp"]`), `takes no "columns"`},
		{"funnel with group by", panel(`"kind":"funnel","signal":"traces","groupBy":["service.name"],"funnel":{"steps":["a","b"]}`), `takes no "groupBy"`},
		{"issues on logs", panel(`"kind":"issues","signal":"logs"`), "reads browser spans"},
		{"funnel without a definition", panel(`"kind":"funnel","signal":"traces"`), `needs a "funnel"`},
		{"funnel on a line", panel(`"kind":"line","signal":"traces","funnel":{"steps":["a","b"]},"series":[{"label":"a","agg":"count"}]`), `only a funnel panel takes "funnel"`},
		{"funnel on issues", panel(`"kind":"issues","signal":"traces","funnel":{"steps":["a","b"]}`), `only a funnel panel takes "funnel"`},
		{"hasError on issues", panel(`"kind":"issues","signal":"traces","hasError":true`), `only a sessions panel takes "hasError"`},
		{"hasError on a table", panel(`"kind":"table","signal":"traces","hasError":true,"series":[{"label":"a","agg":"count"}]`), `only a sessions panel takes "hasError"`},
		{"hasError not a bool", panel(`"kind":"sessions","signal":"traces","hasError":"yes"`), "must be true or false"},
		{"one step", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a"]}`), "2 to 10 steps"},
		{"eleven steps", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a","b","c","d","e","f","g","h","i","j","k"]}`), "2 to 10 steps"},
		{"repeated step", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a","b","a"]}`), `repeats an earlier step`},
		{"empty step", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a"," "]}`), "must not be empty"},
		{"step not a string", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a",7]}`), "must be a string"},
		{"bad window", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a","b"],"window":"1 hour"}`), "not a window"},
		{"zero window", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a","b"],"window":"0h"}`), "not a window"},
		{"bad by", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a","b"],"by":"device"}`), `"device" is not one of: session, user`},
		{"unknown funnel field", panel(`"kind":"funnel","signal":"traces","funnel":{"steps":["a","b"],"order":"any"}`), `unknown field "order"`},
		{"thresholds without poor", panel(`"kind":"table","signal":"traces","series":[{"label":"a","agg":"count","thresholds":{"good":1}}]`), `need both "good" and "poor"`},
		{"thresholds inverted", panel(`"kind":"table","signal":"traces","series":[{"label":"a","agg":"count","thresholds":{"good":9,"poor":3}}]`), "need 0 <= good < poor"},
		{"unknown thresholds field", panel(`"kind":"table","signal":"traces","series":[{"label":"a","agg":"count","thresholds":{"good":1,"poor":2,"ok":3}}]`), `unknown field "ok"`},
	}
	for _, tc := range bad {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := ParseDashboard("f.json", []byte(tc.src))
			if err == nil {
				t.Fatalf("validated; want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestEveryPanelKindTheSpecDeclaresIsKnownToTheValidator(t *testing.T) {
	// A kind added to spec.go but not to validate.go would be a panel the
	// console is sent and the validator rejects in a user's file.
	for _, k := range []PanelKind{KindNumber, KindLine, KindTable, KindSearch, KindIssues, KindSessions, KindFunnel} {
		var src string
		switch k {
		case KindSearch:
			src = `"kind":"search","signal":"logs","columns":["timestamp"]`
		case KindFunnel:
			src = `"kind":"funnel","signal":"traces","funnel":{"steps":["a","b"]}`
		case KindIssues, KindSessions:
			src = `"kind":"` + string(k) + `","signal":"traces"`
		default:
			src = `"kind":"` + string(k) + `","signal":"traces","series":[{"label":"a","agg":"count"}]`
		}
		doc := `{"id":"x","title":"X","panels":[{"id":"p","title":"P",` + src + `}]}`
		if _, err := ParseDashboard("f.json", []byte(doc)); err != nil {
			t.Errorf("kind %q: %v", k, err)
		}
	}
}
