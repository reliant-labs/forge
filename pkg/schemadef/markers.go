package schemadef

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// A `forge:*` declaration in a catalog comment is read by exact name, and a
// name nothing reads does nothing: postgres stores the comment verbatim, the
// generator looks for the markers it knows and finds none, and the column
// keeps the behaviour the author was trying to switch off. `forge:immutible`
// leaves the column writable; `forge:no-softdelete` leaves deleted_at a
// tombstone. Neither says so.
//
// So every forge marker in a catalog comment is checked against the markers
// that object can carry, and one that is unknown — or known but declared
// where nothing reads it — fails the generate that reads the schema
// (CheckMarkers), and the `forge lint` lane that reads the migrations
// (CommentMarkerProblems). Both classify through this file, so the two
// cannot disagree about what a marker is.

// MarkerSite is the kind of catalog object a comment is attached to. Each
// site has its own vocabulary: a column marker on a table comment is read by
// nothing, exactly like a misspelled one.
type MarkerSite string

// The catalog objects a `forge:*` comment can sit on.
const (
	SiteTable      MarkerSite = "table"
	SiteView       MarkerSite = "view"
	SiteColumn     MarkerSite = "column"
	SiteConstraint MarkerSite = "constraint"
)

// SiteMarkers returns the marker names a comment on site may carry.
func SiteMarkers(site MarkerSite) []string {
	switch site {
	case SiteTable:
		return []string{TableMarkerAppendOnly, TableMarkerNoEntity, TableMarkerEntityName}
	case SiteView:
		return []string{ViewMarkerEntity, TableMarkerEntityName}
	case SiteColumn:
		var out []string
		for _, m := range KnownColumnMarkers {
			if m != ColumnMarkerRef {
				out = append(out, m)
			}
		}
		return out
	case SiteConstraint:
		return []string{ColumnMarkerRef}
	}
	return nil
}

// markerTokenRE finds every `forge:<name>` in a comment. The name is the run
// of name characters after the colon, so an argument attached with `=`
// (`forge:fill=ulid`, `forge:go-type=map[string]any`) or separated by a space
// (`forge:ref derived-from=job_id`) is not part of it. Upper case is read
// too, so `forge:Immutable` is reported rather than silently skipped. The
// leading guard keeps `xforge:` and a path segment (`.../forge:`) out.
var markerTokenRE = regexp.MustCompile(`(?:^|[^\w/.:-])(forge:[A-Za-z][A-Za-z0-9_-]*)`)

// MarkerTokens returns every forge marker name a comment carries, in order.
func MarkerTokens(comment string) []string {
	var out []string
	for _, m := range markerTokenRE.FindAllStringSubmatch(comment, -1) {
		out = append(out, m[1])
	}
	return out
}

// CommentMarkerProblems returns one sentence per forge marker in comment that
// a comment on site does not read. column is the column's name for
// SiteColumn, and "" otherwise.
func CommentMarkerProblems(site MarkerSite, column, comment string) []MarkerProblem {
	var out []MarkerProblem
	allowed := SiteMarkers(site)
	for _, tok := range MarkerTokens(comment) {
		if slices.Contains(allowed, tok) {
			if tok == ColumnMarkerNoSoftDelete && column != ColDeletedAt {
				out = append(out, MarkerProblem{Marker: tok, Problem: fmt.Sprintf(
					"%s only applies to the %s column, the one forge soft-deletes by; on %s it does nothing — remove it",
					tok, ColDeletedAt, column)})
			}
			continue
		}
		if other := siteOf(tok); other != "" {
			out = append(out, MarkerProblem{Marker: tok, Problem: fmt.Sprintf(
				"%s is a %s marker and does nothing on a %s — declare it with COMMENT ON %s",
				tok, other, site, strings.ToUpper(string(other)))})
			continue
		}
		problem := fmt.Sprintf("%s is not a marker forge reads, so it does nothing — %s markers: %s",
			tok, site, strings.Join(allowed, ", "))
		if near := nearestMarker(tok); near != "" {
			problem += fmt.Sprintf(" (did you mean %s?)", near)
		}
		out = append(out, MarkerProblem{Marker: tok, Problem: problem})
	}
	return out
}

// siteOf names the site a known marker belongs to, "" for an unknown one.
func siteOf(marker string) MarkerSite {
	for _, s := range []MarkerSite{SiteColumn, SiteConstraint, SiteTable, SiteView} {
		if slices.Contains(SiteMarkers(s), marker) {
			return s
		}
	}
	return ""
}

// nearestMarker is the known marker within a small edit distance of tok, ""
// when none is close enough to be the likely intent.
func nearestMarker(tok string) string {
	best, bestDist := "", 3
	seen := map[string]bool{}
	for _, s := range []MarkerSite{SiteColumn, SiteConstraint, SiteTable, SiteView} {
		for _, m := range SiteMarkers(s) {
			if seen[m] {
				continue
			}
			seen[m] = true
			if d := editDistance(strings.ToLower(tok), m); d < bestDist {
				best, bestDist = m, d
			}
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// MarkerProblem is one forge marker a catalog comment carries that nothing
// reads where it is declared.
type MarkerProblem struct {
	// Object names where the comment sits: "table worktrees",
	// "column worktrees.deleted_at", "constraint x_fkey on worktrees".
	// Empty from CommentMarkerProblems, which does not know it.
	Object string
	// Marker is the offending forge:* name, verbatim.
	Marker string
	// Problem is the sentence saying why it does nothing and what to do.
	Problem string
}

// CheckMarkers checks every catalog comment of the applied schema — table,
// view, column and foreign-key constraint — and returns an error naming each
// forge marker that does nothing where it is declared, nil when there is
// none. It is what makes a misspelled or misplaced marker fail the generate
// that reads it, instead of shipping the behaviour it was written to change.
func CheckMarkers(tables []Table) error {
	var problems []MarkerProblem
	for _, t := range tables {
		site := SiteTable
		if t.IsView {
			site = SiteView
		}
		problems = append(problems, located(fmt.Sprintf("%s %s", site, t.Name), CommentMarkerProblems(site, "", t.Comment))...)
		for _, c := range t.Columns {
			problems = append(problems, located(fmt.Sprintf("column %s.%s", t.Name, c.Name), CommentMarkerProblems(SiteColumn, c.Name, c.Comment))...)
		}
		for _, fk := range t.ForeignKeys {
			problems = append(problems, located(fmt.Sprintf("constraint %s on %s", fk.Name, t.Name), CommentMarkerProblems(SiteConstraint, "", fk.Comment))...)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.SliceStable(problems, func(i, j int) bool { return problems[i].Object < problems[j].Object })
	lines := make([]string, 0, len(problems))
	for _, p := range problems {
		lines = append(lines, fmt.Sprintf("  - %s: %s", p.Object, p.Problem))
	}
	return errors.New("the applied schema's catalog comments carry forge markers that do nothing where they are declared " +
		"(fix the COMMENT ON in the migration; `forge project annotations --kind column` and `--kind table` list every marker):\n" +
		strings.Join(lines, "\n"))
}

func located(object string, ps []MarkerProblem) []MarkerProblem {
	for i := range ps {
		ps[i].Object = object
	}
	return ps
}
