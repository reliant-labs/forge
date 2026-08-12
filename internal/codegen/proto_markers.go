// File: internal/codegen/proto_markers.go
//
// The `forge:*` PROTO marker registry — the single source of truth for the
// marker vocabulary a .proto comment may carry, and the twin of
// schemadef.KnownColumnMarkers on the SQL side.
//
// Before this registry the vocabulary was implicit: six markers spelled as
// six separate regexes across three packages (the raw scanner here, the
// descriptor path in internal/cli/forge_descriptor.go, the hook generator in
// internal/cli/generate_frontend_hooks.go). Nothing enumerated them, so
// nothing could answer "is this a marker forge knows?" — and a misspelled
// marker was therefore indistinguishable from an ordinary proto comment.
// That is exactly how `forge:server-set` (renamed to `forge:read-only`) went
// silent: the old spelling reads as prose, the field stays client-writable
// on Create/Update, and the birth exits zero with no warning.
//
// The registry closes that by giving the recognizers and the
// unknown-proto-marker lint check (internal/cli/lint/lint_proto_markers.go)
// ONE vocabulary. Each recognizer keeps its own scanning logic — the raw
// scanner reads `//`-prefixed source lines, the descriptor path reads text
// buf has already stripped the slashes from — but every one of them now
// spells its marker with a constant from this file, so a rename lands in one
// place and the lint check can never enforce a vocabulary the scanners have
// moved on from.
//
// SCOPE: proto comments only. The `forge:*` markers read out of Go source
// (forge:constructor, forge:optional-dep, forge:no-observe, forge:service,
// forge:exclude-contract, forge:external-component, forge:outbound-io,
// forge:gen, forge:placeholder, forge:custom-read-shape, forge:hash) and the
// ones read out of postgres catalog comments (schemadef.KnownColumnMarkers:
// forge:immutable, forge:ref, forge:version, forge:fill) are deliberately
// NOT here. They are legitimate markers in their own files, but none of them
// is read from a .proto, so registering them would teach the proto check to
// accept a marker that does nothing where it was written.

package codegen

import (
	"regexp"
	"sort"
	"strings"
)

// The markers a .proto comment may carry. Each names the recognizer
// that consumes it, because they are not all consumed by the same pass —
// a fact the registry has to cover or the lint check would flag a valid
// marker whose reader it did not know about.
const (
	// ProtoMarkerEntity tablizes a top-level message at birth. Read by the
	// raw scanner (ScanRawProtoDir) — the message may be brand new and not
	// yet reachable from any RPC, so the compiled descriptor cannot carry it.
	ProtoMarkerEntity = "forge:entity"

	// ProtoMarkerSoftDelete births the entity with a nullable deleted_at
	// column. Implies ProtoMarkerEntity. Read by the raw scanner.
	ProtoMarkerSoftDelete = "forge:soft-delete"

	// ProtoMarkerAppendOnly births the entity without Update/Delete RPCs and
	// with a DB guard rejecting both. Implies ProtoMarkerEntity. Read by the
	// raw scanner.
	ProtoMarkerAppendOnly = "forge:append-only"

	// ProtoMarkerReadOnly keeps a field on the entity and on Get/List but
	// omits it from the born Create/Update requests. Read by BOTH the raw
	// scanner (at birth) and the descriptor path (fieldHasReadOnlyMarker).
	ProtoMarkerReadOnly = "forge:read-only"

	// ProtoMarkerComputed is ProtoMarkerReadOnly plus a DECLARED OBLIGATION:
	// the field is omitted from the born Create/Update requests exactly as
	// read-only is, and the author is additionally stating that something in
	// the app derives its value. The unwritten-computed-field lint check
	// (internal/cli/lint/lint_computed_fields.go) holds them to it.
	//
	// It exists because read-only alone is silent about the SECOND half. A
	// read-only field forge omits from Create and nothing populates takes the
	// column default — `0` for a money column — and the app shows $0.00 with
	// no error anywhere: no constraint is violated, no test fails, and the
	// only symptom is a human eventually reading a screen. The motivating
	// case was a line item's amount_cents, whose comment promised
	// quantity × unit price and whose rows were all zero.
	//
	// Read by BOTH read-only recognizers (the raw scanner at birth and the
	// descriptor path), which treat it as read-only — a marker that only lint
	// understood would change no generated output and would be a second
	// spelling of nothing.
	ProtoMarkerComputed = "forge:computed"

	// ProtoMarkerSecret keeps the column but strips the field from read
	// responses. Read by the descriptor path (fieldHasSecretMarker) ONLY —
	// the raw scanner has no need for it, which is precisely why the
	// registry must be the union of what every pass reads rather than the
	// vocabulary of any single scanner.
	ProtoMarkerSecret = "forge:secret"

	// ProtoMarkerMutation forces an rpc's generated React Query hook to be a
	// useMutation. Read by the frontend hook generator on EVERY generate,
	// not at birth — the one marker here that is not an entity-birth
	// concern, and another reason the registry cannot be derived from the
	// birth scanner alone.
	ProtoMarkerMutation = "forge:mutation"
)

