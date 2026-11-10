package cli

// `forge obs check`'s attribution of a judged release (pkg/releasehealth
// attribution.go): when the release actually went live, from the promotion's
// deploy gates, and which version each issue's after-window events carry. A
// control plane's answer is judged over the windows it planned, anchored at the
// promotion's first promoted_at; these reads are what keep a window that
// measured the release before it, or an issue of another version, from failing
// this one (forge#b7e1113e).

import (
	"context"
	"fmt"
	"time"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

// promotionGateReader reads the gates recorded on a promotion. The cloud
// backend implements it; a local env has no promotions.
type promotionGateReader interface {
	PromotionGates(ctx context.Context, promotionID string) ([]release.Gate, error)
}

// issueVersionReader reads one issue over a range, with its events by version.
type issueVersionReader interface {
	GetIssue(ctx context.Context, req obsbackend.GetIssueRequest) (obsbackend.GetIssueResult, error)
}

// checkLive is what a promotion's gates say about when its release went live.
type checkLive struct {
	at    time.Time
	state releasehealth.LiveState
	// read is false when nothing was read: no gate reader, no promotion id,
	// or the read failed (err says why).
	read bool
	err  error
}

// readCheckLive reads the judged promotion's gates, when the backend can.
func readCheckLive(ctx context.Context, b obsbackend.Backend, r releasehealth.Report) checkLive {
	gr, ok := b.(promotionGateReader)
	if !ok || r.Subject.PromotionID == "" {
		return checkLive{}
	}
	gates, err := gr.PromotionGates(ctx, r.Subject.PromotionID)
	if err != nil {
		return checkLive{err: err}
	}
	at, state := releasehealth.LiveAt(gates)
	return checkLive{at: at, state: state, read: true}
}

// maxAttributedIssues bounds the issue reads one check makes.
const maxAttributedIssues = 10

// attributeCheck applies the attribution rule to a judged report: the live
// time read from the promotion's gates, then each issue finding's after-window
// events by version. A read that fails leaves the report as judged on that
// point (a finding nothing disproves stays), and says so on stderr.
func attributeCheck(ctx context.Context, d obsDeps, b obsbackend.Backend, r releasehealth.Report, live checkLive) releasehealth.Report {
	if live.err != nil {
		fmt.Fprintf(d.Stderr, "could not read the deploy gates of promotion %s, so the after window is judged from its promoted_at: %v\n", r.Subject.PromotionID, live.err)
	}
	// Only a control plane's answer names a promoted release version to
	// attribute an issue's events to.
	ir, ok := b.(issueVersionReader)
	if ok && b.Info().Backend == obsbackend.BackendCloud && r.Subject.Release != "" && !r.After.From.IsZero() && r.After.To.After(r.After.From) {
		r.NewIssues, r.RegressedIssues = cloneIssues(r.NewIssues), cloneIssues(r.RegressedIssues)
		n := 0
		for _, list := range [][]releasehealth.Issue{r.NewIssues, r.RegressedIssues} {
			for i := range list {
				if list[i].WindowVersions != nil || list[i].ID == "" || n >= maxAttributedIssues {
					continue
				}
				n++
				res, err := ir.GetIssue(ctx, obsbackend.GetIssueRequest{
					Narrow: obsbackend.Narrow{Range: obsbackend.Range{From: r.After.From, To: r.After.To}}, ID: list[i].ID,
				})
				if err != nil {
					fmt.Fprintf(d.Stderr, "could not read issue %s's after-window events by version, so it is attributed to the release: %v\n", list[i].ID, err)
					continue
				}
				if len(res.ByVersion) == 0 {
					continue
				}
				list[i].WindowVersions = map[string]int64{}
				for _, v := range res.ByVersion {
					list[i].WindowVersions[v.Version] += v.Events
				}
			}
		}
	}
	return live.attribute(r)
}

// attribute applies the attribution rule with what the gates said, when they
// were read.
func (l checkLive) attribute(r releasehealth.Report) releasehealth.Report {
	if l.read {
		return releasehealth.WithLiveAt(r, l.at, l.state)
	}
	return releasehealth.Attribute(r)
}

func cloneIssues(in []releasehealth.Issue) []releasehealth.Issue {
	if in == nil {
		return nil
	}
	return append([]releasehealth.Issue(nil), in...)
}
