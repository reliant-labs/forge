package cli

// `forge obs check`'s AI findings (pkg/releasehealth ai.go): the release's
// GenAI model and tool calls, per service, before and after the promotion,
// read by this CLI from either backend with the same panels
// (obsbackend.AIReader) and judged by releasehealth.WithAI. Spans only; all
// three findings warn unless the env's fail_on names them.
//
// With a release in the subject (a hosted promotion), the after window is
// read at that release's service.version, so the calls an old version still
// in rotation makes are not the release's evidence; the before window is
// read whole, as the service figures are.

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/internal/obsout"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

// readAIHealth reads the report's two windows' AI calls. note, when set, is
// the not_evaluated entry that says why there are none.
func readAIHealth(ctx context.Context, b obsbackend.Backend, r releasehealth.Report) (out []releasehealth.AIHealth, note string) {
	reader, ok := b.(obsbackend.AIReader)
	if !ok {
		return nil, fmt.Sprintf("%s the %s backend cannot read AI calls", releasehealth.NotEvaluatedAI, b.Info().Backend)
	}
	read := func(w releasehealth.Range, version string) ([]obsbackend.AIServiceCalls, error) {
		if w.To.Sub(w.From) < time.Second {
			return nil, nil // a window too short to compile has no calls to count
		}
		res, err := reader.AI(ctx, obsbackend.AIRequest{Narrow: obsbackend.Narrow{Range: obsbackend.Range{From: w.From, To: w.To}}, Version: version})
		return res.Services, err
	}
	before, err := read(r.Before, "")
	if err == nil {
		var after []obsbackend.AIServiceCalls
		if after, err = read(r.After, r.Subject.Release); err == nil {
			return obsbackend.JoinAIHealth(before, after, r.Subject.Release), ""
		}
	}
	return nil, fmt.Sprintf("%s the AI calls could not be read: %v", releasehealth.NotEvaluatedAI, err)
}

// checkAI applies the AI rule to a check's report.
type checkAI struct {
	b obsbackend.Backend
}

// apply judges the report's AI calls. overrides are the request's
// --threshold pairs. When the calls cannot be read, a report that carries
// them (a control plane that judges them) is held to the request's
// thresholds, and one that carries none says why in not_evaluated.
func (c *checkAI) apply(ctx context.Context, r releasehealth.Report, overrides map[string]string) releasehealth.Report {
	hs, note := readAIHealth(ctx, c.b, r)
	if hs == nil {
		if len(r.AI) > 0 {
			return releasehealth.WithAI(r, nil, overrides)
		}
		r = releasehealth.WithAI(r, []releasehealth.AIHealth{}, overrides)
		if note != "" {
			r.NotEvaluated = append(slices.Clone(r.NotEvaluated), note)
		}
		return r
	}
	return releasehealth.WithAI(r, hs, overrides)
}

// writeAIText is the AI table of `forge obs check`'s text output: each
// service's model and tool calls, failures and model-call p95.
func writeAIText(w io.Writer, hs []releasehealth.AIHealth) {
	rows := make([][]string, len(hs))
	for i, h := range hs {
		b, a := h.Before.All(), h.After.All()
		rows[i] = []string{h.Service,
			fmt.Sprintf("%d -> %d", b.Calls, a.Calls),
			fmt.Sprintf("%.1f%% -> %.1f%%", b.ErrorRate()*100, a.ErrorRate()*100),
			fmt.Sprintf("%.0fms -> %.0fms", h.Before.ModelP95Ms, h.After.ModelP95Ms),
			fmt.Sprint(len(h.Tools))}
	}
	fmt.Fprintln(w)
	obsout.Table(w, []string{"AI SERVICE", "CALLS", "FAILED", "MODEL P95", "TOOLS"}, rows, 40)
}