// KnownProtoMarkers is the complete set of `forge:*` marker names a .proto
// comment may carry, spanning every pass that reads one (entity birth,
// descriptor projection, frontend hook generation). It is the single source
// of truth shared by `forge project annotations` (internal/cli/annotations.go)
// and the unknown-proto-marker lint check (internal/cli/lint), mirroring
// schemadef.KnownColumnMarkers on the SQL side so that neither the docs nor
// the linter can drift from what the scanners actually recognize.
var KnownProtoMarkers = []string{
	ProtoMarkerEntity,
	ProtoMarkerSoftDelete,
	ProtoMarkerAppendOnly,
	ProtoMarkerReadOnly,
	ProtoMarkerComputed,
	ProtoMarkerSecret,
	ProtoMarkerMutation,
}

// RemovedProtoMarkers maps a spelling forge USED to recognize onto the
// marker that replaced it. Entries here are inert — nothing in any scanner
// consults this map, and a proto carrying one of these keys behaves exactly
// as if it carried arbitrary prose, which is the point. It exists so the
// lint check can say "renamed to X" instead of the far less actionable
// "unrecognized", turning the one diagnostic an author gets into a runbook.
//
// Adding a key here must never be mistaken for restoring the alias: the
// contract is that the old spelling stays dead and only the MESSAGE about it
// improves.
var RemovedProtoMarkers = map[string]string{
	// Renamed to forge:read-only. The rename left no diagnostic behind, so
	// an author writing the old spelling got complete silence — the field
	// stayed client-writable on Create/Update and the birth exited zero.
	"forge:server-set": ProtoMarkerReadOnly,
}

// retiredMarkerRE recognizes ONE retired spelling in a `//`-prefixed proto
// comment. Built per marker with the same builder the live markers use, so
// a retired spelling is matched by exactly the rule that used to match it —
// glued `//forge:server-set` included, trailing prose tolerated, and a
// longer token that merely starts with it (`forge:server-settings`)
// refused.
func retiredMarkerRE(marker string) *regexp.Regexp { return protoMarkerLineRE(marker) }

