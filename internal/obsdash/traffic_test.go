package obsdash

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/dashspec"
	"github.com/reliant-labs/forge/pkg/obsquery"
)

// Without a hosted static frontend the frontend dashboard is what it was.
func TestFrontendDashboardHasNoEdgeGroupsWithoutAHostedFrontend(t *testing.T) {
	d := frontendDashboard(t, fixture())
	if len(d.Sections) != 0 {
		t.Errorf("sections = %+v", d.Sections)
	}
	for _, p := range d.Panels {
		if strings.HasPrefix(p.ID, "edge-") || strings.HasPrefix(p.ID, "bots-") {
			t.Errorf("panel %s on a project with no hosted frontend", p.ID)
		}
	}
}

func TestFrontendDashboardGainsTrafficAndBotsForAHostedFrontend(t *testing.T) {
	in := fixture()
	in.HostedStaticFrontends = []string{in.Frontends[0]}
	d := frontendDashboard(t, in)
	if len(d.Sections) != 2 || d.Sections[0].Title != "Traffic" || d.Sections[1].Title != "Bots & agents" {
		t.Fatalf("sections = %+v", d.Sections)
	}
	want := map[string]string{
		"edge-summary": sectionTraffic, "edge-by-class": sectionTraffic, "edge-top-pages": sectionTraffic, "edge-404s": sectionTraffic,
		"edge-top-404s": sectionTraffic, "edge-cache-hit": sectionTraffic, "edge-bytes": sectionTraffic,
		"bots-by-agent": sectionBots, "bots-verified": sectionBots, "bots-discovery": sectionBots, "bots-top-pages": sectionBots,
	}
	by := panelByID(d)
	for id, section := range want {
		p, ok := by[id]
		if !ok {
			t.Errorf("no panel %s", id)
			continue
		}
		if p.Section != section || p.Kind != KindSQL || !strings.Contains(p.SQL, obsquery.TrafficTable) {
			t.Errorf("%s = section %q kind %s", id, p.Section, p.Kind)
		}
		// The statement passes the sql-panel guard and binds like any sql
		// panel, as the console and obs dashboards run it.
		if _, err := (obsquery.SQLPanel{Scope: obsquery.Scope{Database: "t_org", EnvRestricted: true, EnvIDs: []string{"e1"}}, SQL: p.SQL, StepSeconds: 3600}).Compile(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	// The renderer-neutral spec the console draws accepts the dashboard.
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dashspec.ParseDashboard("frontend.json", raw); err != nil {
		t.Errorf("the dashboard fails dashspec validation: %v", err)
	}
	// HyperDX draws no sql panel: the local dashboard is unchanged.
	for _, p := range edgePanels() {
		if hyperDXDraws(p) {
			t.Errorf("HyperDX would draw %s", p.ID)
		}
	}
}
