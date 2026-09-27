package release

import (
	"encoding/json"
	"fmt"
	"time"
)

// PromotionKind records what a ledger entry is. There is ONE kind: a
// promotion binds an environment to a release. Moving an environment to an
// OLDER release is still a promotion — the plan labels its direction
// `behind` — because there is no such thing as undoing a release: the newer
// one already wrote data and ran migrations. Recovery is roll forward.
//
// The field survives because it is the ledger's wire format, and an
// explicit kind keeps a future entry type from being read as a promotion.
type PromotionKind string

const (
	// KindPromote binds an environment to a release.
	KindPromote PromotionKind = "promote"

	// legacyKindRollback is what the retired promote-as-rollback flag
	// recorded before rollback was removed. It is READ ONLY: existing ledgers still
	// carry it, and an entry that bound an env to a release is a promotion
	// of that release whatever intent it was labelled with. It decodes as
	// KindPromote and can never be written.
	legacyKindRollback = "rollback"
)

// Valid reports whether k is a kind a ledger entry may carry.
func (k PromotionKind) Valid() bool { return k == KindPromote }

// ParsePromotionKind reads a STORED kind — a ledger line, a database row —
// into the kind it means today. It is the one place the retired "rollback"
// kind is recognised, so every backend that holds old entries reads them the
// same way: as the promote of their release. Unknown and empty are refused,
// never defaulted — a kind nobody recognises must not read as a binding.
func ParsePromotionKind(stored string) (PromotionKind, error) {
	if stored == legacyKindRollback {
		return KindPromote, nil
	}
	if k := PromotionKind(stored); k.Valid() {
		return k, nil
	}
	return "", fmt.Errorf("%w: unknown promotion kind %q (expected %s)", ErrInvalid, stored, KindPromote)
}

// UnmarshalJSON decodes through ParsePromotionKind.
func (k *PromotionKind) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: promotion kind must be a string: %v", ErrInvalid, err)
	}
	parsed, err := ParsePromotionKind(raw)
	if err != nil {
		return err
	}
	*k = parsed
	return nil
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
	// for a first deploy or a direct promote. Makes the promotion PATH auditable.
	FromEnv string `json:"from_env,omitempty"`
	// Resolved is the image → digest pin set FROZEN at promote time. A
	// deploy pins exactly these; nothing re-reads the release later.
	Resolved map[string]string `json:"resolved"`
	// Sources is the source-built half of the same snapshot.
	Sources    map[string]Source `json:"sources,omitempty"`
	PromotedBy Actor             `json:"promoted_by,omitempty"`
	Gates      []Gate            `json:"gates,omitempty"`
	// Note is the free-form "why" — the most valuable field on a promote
	// that moves an environment backwards.
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
		return fmt.Errorf("%w: promotion kind %q (expected promote)", ErrInvalid, p.Kind)
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

// Decide applies the ledger's append rule to one environment's history
// (OLDEST FIRST) and a requested entry. It returns:
//
//   - (current, nil) when the request is already the current state — the
//     same release. A CI retry must not append a second entry claiming the
//     environment moved where it already was. The key is the CURRENT
//     entry, not "ever promoted": v1→v2→v1 is three real moves, and the
//     third must be recorded.
//   - (nil, nil) when the entry should be appended.
//
// Both backends call this inside whatever serializes their appends (the
// hosted one under a row lock), so the rule has one implementation. The
// error return is kept so a future rule can refuse without a signature
// change across both backends.
func Decide(history []Promotion, requested Promotion) (*Promotion, error) {
	if n := len(history); n > 0 {
		current := history[n-1]
		if current.Release == requested.Release {
			return &current, nil
		}
	}
	return nil, nil
}
