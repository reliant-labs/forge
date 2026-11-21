package testflakes

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/testreport"
)

// fakeGH serves issue lists and records every write.
type fakeGH struct {
	open   string            // `issue list --state open` JSON
	search map[string]string // test name -> `issue list --state all` JSON
	writes []string
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(joined, "issue list") && hasArg(args, "open"):
		return []byte(f.open), nil
	case strings.HasPrefix(joined, "issue list"):
		for name, js := range f.search {
			if strings.Contains(argAfter(args, "--search"), name) {
				return []byte(js), nil
			}
		}
		return []byte("[]"), nil
	case strings.HasPrefix(joined, "label create"):
		return nil, nil
	case strings.HasPrefix(joined, "issue create"):
		f.writes = append(f.writes, joined)
		return []byte("https://github.com/acme/m/issues/99\n"), nil
	}
	f.writes = append(f.writes, joined)
	return nil, nil
}

func flaky(name string, st testreport.State, runs ...string) testreport.FlakyTest {
	ft := testreport.FlakyTest{ID: testreport.TestID{Lang: testreport.LangGo, Package: testModule + "/internal/a", Name: name}, State: st, N: 10}
	for i, r := range runs {
		ft.Events = append(ft.Events, testreport.FlakeEvent{Test: ft.ID, Run: r, Commit: "c" + r, Lane: "unit", Signal: testreport.SignalRetryPass, Verdict: testreport.VerdictConfirmed, Signature: "sig", At: day0.Add(time.Duration(i) * time.Hour)})
	}
	return ft
}

func TestSyncIssues(t *testing.T) {
	root := newModule(t, map[string]string{
		"internal/a/a_test.go": "package a\n\nfunc TestNew(t *testing.T) {\n\tif x() {\n\t\tt.Fatal(\"boom\")\n\t}\n}\n\nfunc TestOther(t *testing.T) {}\n",
	})
	gh := &fakeGH{
		open: `[{"number":7,"title":"flake:suspected: TestHealed (internal/a)","state":"OPEN","url":"https://github.com/acme/m/issues/7"},
		        {"number":8,"title":"flake:suspected: TestUnknown (internal/a)","state":"OPEN","url":"https://github.com/acme/m/issues/8"}]`,
		search: map[string]string{
			"TestKnown": `[{"number":5,"title":"flake:suspected: TestKnown (internal/a)","state":"CLOSED","url":"https://github.com/acme/m/issues/5","body":"run ` + "`r1`" + `","comments":[]}]`,
		},
	}
	doc := &testreport.Flakes{Tests: []testreport.FlakyTest{
		flaky("TestNew", testreport.StateSuspected, "r1"),
		flaky("TestKnown", testreport.StateFlaky, "r1", "r2"),
		flaky("TestHealed", testreport.StateHealthy, "r0"),
	}}
	out := &bytes.Buffer{}
	res := SyncIssues(context.Background(), IssueOptions{Root: root, Repo: "acme/m", GH: gh.run, Stdout: out, Getenv: noEnv}, doc)

	writes := strings.Join(gh.writes, "\n")
	if len(res.Opened) != 1 || !strings.Contains(writes, "issue create -R acme/m --title flake:suspected: TestNew (internal/a) --label flake:suspected") {
		t.Errorf("open: %v\n%s", res.Opened, writes)
	}
	// The body carries the test's source, the signature and the runs.
	for _, want := range []string{"func TestNew(t *testing.T) {", "t.Fatal(\"boom\")", "`sig`", "run `r1`"} {
		if !strings.Contains(writes, want) {
			t.Errorf("issue body lacks %q", want)
		}
	}
	if strings.Contains(writes, "func TestOther") {
		t.Error("the body must stop at the test's closing brace")
	}
	// The closed issue is reopened, and only the run it does not mention is
	// added.
	if !strings.Contains(writes, "issue reopen 5 -R acme/m") || !strings.Contains(writes, "issue comment 5") ||
		!strings.Contains(writes, "run `r2`") {
		t.Errorf("update:\n%s", writes)
	}
	for _, w := range gh.writes {
		if strings.HasPrefix(w, "issue comment 5") && strings.Contains(w, "run `r1`") {
			t.Errorf("an already-mentioned run is not repeated: %s", w)
		}
	}
	// A test that left the state has its issue closed; one the record does
	// not know is left alone.
	if !strings.Contains(writes, "issue close 7 -R acme/m --reason not planned") || strings.Contains(writes, "issue close 8") {
		t.Errorf("close:\n%s", writes)
	}
	if res.URLs[doc.Tests[0].ID.String()] != "https://github.com/acme/m/issues/99" {
		t.Errorf("urls = %v", res.URLs)
	}
}

func TestSyncIssuesDryRunWritesNothing(t *testing.T) {
	gh := &fakeGH{open: `[{"number":7,"title":"flake:suspected: TestHealed (internal/a)","state":"OPEN"}]`}
	doc := &testreport.Flakes{Tests: []testreport.FlakyTest{flaky("TestNew", testreport.StateSuspected, "r1"), flaky("TestHealed", testreport.StateFixed, "r0")}}
	res := SyncIssues(context.Background(), IssueOptions{Root: newModule(t, nil), Repo: "acme/m", GH: gh.run, Stdout: &bytes.Buffer{}, DryRun: true}, doc)
	if len(gh.writes) != 0 || len(res.Messages) != 2 {
		t.Errorf("writes %v, messages %v", gh.writes, res.Messages)
	}
}
