package cli

// One page of an environment's promotion ledger, newest first — the read
// `forge env status --history` is built on (hosted-deploy-primitives §3.5).
//
// An optional capability of a binding store, declared here at its consumer:
// the two backends answer it from what they already hold (the hosted one from
// ListPromotions with C7's keyset cursor, the file one from its jsonl), and
// neither grows a method the other callers of bindingStore never need.

import (
	"context"
	"errors"
	"fmt"

	"github.com/reliant-labs/forge/pkg/release"
)

// historyQuery is one page request.
type historyQuery struct {
	// Limit caps the page. Zero is defaultHistoryLimit.
	Limit int
	// Before is a KEYSET cursor: return entries strictly older than this
	// promotion id. Take it from the previous page's Next. Keyset, not an
	// offset, because the ledger is append-only and read newest-first — a
	// promote during paging would shift every offset by one.
	Before string
	// Release keeps only promotions of this release version.
	Release string
}

// historyPage is one page of promotions, newest first.
type historyPage struct {
	Promotions []release.Promotion
	// Next is the cursor for the following page; EMPTY MEANS THE LAST PAGE.
	// A reader stops on the empty cursor, not on a short page, because a
	// page can legitimately come back short.
	Next string
}

const (
	defaultHistoryLimit = 20
	// maxHistoryLimit mirrors the control plane's own page cap, so the two
	// backends refuse the same requests rather than the file one quietly
	// serving more.
	maxHistoryLimit = 500
)

// errHistoryQueryInvalid marks a request no re-read can fix: a --limit out of
// range, or a --before id that names no promotion of this env. The cursor
// case is refused rather than read as "from the start", because a pipeline
// paging with a typo would otherwise loop over page one forever. A sentinel,
// so the verb classifies it with errors.Is rather than by message text.
var errHistoryQueryInvalid = errors.New("invalid history query")

// bindingHistoryReader is the optional history capability.
type bindingHistoryReader interface {
	HistoryPage(ctx context.Context, env string, q historyQuery) (historyPage, error)
}

func (q historyQuery) limit() (int, error) {
	switch {
	case q.Limit == 0:
		return defaultHistoryLimit, nil
	case q.Limit < 0 || q.Limit > maxHistoryLimit:
		return 0, fmt.Errorf("%w: --limit must be between 1 and %d, got %d", errHistoryQueryInvalid, maxHistoryLimit, q.Limit)
	default:
		return q.Limit, nil
	}
}

// HistoryPage serves the file ledger's history. The log is oldest-first and
// small, so the page is cut in memory with the SAME semantics the server
// applies: newest first, strictly before the cursor, optionally one release.
func (s fileBindingStore) HistoryPage(_ context.Context, env string, q historyQuery) (historyPage, error) {
	limit, err := q.limit()
	if err != nil {
		return historyPage{}, err
	}
	all, err := s.History(env)
	if err != nil {
		return historyPage{}, err
	}
	// Newest first.
	newestFirst := make([]release.Promotion, len(all))
	for i := range all {
		newestFirst[len(all)-1-i] = all[i]
	}
	start := 0
	if q.Before != "" {
		start = -1
		for i, p := range newestFirst {
			if p.ID == q.Before {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return historyPage{}, fmt.Errorf("%w: --before %q names no promotion of %s", errHistoryQueryInvalid, q.Before, env)
		}
	}
	var page historyPage
	for i := start; i < len(newestFirst); i++ {
		p := newestFirst[i]
		if q.Release != "" && p.Release != q.Release {
			continue
		}
		if len(page.Promotions) == limit {
			// There is at least one more matching entry, so this page
			// is not the last: the cursor is the last entry SERVED.
			page.Next = page.Promotions[len(page.Promotions)-1].ID
			break
		}
		page.Promotions = append(page.Promotions, p)
	}
	return page, nil
}

// HistoryPage serves the hosted ledger's history through ListPromotions, with
// C7's cursor and version filter. An environment the control plane has never
// heard of has, by definition, no history.
func (s *hostedStore) HistoryPage(ctx context.Context, env string, q historyQuery) (historyPage, error) {
	limit, err := q.limit()
	if err != nil {
		return historyPage{}, err
	}
	id, err := s.envID(ctx, env)
	if errors.Is(err, errHostedEnvNotFound) {
		return historyPage{}, nil
	}
	if err != nil {
		return historyPage{}, err
	}
	req := map[string]any{"environmentId": id, "limit": limit}
	if q.Before != "" {
		req["beforePromotionId"] = q.Before
	}
	if q.Release != "" {
		req["releaseVersion"] = q.Release
	}
	var resp struct {
		Promotions []wirePromotion `json:"promotions"`
		Next       string          `json:"nextBeforePromotionId"`
	}
	if err := s.client.Call(ctx, procListPromotions, req, &resp); err != nil {
		return historyPage{}, err
	}
	page := historyPage{Next: resp.Next, Promotions: make([]release.Promotion, 0, len(resp.Promotions))}
	for _, w := range resp.Promotions {
		p, err := s.promotionFromWire(env, w)
		if err != nil {
			return historyPage{}, err
		}
		page.Promotions = append(page.Promotions, p)
	}
	return page, nil
}

// gatesSummary is the one-line count of a promotion's evidence, both halves:
// what was claimed at promote time and what was recorded afterwards.
type gatesSummary struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	Errored int `json:"errored"`
}

func summarizeGates(groups ...[]release.Gate) *gatesSummary {
	var s gatesSummary
	n := 0
	for _, gates := range groups {
		for _, g := range gates {
			n++
			switch release.StageStatusOfGate(g) {
			case release.StagePassed:
				s.Passed++
			case release.StageFailed:
				s.Failed++
			case release.StageSkipped:
				s.Skipped++
			default:
				s.Errored++
			}
		}
	}
	if n == 0 {
		return nil
	}
	return &s
}
