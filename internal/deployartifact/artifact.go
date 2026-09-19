package deployartifact

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The artifact kind vocabulary.
//
// THESE ARE THE SAME TOKENS forge's release ledger already uses
// (internal/cli.ArtifactKind*) and the same ones control-plane migration
// 00075 constrains its `kind` column to. They are re-declared here rather
// than imported because internal/cli imports this package's neighbours
// and a dependency in the other direction would be a cycle — but the
// SPELLING is deliberately identical, and TestKindVocabularyMatchesLedger
// pins that it stays so. A second vocabulary is how a producer and a
// consumer end up disagreeing about what an artifact is.
const (
	// KindOCI is a container image or OCI artifact, addressed by digest.
	// The DEFAULT: an empty kind means this.
	KindOCI = "oci"
	// KindNPM is a package on an npm registry, addressed by version plus
	// the registry's integrity hash.
	KindNPM = "npm"
	// KindGoModule is a module on a Go module proxy, addressed by version
	// plus the go.sum hash.
	KindGoModule = "gomod"
	// KindFile is a file published to object storage or a CDN, addressed
	// by URL plus a content hash.
	KindFile = "file"
)

// Item is one typed thing a desired-state artifact carries.
//
// The two addressing shapes are the ledger's, unchanged: an OCI item is
// addressed by DIGEST ALONE (the digest is the name), everything else by
// a COORDINATE (version or uri) plus a hash. Kind discriminates.
type Item struct {
	// Name is the key a consumer matches on — an image name, a package
	// name. Required.
	Name string `json:"name"`
	// Kind is one of the Kind* constants. EMPTY MEANS OCI, matching
	// ReleaseArtifact.EffectiveKind and migration 00075's
	// COALESCE(NULLIF(kind,''),'oci'). Use EffectiveKind rather than
	// reading this field directly.
	Kind string `json:"kind,omitempty"`
	// Digest is the canonical `sha256:<64 hex>` identity. Required for
	// OCI, and MUST be empty for every other kind — see Validate.
	Digest string `json:"digest,omitempty"`
	// Version is how a consumer ASKS for a non-OCI item.
	Version string `json:"version,omitempty"`
	// Integrity is how a consumer KNOWS they got the right bytes, in the
	// hash format native to the ecosystem. Stored verbatim.
	Integrity string `json:"integrity,omitempty"`
	// URI is the location when it is not implied by name+version.
	URI string `json:"uri,omitempty"`
}

// EffectiveKind reports the item's kind, treating empty as OCI.
//
// Not a guess: before kinds existed an artifact could only hold images,
// so an item with no kind IS one. Spelled the same way on both sides of
// the wire (forge's EffectiveKind, the SQL COALESCE(NULLIF(kind,”),…))
// so that writing an empty string cannot mean something different from
// writing nothing.
func (it Item) EffectiveKind() string {
	if it.Kind == "" {
		return KindOCI
	}
	return it.Kind
}

// PinnedDigest returns an OCI item's digest and whether it had one.
//
// A NON-OCI ITEM ALWAYS RETURNS ("", false), and that is the guard, not a
// convenience. An npm package has no digest a container spec could be
// pinned with, so a caller asking for one must get "no" rather than a
// value it might write into a pod. Callers pinning images therefore skip
// non-OCI items for free, with no call site having to remember to check
// Kind — the same reasoning as ReleaseArtifact.SharedDigest.
func (it Item) PinnedDigest() (string, bool) {
	if it.EffectiveKind() != KindOCI {
		return "", false
	}
	return it.Digest, it.Digest != ""
}

// Artifact is the unpacked desired state for one environment.
type Artifact struct {
	// Release is the human-readable version label the artifact was cut
	// under.
	Release string `json:"release"`
	// Env is the environment this desired state describes.
	Env string `json:"env"`
	// Items are the typed artifacts the release ships, keyed by name.
	Items map[string]Item `json:"items"`
	// Digest is the artifact's OWN content-addressed identity — the
	// manifest digest it was fetched by. Set by the fetcher, not by the
	// document: a document that named its own digest would be asserting
	// something only the transport can verify.
	Digest string `json:"-"`
}

