package obsbackend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
)

const fullIssueID = "484169c0-ab61-4c24-baf4-889e8d3e3046"

// prefixIssuesCP serves GetIssue for full ids only, as the control plane
// does, and lists the env's issues.
type prefixIssuesCP struct {
	ids  []string
	gets []string
	// endless answers every ListIssues page with a next token, as an env
	// with more issues than the scan reads.
	endless bool
	// sources maps an issue source to its issues' ids. Nil is a control
	// plane that predates sources: every list answers the error issues.
	sources map[string][]string
}

func (p *prefixIssuesCP) Call(_ context.Context, proc string, req, out any) error {
	switch {
	case strings.HasSuffix(proc, "/GetIssue"):
		b, _ := json.Marshal(req)
		var in struct {
			IssueID string `json:"issueId"`
		}
		_ = json.Unmarshal(b, &in)
		p.gets = append(p.gets, in.IssueID)
		all := append([]string(nil), p.ids...)
		for _, ids := range p.sources {
			all = append(all, ids...)
		}
		for _, id := range all {
			if id == in.IssueID {
				return json.Unmarshal([]byte(`{"issue":{"id":"`+id+`","envId":"env-1","status":"ISSUE_STATUS_NEW"}}`), out)
			}
		}
		return &cloud.Error{Code: cloud.CodeNotFound, Message: "issue not found"}
	case strings.HasSuffix(proc, "/ListIssues"):
		b, _ := json.Marshal(req)
		var in struct {
			Source string `json:"source"`
		}
		_ = json.Unmarshal(b, &in)
		ids, source := p.ids, ""
		if in.Source != "" && p.sources != nil {
			ids, source = p.sources[in.Source], in.Source
		}
		var rows []string
		for _, id := range ids {
			rows = append(rows, `{"id":"`+id+`","envId":"env-1","source":"`+source+`"}`)
		}
		next := ""
		if p.endless {
			next = "more"
		}
		return json.Unmarshal([]byte(`{"issues":[`+strings.Join(rows, ",")+`],"nextPageToken":"`+next+`"}`), out)
	}
	return &cloud.Error{Code: cloud.CodeUnimplemented, Message: proc}
}

func prefixNarrow() Narrow {
	to := time.Date(2026, 11, 25, 18, 0, 0, 0, time.UTC)
	return Narrow{Range: Range{From: to.Add(-72 * time.Hour), To: to}}
}

// `forge obs issue 484169c0 prod`: the 8-character id a table shows resolves
// to the one issue it prefixes (obs d42c4b4c).
func TestCloudGetIssueResolvesAUniquePrefix(t *testing.T) {
	cp := &prefixIssuesCP{ids: []string{fullIssueID, "99999999-0000-4000-8000-000000000000"}}
	res, err := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: cp}.GetIssue(context.Background(), GetIssueRequest{Narrow: prefixNarrow(), ID: "484169c0"})
	if err != nil || res.Issue.ID != fullIssueID {
		t.Fatalf("GetIssue(prefix) = %+v, %v; want the full issue", res.Issue, err)
	}
	if got := cp.gets[len(cp.gets)-1]; got != fullIssueID {
		t.Errorf("last GetIssue asked for %q, want the full id", got)
	}
}

// `forge obs issue c40fad01 prod` for a bug report: the issues of a source are
// never in the errors' list, so a short id is resolved against the
// bug-report sources too, and an ambiguity across sources is still refused.
func TestCloudGetIssueResolvesABugReportPrefix(t *testing.T) {
	const report = "c40fad01-a80e-4828-a6c8-db3a22d1dadf"
	cp := &prefixIssuesCP{ids: []string{fullIssueID}, sources: map[string][]string{"forge_cli_report": {report}}}
	res, err := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: cp}.GetIssue(context.Background(), GetIssueRequest{Narrow: prefixNarrow(), ID: "c40fad01"})
	if err != nil || res.Issue.ID != report {
		t.Fatalf("GetIssue(bug-report prefix) = %+v, %v; want the report's issue", res.Issue, err)
	}
	cp = &prefixIssuesCP{ids: []string{fullIssueID}, sources: map[string][]string{"llm_bug_report": {"48416900-0000-4000-8000-000000000000"}}}
	_, err = Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: cp}.GetIssue(context.Background(), GetIssueRequest{Narrow: prefixNarrow(), ID: "4841"})
	if err == nil || !strings.Contains(err.Error(), "give more of the id") {
		t.Fatalf("an ambiguity across sources = %v, want a refusal", err)
	}
}

func TestCloudGetIssuePrefixRefusals(t *testing.T) {
	cases := map[string]struct {
		ids  []string
		id   string
		want string
	}{
		"no match":         {[]string{fullIssueID}, "deadbeef", "use the full id from `forge obs issues prod`"},
		"ambiguous prefix": {[]string{fullIssueID, "48416900-0000-4000-8000-000000000000"}, "4841", "give more of the id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: &prefixIssuesCP{ids: tc.ids}}.GetIssue(context.Background(), GetIssueRequest{Narrow: prefixNarrow(), ID: tc.id})
			if err == nil || !strings.Contains(err.Error(), tc.want) || exitOf(err) != ExitWrong {
				t.Fatalf("err = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
	// A scan that hits its page cap with issues unread proves no uniqueness:
	// one match so far is refused, not resolved.
	_, err := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: &prefixIssuesCP{ids: []string{fullIssueID}, endless: true}}.GetIssue(context.Background(), GetIssueRequest{Narrow: prefixNarrow(), ID: "484169c0"})
	if err == nil || !strings.Contains(err.Error(), "could not be resolved to exactly one") || exitOf(err) != ExitWrong {
		t.Errorf("capped scan = %v, want a refusal", err)
	}
	// A full id that is not found stays the control plane's NotFound.
	_, err = Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: &prefixIssuesCP{}}.GetIssue(context.Background(), GetIssueRequest{Narrow: prefixNarrow(), ID: fullIssueID})
	if !isCloudNotFound(err) {
		t.Errorf("full-id miss = %v, want the NotFound unchanged", err)
	}
}
