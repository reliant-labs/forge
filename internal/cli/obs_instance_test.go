package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/devtelemetry"
	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/pkg/release"
)

const testSession = "sess-0123456789abcdef01234567"

func hostedDevRec() devtelemetry.Record {
	return devtelemetry.Record{Env: "dev", Backend: "cloud", OrgID: "org-1", EnvironmentID: "env-dev",
		InstanceID: testSession, InstanceLabel: "alice/fix-login", InstanceUser: "alice@x"}
}

// obsRunHostedDev runs `forge obs <args>` against a dev env whose `forge env
// up` recorded the hosted store, with caller as its control plane.
func obsRunHostedDev(t *testing.T, caller *fakeCloudCaller, sessions []release.LocalSession, args ...string) (string, error) {
	t.Helper()
	rec := hostedDevRec()
	var out, errb bytes.Buffer
	old := obsDepsOverride
	t.Cleanup(func() { obsDepsOverride = old })
	obsDepsOverride = &obsDeps{
		Facts: func(context.Context, io.Writer, string) (obsbackend.Facts, error) {
			return obsbackend.Facts{Env: "dev", HasLocal: true, ControlPlane: "https://cp.example.com", DevTelemetry: &rec}, nil
		},
		Local: func(env string) obsbackend.Backend { return obsbackend.Local{Env: env, Runner: &scriptedRunner{}} },
		Cloud: func(context.Context, string) (obsbackend.Cloud, error) {
			t.Fatal("a hosted-dev env must not look up a hosted environment")
			return obsbackend.Cloud{}, nil
		},
		HostedDev: func(_ context.Context, env string, r devtelemetry.Record) (obsbackend.Cloud, error) {
			return obsbackend.Cloud{Env: env, EnvID: r.EnvironmentID, OrgID: r.OrgID, Caller: caller}, nil
		},
		Instances: func(obsbackend.Caller) instanceLister {
			return func(context.Context, string, string) ([]release.LocalSession, error) { return sessions, nil }
		},
		Now:     func() time.Time { return obsNow },
		Release: func(context.Context, string, time.Time) (*liveRelease, error) { return nil, nil },
	}
	cmd := newTestRootCmd(t)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{"obs"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func lastScope(t *testing.T, c *fakeCloudCaller) map[string]any {
	t.Helper()
	if len(c.calls) == 0 {
		t.Fatal("nothing reached the control plane")
	}
	m := decode(t, c.calls[len(c.calls)-1].body)
	s, _ := m["scope"].(map[string]any)
	return s
}

// A hosted-dev env reads THIS checkout's instance by default, bound to the
// record's org and environment, and says so in scope.instance.
func TestObsHostedDevDefaultsToTheRecordedInstance(t *testing.T) {
	c := &fakeCloudCaller{replies: map[string]string{"controlplane.v1.ObservabilityService/SearchLogs": `{}`}}
	out, err := obsRunHostedDev(t, c, nil, "logs", "dev", "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	scope := lastScope(t, c)
	if scope["orgId"] != "org-1" || jsonList(scope["envIds"]) != "env-dev" || jsonList(scope["instanceIds"]) != testSession {
		t.Errorf("scope sent = %v", scope)
	}
	doc := decode(t, out)
	ds := doc["scope"].(map[string]any)
	in, _ := ds["instance"].(map[string]any)
	if ds["backend"] != "cloud" || in["id"] != testSession || in["label"] != "alice/fix-login" || in["user"] != "alice@x" {
		t.Errorf("document scope = %v", ds)
	}
}

func TestObsHostedDevInstanceFlags(t *testing.T) {
	logs := map[string]string{"controlplane.v1.ObservabilityService/SearchLogs": `{}`}
	sessions := []release.LocalSession{
		{ID: "sess-aaaaaaaaaaaaaaaaaaaaaaaa", Worktree: release.Worktree{Label: "fix-login"}},
		{ID: "sess-bbbbbbbbbbbbbbbbbbbbbbbb", Worktree: release.Worktree{Label: "main"}},
	}
	for name, c := range map[string]struct {
		args      []string
		instances string
		users     string
		docID     string
	}{
		"--all-instances reads the whole env": {[]string{"--all-instances"}, "", "", ""},
		"an id":                               {[]string{"--instance", "sess-cccccccccccccccccccccccc"}, "sess-cccccccccccccccccccccccc", "", "sess-cccccccccccccccccccccccc"},
		"the record's own label":              {[]string{"--instance", "alice/fix-login"}, testSession, "", testSession},
		"a teammate's label":                  {[]string{"--instance", "bob/main"}, "sess-bbbbbbbbbbbbbbbbbbbbbbbb", "", "sess-bbbbbbbbbbbbbbbbbbbbbbbb"},
		"a user widens the default":           {[]string{"--user", "bob@x"}, "", "bob@x", ""},
	} {
		t.Run(name, func(t *testing.T) {
			caller := &fakeCloudCaller{replies: logs}
			out, err := obsRunHostedDev(t, caller, sessions, append([]string{"logs", "dev", "--json"}, c.args...)...)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			scope := lastScope(t, caller)
			if jsonList(scope["instanceIds"]) != c.instances || jsonList(scope["instanceUsers"]) != c.users {
				t.Errorf("scope sent = %v", scope)
			}
			in, _ := decode(t, out)["scope"].(map[string]any)["instance"].(map[string]any)
			if c.instances == "" && c.users == "" {
				if in != nil {
					t.Errorf("a whole-env read names an instance: %v", in)
				}
				return
			}
			if id, _ := in["id"].(string); id != c.docID {
				t.Errorf("scope.instance = %v, want id %q", in, c.docID)
			}
		})
	}

	for name, args := range map[string][]string{
		"an unknown label":                  {"--instance", "nope"},
		"--all-instances with --instance":   {"--all-instances", "--instance", testSession},
		"an ambiguous label (two sessions)": {"--instance", "dup"},
	} {
		t.Run(name, func(t *testing.T) {
			dup := append(sessions,
				release.LocalSession{ID: "sess-dddddddddddddddddddddddd", Worktree: release.Worktree{Label: "dup"}},
				release.LocalSession{ID: "sess-eeeeeeeeeeeeeeeeeeeeeeee", Worktree: release.Worktree{Label: "dup"}})
			out, err := obsRunHostedDev(t, &fakeCloudCaller{replies: logs}, dup, append([]string{"logs", "dev", "--json"}, args...)...)
			if exitCodeForError(err) != 1 {
				t.Errorf("exit = %d, want 1: %v\n%s", exitCodeForError(err), err, out)
			}
		})
	}
}

// A non-default narrowing rides along on every suggested telemetry read.
func TestObsInstanceCarriesIntoSuggestions(t *testing.T) {
	f := obsInstanceFlags{instance: "sess-cccccccccccccccccccccccc"}
	rec := hostedDevRec()
	if _, err := f.resolve(context.Background(), "dev", &rec, nil); err != nil {
		t.Fatal(err)
	}
	got := f.carrySuggested([]string{
		"forge obs traces dev --errors", "forge obs changes dev", "forge env releases show dev v1", "forge obs query --help",
	})
	want := []string{
		"forge obs traces dev --errors --instance sess-cccccccccccccccccccccccc", "forge obs changes dev",
		"forge env releases show dev v1", "forge obs query --help",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("carried = %q", got)
	}

	def := obsInstanceFlags{}
	if _, err := def.resolve(context.Background(), "dev", &rec, nil); err != nil {
		t.Fatal(err)
	}
	if got := def.carrySuggested([]string{"forge obs traces dev"}); got[0] != "forge obs traces dev" {
		t.Errorf("the default instance is carried: %q", got)
	}

	// A label cannot be resolved without a control plane: the error says to
	// pass an id.
	lbl := obsInstanceFlags{instance: "fix-login"}
	if _, err := lbl.resolve(context.Background(), "dev", nil, nil); err == nil || !strings.Contains(err.Error(), "sess-") {
		t.Errorf("a label on a local backend: %v", err)
	}
}

func jsonList(v any) string {
	l, _ := v.([]any)
	s := make([]string, len(l))
	for i, x := range l {
		s[i], _ = x.(string)
	}
	return strings.Join(s, ",")
}

// `forge obs status dev` for a hosted-dev instance reads its freshness (the
// newest log and span) from the control plane, narrowed to the instance, and
// names the instance in its header.
func TestObsStatusHostedDevShowsFreshness(t *testing.T) {
	c := &fakeCloudCaller{replies: map[string]string{
		procSearchLogs: `{"logs":[{"timestamp":"2026-10-22T11:59:00Z","severityText":"INFO","service":"api","body":"ok"}]}`,
		procTraces:     `{"traces":[{"traceId":"t1","startTime":"2026-10-22T11:59:30Z","durationMs":12}]}`,
	}}
	out, _ := obsRunHostedDev(t, c, nil, "status", "dev", "--json")
	doc := decode(t, out)
	tel, _ := doc["telemetry"].(map[string]any)
	if tel["last_log_at"] != "2026-10-22T11:59:00Z" || tel["last_span_at"] != "2026-10-22T11:59:30Z" {
		t.Errorf("telemetry = %v\n%s", tel, out)
	}
	for _, proc := range []string{procSearchLogs, procTraces} {
		call := callTo(c.calls, proc)
		if call == nil || !strings.Contains(call.body, `"instanceIds":["`+testSession+`"]`) {
			t.Errorf("%s is not narrowed to the instance: %v", proc, call)
		}
	}
	if in, _ := doc["scope"].(map[string]any)["instance"].(map[string]any); in["id"] != testSession {
		t.Errorf("scope.instance = %v", in)
	}

	text, _ := obsRunHostedDev(t, c, nil, "status", "dev")
	if !strings.Contains(text, "instance alice/fix-login ("+testSession+")") || !strings.Contains(text, "dev (cloud backend)") {
		t.Errorf("status text:\n%s", text)
	}
}
