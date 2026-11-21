package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/pkg/obsquery"
)

// TestUndeclaredGroupByWarnings pins obs 03279cf8: `--group-by rpc.method` on
// rpc.server.duration came back as one group with an empty rpc.method and no
// hint that the interceptor records the RPC as `procedure`.
func TestUndeclaredGroupByWarnings(t *testing.T) {
	t.Parallel()
	duration := []obsquery.PanelSeries{{Label: "A", Field: "rpc.server.duration"}}
	empty := func(key string) []obsbackend.MetricSeries {
		return []obsbackend.MetricSeries{{Label: "A", Labels: map[string]string{key: ""}}}
	}
	cases := map[string]struct {
		series  []obsquery.PanelSeries
		groupBy string
		result  []obsbackend.MetricSeries
		want    string // "" = no warning
	}{
		"undeclared key, one empty group": {duration, "rpc.method", empty("rpc.method"), `rpc.server.duration records no "rpc.method" attribute`},
		"declared key":                    {duration, "procedure", empty("procedure"), ""},
		"resource key":                    {duration, "service.name", empty("service.name"), ""},
		"undeclared key but values came back": {duration, "rpc.method",
			[]obsbackend.MetricSeries{{Label: "A", Labels: map[string]string{"rpc.method": "PlaceOrder"}}}, ""},
		"project metric is not judged": {[]obsquery.PanelSeries{{Label: "A", Field: "orders_created"}}, "rpc.method", empty("rpc.method"), ""},
		"no data is not judged":        {duration, "rpc.method", nil, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := undeclaredGroupByWarnings(obsquery.Panel{Series: tc.series, GroupBy: []string{tc.groupBy}}, tc.result)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no warning, got %q", got)
				}
				return
			}
			if len(got) != 1 || !strings.HasPrefix(got[0], warnGroupByUndeclared+": ") || !strings.Contains(got[0], tc.want) || !strings.Contains(got[0], "its attributes: procedure") {
				t.Fatalf("want one %s warning containing %q and naming procedure, got %q", warnGroupByUndeclared, tc.want, got)
			}
		})
	}
}
