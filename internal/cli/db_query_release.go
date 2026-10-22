package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/reliant-labs/forge/internal/releasetimeline"
)

// dbQueryRelease is the release that was live in the env when a query ran:
// what lets an agent line rows up with the telemetry and the change history
// (`forge env releases at`, `forge obs issue`).
type dbQueryRelease struct {
	Version     string    `json:"version"`
	PromotionID string    `json:"promotion_id,omitempty"`
	PromotedAt  time.Time `json:"promoted_at"`
	// Unsettled: the promotion's rollout had not finished, so the env may
	// still have been serving the previous release.
	Unsettled bool `json:"unsettled,omitempty"`
}

// dbQueryReleaseTimeout bounds the release lookup. It is context for the
// rows, never a reason to withhold them.
const dbQueryReleaseTimeout = 10 * time.Second

// liveReleaseAt reads env's release ledger (the machine ledger or the
// control plane, as `forge env releases` does) and returns the release live at
// at. A nil release with a nil error means nothing was promoted yet (a dev
// env that only ever ran `env up`). A variable so a test can substitute it.
var liveReleaseAt = func(ctx context.Context, env string, at time.Time) (*dbQueryRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, dbQueryReleaseTimeout)
	defer cancel()
	b, err := openReleasesBackend(ctx, env)
	if err != nil {
		return nil, err
	}
	d, err := b.Source.Load(ctx, env, time.Time{})
	if err != nil {
		return nil, err
	}
	return releaseFromAt(releasetimeline.Assemble(d, time.Time{}, 0).At(at)), nil
}

// releaseFromAt projects the live promotion of an At answer.
func releaseFromAt(r releasetimeline.AtResult) *dbQueryRelease {
	if r.Live == nil {
		return nil
	}
	return &dbQueryRelease{Version: r.Live.Version, PromotionID: r.Live.PromotionID, PromotedAt: r.Live.At.UTC(), Unsettled: r.Unsettled}
}

// stampRelease looks the live release up, warning on errOut (never failing
// the query) when the ledger cannot be read.
func stampRelease(ctx context.Context, errOut io.Writer, env string, at time.Time) *dbQueryRelease {
	rel, err := liveReleaseAt(ctx, env, at)
	if err != nil {
		fmt.Fprintf(errOut, "warning: the release live in %s is unknown: %v\n", env, err)
		return nil
	}
	return rel
}