// canonicalDigest is the OCI digest form migration 00075's CHECK enforces
// (`^sha256:[a-f0-9]{64}$`). Spelled identically so a digest this package
// accepts is one the ledger would also accept — a looser regex here would
// let forge pull something the server could never have recorded.
var canonicalDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// kindToken is 00075's kind shape check (`^[a-z][a-z0-9+._-]*$`).
//
// It is what keeps "OCI" and "oci " from taking the non-OCI branch and
// thereby SILENTLY SKIPPING the digest requirement, which is the exact
// bypass the migration's comment calls out.
var kindToken = regexp.MustCompile(`^[a-z][a-z0-9+._-]*$`)

// ErrInvalidArtifact is the sentinel every validation failure wraps.
var ErrInvalidArtifact = errors.New("forge: invalid deploy artifact")

// Validate enforces the same rules migration 00075 enforces in SQL, on
// the client side, before anything is deployed from this artifact.
//
// Duplicating the server's constraints here is deliberate. The artifact
// is fetched from a registry, not read out of the database, so the
// database's CHECKs never see it — an artifact that would have been
// refused on insert can still be pulled and deployed unless the consumer
// re-checks. The rules:
//
//   - An OCI item MUST carry a canonical digest. The migration's comment
//     explains why this is written explicitly rather than left implicit:
//     a CHECK that evaluates to NULL is SATISFIED, so a nullable digest
//     column needs `IS NOT NULL` spelled out or the constraint enforces
//     nothing. The Go analogue is that "" passes any regex-free check, so
//     emptiness is tested first.
//   - A non-OCI item MUST NOT carry a digest, so that "is this digest
//     referenced" can never be answered "yes" by a package that merely
//     happened to carry an image's digest string.
//   - A non-OCI item MUST be ADDRESSABLE (version or uri), or the item
//     records that something exists while giving no way to fetch it —
//     the exact shape of the web-runtime failure that motivated 00075.
//     Integrity is deliberately NOT required: an unaddressable artifact
//     is unusable now, an unverified one is merely unverified.
func (a Artifact) Validate() error {
	if strings.TrimSpace(a.Release) == "" {
		return fmt.Errorf("%w: no release label", ErrInvalidArtifact)
	}
	var problems []string
	for name, it := range a.Items {
		if strings.TrimSpace(name) == "" {
			problems = append(problems, "an item has an empty name")
			continue
		}
		if it.Kind != "" && !kindToken.MatchString(it.Kind) {
			problems = append(problems, fmt.Sprintf(
				"%s: kind %q is not an ecosystem token (lowercase, leading letter); "+
					"a mis-cased kind would take the non-oci branch and skip the digest requirement",
				name, it.Kind))
			continue
		}
		if it.EffectiveKind() == KindOCI {
			if it.Digest == "" {
				problems = append(problems, fmt.Sprintf(
					"%s: oci item has no digest; an image that cannot be pinned cannot be promoted", name))
			} else if !canonicalDigest.MatchString(it.Digest) {
				problems = append(problems, fmt.Sprintf(
					"%s: digest %q is not canonical sha256:<64 hex>", name, it.Digest))
			}
			continue
		}
		if it.Digest != "" {
			problems = append(problems, fmt.Sprintf(
				"%s: %s item carries a digest; only oci items are digest-addressed, and a stray "+
					"digest here would make a package answer an image's identity query",
				name, it.EffectiveKind()))
		}
		if it.Version == "" && it.URI == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: %s item has neither version nor uri, so nothing can fetch it",
				name, it.EffectiveKind()))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidArtifact, strings.Join(problems, "; "))
	}
	return nil
}
