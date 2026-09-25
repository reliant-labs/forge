package release

import (
	"errors"
	"fmt"
	"time"
)

// ErrNeverPromoted is returned for a rollback to a release that never ran in
// the environment. Rolling "back" to something that never ran here is a
// promotion, and must be called one.
var ErrNeverPromoted = errors.New("release was never promoted to this environment")

// PromotionKind records INTENT. Both kinds are mechanically identical — a
// new ledger entry naming an already-cut release — but a rollback must read
// as a rollback in the audit trail. Closed.
type PromotionKind string

const (
	// KindPromote advances an environment to a release.
	KindPromote PromotionKind = "promote"
	// KindRollback returns an environment to a release it already ran.
	KindRollback PromotionKind = "rollback"
)

// Valid reports whether k is promote or rollback.
func (k PromotionKind) Valid() bool { return k == KindPromote || k == KindRollback }

// UnmarshalJSON refuses an empty or unknown kind.
func (k *PromotionKind) UnmarshalJSON(data []byte) error {
	return decodeClosed(data, "promotion kind", func(s string) bool { return PromotionKind(s).Valid() },
		[]string{string(KindPromote), string(KindRollback)}, (*string)(k))
}

// Gate is one check that had passed when a promotion was recorded. EVIDENCE,
// not enforcement: a gate that must block a promotion is checked before the
// entry is written, because a recorded claim proves only that it was made.
type Gate struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	URL    string `json:"url,omitempty"`
}

// Actor is who recorded a promotion: a human user, or a named automation
// ("ci", "preview-bot"). A file ledger records whatever the caller states.
type Actor struct {
	User  string `json:"user,omitempty"`
	Actor string `json:"actor,omitempty"`
}

// Promotion is ONE append-only ledger entry: environment Env ran release
// Release from At onward. An environment's CURRENT binding is simply its
// most recent entry; there is no separate pointer that could disagree with
// the history.
type Promotion struct {
	// ID is assigned by the backend that recorded the entry.
	ID string `json:"id,omitempty"`
	// Env is the environment name.
	Env string `json:"env"`
	// Release is the version label bound.
	Release string        `json:"release"`
	Kind    PromotionKind `json:"kind"`
	// FromEnv is the environment this release was promoted FROM, or empty
	// for a first deploy or a rollback. Makes the promotion PATH auditable.
	FromEnv string `json:"from_env,omitempty"`
	// Resolved is the image → digest pin set FROZEN at promote time. A
	// deploy pins exactly these; nothing re-reads the release later.
	Resolved map[string]string `json:"resolved"`
	// Sources is the source-built half of the same snapshot.
	Sources    map[string]Source `json:"sources,omitempty"`
	PromotedBy Actor             `json:"promoted_by,omitempty"`
	Gates      []Gate            `json:"gates,omitempty"`
	// Note is the free-form "why" — the most valuable field on a rollback.
	Note string `json:"note,omitempty"`
	// PromotedAt is when the entry was written. It is PROMOTE time, not
	// deploy time: a promotion moves no bytes.
	PromotedAt time.Time `json:"promoted_at"`
}

// Validate checks a promotion before it is appended, and every entry read
// back.
func (p Promotion) Validate() error {
	switch {
	case p.Env == "":
		return fmt.Errorf("%w: promotion environment is required", ErrInvalid)
	case p.Release == "":
		return fmt.Errorf("%w: promotion release is required", ErrInvalid)
	case !p.Kind.Valid():
		return fmt.Errorf("%w: promotion kind %q (expected promote or rollback)", ErrInvalid, p.Kind)
	case p.FromEnv != "" && p.FromEnv == p.Env:
		return fmt.Errorf("%w: environment %q cannot be promoted from itself", ErrInvalid, p.Env)
	}
	for image, d := range p.Resolved {
		if !ValidDigest(d) {
			return fmt.Errorf("%w: promotion of %q: resolved digest %q for %q is not canonical", ErrInvalid, p.Env, d, image)
		}
	}
	return nil
}

// NewPromotion freezes a release's pin set into a promotion of env. The
// digests are resolved HERE, at promote time, so a later edit to the release
// cannot change what this promotion ships.
func NewPromotion(env string, r Release, kind PromotionKind) Promotion {
	return Promotion{
		Env:      env,
		Release:  r.Version,
		Kind:     kind,
		Resolved: r.SharedDigests(),
		Sources:  r.Sources(),
	}
}

// Decide applies the ledger's append rules to one environment's history
// (OLDEST FIRST) and a requested entry. It returns:
//
//   - (current, nil) when the request is already the current state — the
//     same release with the same kind. A CI retry must not append a second
//     entry claiming the environment moved where it already was. The key
//     is the CURRENT entry, not "ever promoted": v1→v2→v1 is three real
//     moves, and the third must be recorded.
//   - (nil, ErrNeverPromoted) for a rollback to a release absent from the
//     history.
//   - (nil, nil) when the entry should be appended.
//
// Both backends call this inside whatever serializes their appends (the
// hosted one under a row lock), so the rule has one implementation.
func Decide(history []Promotion, requested Promotion) (*Promotion, error) {
	if n := len(history); n > 0 {
		current := history[n-1]
		if current.Release == requested.Release && current.Kind == requested.Kind {
			return &current, nil
		}
	}
	if requested.Kind == KindRollback {
		for _, p := range history {
			if p.Release == requested.Release {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("rollback of %q to %q: %w", requested.Env, requested.Release, ErrNeverPromoted)
	}
	return nil, nil
}
