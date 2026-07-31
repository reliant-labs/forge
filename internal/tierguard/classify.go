package tierguard

// Classification of Tier-1 files into derived (correctly Tier-1) and
// constant (mis-tier candidates), plus the documented allow-list.

import (
	"bytes"
	"sort"
)

// Verdict is the classification of one Tier-1 path.
type Verdict int

const (
	// Derived: the file's bytes responded to the change in user inputs.
	// Tier-1 is correct — regenerating it is the only way the file can
	// track what the user declared.
	Derived Verdict = iota
	// Constant: both renders produced byte-identical content despite
	// materially different inputs. The file is not a function of user
	// input; it is library code behind a "do not edit" banner.
	Constant
	// OnlyInOne: the path was a Tier-1 target in one fixture and not the
	// other. That existence itself is input-dependent (a per-service or
	// per-entity file), which is derivation — reported separately so the
	// asymmetry is visible rather than silently counted as Derived.
	OnlyInOne
	// Unreadable: a Tier-1 target with no readable bytes in one or both
	// renders. Classification impossible; never silently a pass.
	Unreadable
)

func (v Verdict) String() string {
	switch v {
	case Derived:
		return "derived"
	case Constant:
		return "CONSTANT"
	case OnlyInOne:
		return "derived (presence)"
	case Unreadable:
		return "UNREADABLE"
	default:
		return "unknown"
	}
}

// Classification is one path's verdict plus the evidence for it.
type Classification struct {
	Path string
	// Verdict is the outcome.
	Verdict Verdict
	// Evidence states what actually differed (or that nothing did), in
	// terms a reader can check against the two rendered trees.
	Evidence string
	// SizeBytes is the file's size in the fixture where it exists (the
	// A render when it exists in both). Reported so mis-tier candidates
	// can be ranked by how much library code is sitting in user trees.
	SizeBytes int
	// Lines is the newline count of the same content.
	Lines int
	// IdentityDependent is meaningful only for a Constant verdict: the
	// file's bytes did not respond to any user DECLARATION, but they do
	// change with the project's module path / binary name (fixture C).
	// It does not rescue Tier-1 — the body is still not derived — but it
	// changes the remedy from "move verbatim to forge/pkg" to "library
	// code in forge/pkg plus a small user-owned scaffold that wires the
	// module-specific names".
	IdentityDependent bool
}

// Classify compares two renders and returns one Classification per path
// that either render targeted as Tier-1, sorted by path.
//
// The comparison is over marker-stripped bytes. The forge:hash marker is
// a hash of the content, so it cannot make equal content look different;
// stripping it keeps the reported evidence about the body.
//
// identity, when non-nil, is the third render (fixture C: A's inputs
// under a different module path). It never changes a verdict — only
// annotates Constant findings with whether the file embeds the user's
// module identity, which determines the remedy.
func Classify(a, b, identity *renderResult) []Classification {
	paths := map[string]bool{}
	for p := range a.Tier1 {
		paths[p] = true
	}
	for p := range b.Tier1 {
		paths[p] = true
	}

	var out []Classification
	for p := range paths {
		c := classifyOne(p, a, b)
		if c.Verdict == Constant && identity != nil {
			c.IdentityDependent = differsUnderIdentity(p, a, identity)
			if c.IdentityDependent {
				c.Evidence += "; does vary with the module path (fixture " + identity.Label +
					"), so it embeds the user's own import paths but no declaration of theirs"
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// differsUnderIdentity reports whether path's bytes change between the
// baseline render and the identity-varied render. Absent from the
// identity render (or unreadable) counts as not differing: the annotation
// must never invent a distinction it did not observe.
func differsUnderIdentity(path string, base, identity *renderResult) bool {
	bodyBase, okBase := base.Bodies[path]
	bodyAlt, okAlt := identity.Bodies[path]
	if !okBase || !okAlt {
		return false
	}
	return !bytes.Equal(bodyBase, bodyAlt)
}

func classifyOne(p string, a, b *renderResult) Classification {
	inA, inB := a.Tier1[p], b.Tier1[p]
	bodyA, okA := a.Bodies[p]
	bodyB, okB := b.Bodies[p]

	size, lines := 0, 0
	switch {
	case okA:
		size, lines = len(bodyA), bytes.Count(bodyA, []byte("\n"))
	case okB:
		size, lines = len(bodyB), bytes.Count(bodyB, []byte("\n"))
	}

	// Targeted by only one fixture: its very existence tracks the
	// inputs. Per-service and per-entity files land here.
	if inA != inB {
		only, other := a.Label, b.Label
		if inB {
			only, other = b.Label, a.Label
		}
		return Classification{
			Path:      p,
			Verdict:   OnlyInOne,
			Evidence:  "emitted only for " + only + ", absent from " + other + " — the path itself is input-dependent",
			SizeBytes: size,
			Lines:     lines,
		}
	}

	if !okA || !okB {
		missing := a.Label
		if okA {
			missing = b.Label
		}
		return Classification{
			Path:      p,
			Verdict:   Unreadable,
			Evidence:  "Tier-1 target in both fixtures but unreadable in " + missing + " (disowned-skip, or emitted outside the project root) — cannot be classified",
			SizeBytes: size,
			Lines:     lines,
		}
	}

	if !bytes.Equal(bodyA, bodyB) {
		return Classification{
			Path:      p,
			Verdict:   Derived,
			Evidence:  describeDiff(bodyA, bodyB),
			SizeBytes: size,
			Lines:     lines,
		}
	}

	return Classification{
		Path:    p,
		Verdict: Constant,
		Evidence: "byte-identical across both fixtures: services, entities, columns, scalar kinds, " +
			"frontend and contract.go all changed and this file did not move",
		SizeBytes: size,
		Lines:     lines,
	}
}

// describeDiff summarizes how two renders of the same path differ, so a
// Derived verdict carries checkable evidence rather than an assertion.
// It reports the first differing line with both sides' text, which is
// enough to see WHICH user input the file tracks.
func describeDiff(a, b []byte) string {
	la := bytes.Split(a, []byte("\n"))
	lb := bytes.Split(b, []byte("\n"))
	for i := 0; i < len(la) && i < len(lb); i++ {
		if !bytes.Equal(la[i], lb[i]) {
			return "differs from line " + itoa(i+1) + ": A=" + trunc(string(la[i])) + " | B=" + trunc(string(lb[i]))
		}
	}
	return "identical prefix, differing length: A=" + itoa(len(la)) + " lines, B=" + itoa(len(lb)) + " lines"
}

func trunc(s string) string {
	const max = 90
	s = trimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
