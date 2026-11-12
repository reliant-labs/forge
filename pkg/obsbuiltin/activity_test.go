package obsbuiltin

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/dashspec"
	"github.com/reliant-labs/forge/pkg/obsquery"
)

// A project's declared user-activity rules reach every user count: each
// place a panel reads obsquery.ActiveEvidence it reads the rules' evidence,
// the Overview's Users card included, and a project with no rules is drawn
// exactly as before.
func TestActivityRulesReachEveryUserCount(t *testing.T) {
	rules := obsquery.Activity{NotActivity: []string{"billing_subscription_state"}, ExcludeUsersWith: []string{"internal_user"}}
	evidence := obsquery.ActiveEvidenceFor("product_events", rules)
	in := fullInput()
	in.Activity = rules
	plain := map[string]dashspec.Dashboard{}
	for _, d := range every(fullInput()) {
		plain[d.ID] = d
	}
	counted := map[string]int{}
	for _, d := range every(in) {
		for i, p := range d.Panels {
			for _, sql := range []string{p.SQL, p.TrendSQL} {
				bare, ruled := strings.Count(sql, obsquery.ActiveEvidence), strings.Count(sql, evidence)
				if bare != ruled {
					t.Errorf("%s/%s reads ActiveEvidence %d times but the rules only %d", d.ID, p.ID, bare, ruled)
				}
				counted[d.ID] += ruled
			}
			if was := plain[d.ID].Panels[i]; !strings.Contains(was.SQL+was.TrendSQL, obsquery.ActiveEvidence) &&
				(was.SQL != p.SQL || was.TrendSQL != p.TrendSQL) {
				t.Errorf("%s/%s does not count users but the rules changed it", d.ID, p.ID)
			}
		}
	}
	for _, id := range []string{UsersID, "overview"} {
		if counted[id] == 0 {
			t.Errorf("no panel of %s reads the rules", id)
		}
	}
}

func TestNoActivityRulesDrawTheDashboardsUnchanged(t *testing.T) {
	a, b := every(fullInput()), every(func() Input { in := fullInput(); in.Activity = obsquery.Activity{}; return in }())
	for i := range a {
		if string(marshal(t, a[i])) != string(marshal(t, b[i])) {
			t.Errorf("%s changed under zero rules", a[i].ID)
		}
	}
}
