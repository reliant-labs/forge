package obsquery

import (
	"strings"
	"testing"
)

// A trace's page row names its client, its frontend build and its backend
// builds after the location, before any RED group column, so a reader that
// scans by position (the local backend, the control plane) keeps working.
func TestTraceRowsNameTheClientAndBuilds(t *testing.T) {
	st, err := (TraceQuery{Scope: scope(), Limit: 10, GroupBy: []string{"service.name"}}).CompileREDGroups()
	if err != nil {
		t.Fatal(err)
	}
	page, err := (TraceQuery{Scope: scope(), Limit: 10}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	last := strings.Index(page.SQL, "AS city")
	for _, col := range []string{"AS client_name", "AS client_version", "AS os_name", "AS os_version", "AS device_type",
		"AS desktop_version", "AS frontend_version", "AS backend_services", "AS backend_versions"} {
		i := strings.Index(page.SQL, col)
		if i < last {
			t.Errorf("%s missing or out of order:\n%s", col, page.SQL)
		}
		last = i
	}
	if g := strings.Index(st.SQL, "AS g0"); g < strings.Index(st.SQL, "AS backend_versions") {
		t.Errorf("a RED group column precedes the client columns:\n%s", st.SQL)
	}
	// The frontend is a browser span's build; the backend every other span's
	// that did not come through the browser pipeline.
	for _, want := range []string{
		"anyIf(ResourceAttributes['service.version'], ResourceAttributes['rum.sessionId'] != '' AND",
		"ResourceAttributes['rum.sessionId'] = '' AND ResourceAttributes['reliant.ingest.class'] != 'browser'",
	} {
		if !strings.Contains(page.SQL, want) {
			t.Errorf("missing %q:\n%s", want, page.SQL)
		}
	}
}

// Each client and build filter narrows the candidate traces to those with a
// matching span on its side, binds its value, and is part of the page token.
func TestTraceClientAndBuildFilters(t *testing.T) {
	base := TraceQuery{Scope: scope(), Limit: 10}
	for name, c := range map[string]struct {
		q    TraceQuery
		want string
	}{
		"browser":  {TraceQuery{Scope: scope(), Limit: 10, Browser: "Firefox"}, "ResourceAttributes['rum.sessionId'] != '' AND ResourceAttributes['user_agent.name'] = {"},
		"os":       {TraceQuery{Scope: scope(), Limit: 10, OS: "Windows"}, "ResourceAttributes['rum.sessionId'] != '' AND ResourceAttributes['os.name'] = {"},
		"frontend": {TraceQuery{Scope: scope(), Limit: 10, FrontendVersion: "1.8.16"}, "ResourceAttributes['rum.sessionId'] != '' AND ResourceAttributes['service.version'] = {"},
		"backend":  {TraceQuery{Scope: scope(), Limit: 10, BackendVersion: "v1.7.91"}, "ResourceAttributes['reliant.ingest.class'] != 'browser' AND ResourceAttributes['service.version'] = {"},
	} {
		st, err := c.q.Compile()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(st.SQL, "TraceId IN (SELECT TraceId FROM") || !strings.Contains(st.SQL, c.want) {
			t.Errorf("%s: no prefilter %q:\n%s", name, c.want, st.SQL)
		}
		if c.q.Fingerprint() == base.Fingerprint() {
			t.Errorf("%s is not part of the page token", name)
		}
	}
	if (TraceQuery{Scope: scope(), Limit: 10}).Fingerprint() != base.Fingerprint() {
		t.Error("a query without the new filters changed its page token")
	}
	if _, err := (TraceQuery{Scope: scope(), Limit: 10, BackendVersion: strings.Repeat("v", 1000)}).Compile(); !isInvalid(err) {
		t.Errorf("an oversized backend_version compiled: %v", err)
	}
}
