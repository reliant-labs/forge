package obsbackend

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/devtelemetry"
)

func hostedRecord() *devtelemetry.Record {
	return &devtelemetry.Record{Env: "dev", Backend: "cloud", OrgID: "org-1", EnvironmentID: "env-dev",
		InstanceID: "sess-0123456789abcdef01234567", InstanceLabel: "alice/fix-login", InstanceUser: "alice@x"}
}

// A local env whose `forge env up` recorded the hosted store resolves to the
// cloud backend, bound to the record, instead of refusing as "not declared".
func TestResolveHostedDevRecord(t *testing.T) {
	cp := "https://cp.example.com"
	local := Facts{Env: "dev", HasLocal: true, ControlPlane: cp, DevTelemetry: hostedRecord()}

	dec, err := Resolve(local, "")
	if err != nil || dec.Backend != BackendCloud || dec.HostedDev == nil || dec.HostedDev.EnvironmentID != "env-dev" {
		t.Fatalf("Resolve = %+v, %v; want cloud bound to the record", dec, err)
	}
	if dec.Reason != "dev_telemetry=hosted: instance alice/fix-login" {
		t.Errorf("reason = %q", dec.Reason)
	}
	declared := local
	declared.Declared, declared.Enabled = true, true
	if dec, err := Resolve(declared, ""); err != nil || dec.HostedDev == nil {
		t.Errorf("a declared block with a hosted record: %+v, %v", dec, err)
	}
	if dec, err := Resolve(local, BackendCloud); err != nil || dec.HostedDev == nil || !strings.HasPrefix(dec.Reason, "--backend cloud: ") {
		t.Errorf("--backend cloud with a hosted record: %+v, %v", dec, err)
	}
	if dec, err := Resolve(local, BackendLocal); err != nil || dec.Backend != BackendLocal || dec.HostedDev != nil {
		t.Errorf("--backend local still wins: %+v, %v", dec, err)
	}

	for name, c := range map[string]struct {
		f      Facts
		errHas string
	}{
		"disabled wins over a stale record": {func() Facts { f := local; f.Declared = true; return f }(), "its observability is disabled"},
		"no control plane to query":         {func() Facts { f := local; f.ControlPlane = ""; return f }(), "declares no control_plane to query it through"},
		"incomplete record": {func() Facts {
			f := local
			r := *hostedRecord()
			r.EnvironmentID = ""
			f.DevTelemetry = &r
			return f
		}(), "names no org or environment"},
	} {
		if _, err := Resolve(c.f, ""); err == nil || !strings.Contains(err.Error(), c.errHas) || err.(*Error).ExitCode() != ExitUndetermined {
			t.Errorf("%s: err = %v, want exit 2 containing %q", name, err, c.errHas)
		}
	}

	// A record says nothing for an env that no longer runs here, nor when it
	// recorded the local store: today's table decides.
	for name, f := range map[string]Facts{
		"not local": func() Facts { f := local; f.HasLocal = false; return f }(),
		"recorded local": func() Facts {
			f := local
			r := *hostedRecord()
			r.Backend = "local"
			f.DevTelemetry = &r
			return f
		}(),
	} {
		if dec, err := Resolve(f, ""); err == nil || dec.HostedDev != nil {
			t.Errorf("%s: %+v, %v; want today's refusal", name, dec, err)
		}
	}
}

// With no record, an undeclared block is still off, and the refusal names the
// KCL that turns it on.
func TestResolveOffNamesTheKCLToAdd(t *testing.T) {
	_, err := Resolve(Facts{Env: "dev", HasLocal: true}, "")
	if err == nil {
		t.Fatal("an undeclared dev env resolved")
	}
	for _, want := range []string{
		"telemetry is off for dev: its observability is not declared",
		`observability = forge.Observability {dev_telemetry = "auto"}`, "control_plane", "forge env up dev",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
}

// The cloud backend sends its instance narrowing on every scope it builds:
// telemetry, issues and sessions.
func TestCloudSendsInstanceNarrowing(t *testing.T) {
	f := &fakeCaller{replies: map[string]string{procSearchLogs: `{}`, procListIssues: `{}`, procListSessions: `{}`}}
	c := cloudWith(f)
	c.Instances = Instances{IDs: []string{"sess-1"}, Users: []string{"u1"}}
	ctx := context.Background()
	n := narrowC()
	if _, err := c.Logs(ctx, LogsRequest{Narrow: n, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	n.Filters = nil
	if _, err := c.ListIssues(ctx, ListIssuesRequest{Narrow: n, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListSessions(ctx, ListSessionsRequest{Narrow: n, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	for _, proc := range []string{procSearchLogs, procListIssues, procListSessions} {
		scope, _ := f.reqs[proc]["scope"].(map[string]any)
		if fmt.Sprint(scope["instanceIds"]) != "[sess-1]" || fmt.Sprint(scope["instanceUsers"]) != "[u1]" || fmt.Sprint(scope["envIds"]) != "[env-1]" {
			t.Errorf("%s scope = %v", proc, scope)
		}
	}

	// A whole-env read sends neither field: the request every control plane knew.
	f = &fakeCaller{replies: map[string]string{procSearchLogs: `{}`}}
	if _, err := cloudWith(f).Logs(ctx, LogsRequest{Narrow: narrowC(), Limit: 5}); err != nil {
		t.Fatal(err)
	}
	scope := f.reqs[procSearchLogs]["scope"].(map[string]any)
	if _, ok := scope["instanceIds"]; ok {
		t.Errorf("an unnarrowed scope sent instanceIds: %v", scope)
	}
	if _, ok := scope["instanceUsers"]; ok {
		t.Errorf("an unnarrowed scope sent instanceUsers: %v", scope)
	}
}

// The local backend narrows its compiled statements the same way, and the
// cloud backend's --explain shows the narrowing it will ask for.
func TestLocalAndExplainNarrowByInstance(t *testing.T) {
	fr := &fakeRunner{}
	l := Local{Env: "dev", Runner: fr, Instances: Instances{IDs: []string{"sess-1"}}}
	ctx := context.Background()
	if _, err := l.Logs(ctx, LogsRequest{Narrow: rng(), Limit: 5}); err != nil {
		t.Fatal(err)
	}
	// No spans answer: the trace read still compiled, narrowed, before it said so.
	_, _ = l.Trace(ctx, TraceRequest{Narrow: rng(), TraceID: "ab", LogLimit: 5})
	if _, err := l.ListIssues(ctx, ListIssuesRequest{Narrow: rng(), Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if len(fr.ran) < 3 {
		t.Fatalf("ran %d statements", len(fr.ran))
	}
	for _, st := range fr.ran {
		if !strings.Contains(st.SQL, "ResourceAttributes['reliant.instance.id']") {
			t.Errorf("a local statement is not narrowed to the instance:\n%s", st.SQL)
		}
	}

	c := cloudWith(&fakeCaller{})
	c.Instances = Instances{IDs: []string{"sess-1"}}
	n := narrowC()
	n.Explain = true
	res, err := c.Logs(ctx, LogsRequest{Narrow: n, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Statements) == 0 || !strings.Contains(res.Statements[0].SQL, "reliant.instance.id") {
		t.Errorf("--explain on a cloud env omits the instance: %+v", res.Statements)
	}
}