// RetiredProtoMarkerSites returns every retired-spelling occurrence in one
// .proto file's source, as byte offsets paired with the marker found and
// its replacement.
//
// This is deliberately a SOURCE scan over the same text the raw scanner
// reads, not a token scan over all prose containing `forge:`: only the
// exact retired spellings in RemovedProtoMarkers can match, so a comment
// merely mentioning `forge:server-set` in a sentence still matches (it is
// the same regex that would have honoured it) while unrelated prose,
// unknown typos, and markers belonging to other passes (forge:mutation,
// forge:secret) cannot. Typos stay the lint check's business at warning
// severity; a retired spelling has a definite fix and is escalated here.
func RetiredProtoMarkerSites(content string) []RetiredProtoMarkerMatch {
	var out []RetiredProtoMarkerMatch
	for marker, replacement := range RemovedProtoMarkers {
		for _, loc := range retiredMarkerRE(marker).FindAllStringIndex(content, -1) {
			out = append(out, RetiredProtoMarkerMatch{
				Offset:     loc[0],
				Marker:     marker,
				ReplacedBy: replacement,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

// RetiredProtoMarkerMatch is one retired-spelling occurrence located in
// proto source, before it is attributed to a message.
type RetiredProtoMarkerMatch struct {
	// Offset is the byte offset of the match within the scanned content.
	Offset int
	// Marker is the retired spelling as registered (e.g. forge:server-set).
	Marker string
	// ReplacedBy is the marker that superseded it.
	ReplacedBy string
}

// IsKnownProtoMarker reports whether name is a marker forge reads out of a
// proto comment. The comparison is EXACT: `forge:read-only-ish` is not
// `forge:read-only`, the same discipline the column-marker check enforces so
// a longer marker cannot pass as a shorter one it merely starts with.
func IsKnownProtoMarker(name string) bool {
	for _, m := range KnownProtoMarkers {
		if name == m {
			return true
		}
	}
	return false
}

// ClosestProtoMarker returns the known proto marker nearest to name by
// Levenshtein distance, or "" when nothing is close enough to be worth
// suggesting. The cutoff scales with the name's length (a third of it,
// minimum 1) so a genuine typo — `forge:read-only`, `forge:etnity` — earns a
// suggestion while unrelated prose that happens to start with `forge:` does
// not get an arbitrary marker pinned on it.
//
// A removed spelling is deliberately NOT resolved here; RemovedProtoMarkers
// carries a better answer than edit distance can, and the caller checks it
// first.
func ClosestProtoMarker(name string) string {
	return ClosestFrom(name, KnownProtoMarkers)
}

// ClosestFrom returns the entry of vocabulary nearest to name by
// Levenshtein distance, or "" when nothing is close enough to be worth
// suggesting. The cutoff scales with the name's length (a third of it,
// minimum 1), so a genuine typo earns a suggestion while an unrelated
// token does not get an arbitrary entry pinned on it.
//
// Exported as the vocabulary-agnostic core of [ClosestProtoMarker] so the
// checks that validate a DIFFERENT forge vocabulary — the annotation
// OPTION fields in internal/cli/lint/lint_proto_options.go, read off the
// compiled descriptors — score near-misses by exactly the same rule
// rather than growing a second, subtly different one.
func ClosestFrom(name string, vocabulary []string) string {
	best, bestDist := "", -1
	for _, m := range vocabulary {
		d := levenshtein(name, m)
		if bestDist < 0 || d < bestDist {
			best, bestDist = m, d
		}
	}
	cutoff := len(name) / 3
	if cutoff < 1 {
		cutoff = 1
	}
	if bestDist > cutoff {
		return ""
	}
	return best
}

// levenshtein is the standard two-row edit distance. Small and local on
// purpose: the only caller is ClosestProtoMarker, and the marker names it
// compares are a handful of short strings.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(min(curr[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// protoMarkerLineRE builds the recognizer for a marker written as a
// `//`-prefixed proto comment: `//\s*` tolerates the glued `//forge:entity`
// spelling, and the trailing group tolerates prose after the token
// (`// forge:entity — orders table`) while still refusing a longer marker
// that merely starts with this one.
//
// The expression is NOT anchored to line start, so the same pattern matches
// a TRAILING inline comment; callers that want the leading-full-line form
// only (the entity-level markers) anchor it themselves via
// protoMarkerFullLineRE.
func protoMarkerLineRE(marker string) *regexp.Regexp {
	return regexp.MustCompile(`//+\s*` + regexp.QuoteMeta(marker) + `(\s|$|[^\w])`)
}

// ReadOnlyProtoMarkers are the markers that exclude a field from the born
// Create/Update request messages. ProtoMarkerComputed is read-only PLUS a
// declared derivation obligation, so every pass that honours read-only must
// honour it identically — a marker that changed no generated output would
// be a second spelling of nothing, and a field marked computed but still
// client-writable is the exact silent-write hole read-only exists to close.
//
// Exported as a set rather than left as two independent regexes because the
// implication has to hold in BOTH recognizers (the raw scanner at birth and
// the descriptor path). Two hand-maintained lists is how one of them ends up
// honouring a marker the other ignores, which would make a field's
// writability depend on which pass found it.
var ReadOnlyProtoMarkers = []string{ProtoMarkerReadOnly, ProtoMarkerComputed}

// ProtoMarkerAnyLineRE builds a recognizer matching ANY of markers in a
// `//`-prefixed proto comment, with the same spacing/trailing-prose rules as
// the single-marker builder. Alternation is over quoted marker names, so a
// longer token that merely starts with one of them is still refused.
func ProtoMarkerAnyLineRE(markers []string) *regexp.Regexp {
	return regexp.MustCompile(`//+\s*(?:` + quotedMarkerAlternation(markers) + `)(\s|$|[^\w])`)
}

// ProtoMarkerAnyCommentRE is ProtoMarkerAnyLineRE for comment text buf has
// already stripped the `//` from — the descriptor path's shape.
func ProtoMarkerAnyCommentRE(markers []string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*(?:` + quotedMarkerAlternation(markers) + `)(\s|$|[^\w])`)
}

func quotedMarkerAlternation(markers []string) string {
	quoted := make([]string, 0, len(markers))
	for _, m := range markers {
		quoted = append(quoted, regexp.QuoteMeta(m))
	}
	return strings.Join(quoted, "|")
}

// protoMarkerFullLineRE is protoMarkerLineRE anchored to the start of the
// line: the marker must be the whole comment line, which is the documented
// site for the entity-level markers that precede a `message X {`.
func protoMarkerFullLineRE(marker string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*//+\s*` + regexp.QuoteMeta(marker) + `(\s|$|[^\w])`)
}

// ProtoMarkerCommentRE builds the recognizer for a marker read out of a
// comment buf has ALREADY stripped the `//` from — the shape protogen hands
// to the descriptor path in internal/cli/forge_descriptor.go. Spacing and
// trailing prose are tolerated as above; `(?m)` lets it match any line of a
// multi-line comment block.
//
// Exported because its callers live outside this package, unlike the two
// raw-source builders above.
func ProtoMarkerCommentRE(marker string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(marker) + `(\s|$|[^\w])`)
}
