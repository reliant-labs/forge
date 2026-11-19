package seedplan

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// What an ARRAY column's own CHECKs declare about its value.
//
// An array column used to get two invented elements on every row, whatever
// its CHECK said. That holds for an unconstrained `tags TEXT[]`, and it is a
// guaranteed rejection for the shape a real closed-set array takes:
//
//	scopes TEXT[] NOT NULL,
//	CONSTRAINT access_tokens_scopes CHECK (
//	    cardinality(scopes) > 0
//	    AND scopes <@ ARRAY['deploy:read', 'deploy:write', …])
//
// `ARRAY['sample_scopes_1', 'sample_scopes_2']` is contained in no declared
// set, so the INSERT fails the CHECK — and the seed being one transaction, it
// takes every other table down with it. The array's elements are a
// vocabulary exactly as a scalar `status IN (…)` is (closed-set declares
// both), so they are drawn from it; the cardinality bound is a declaration
// about the element COUNT, so it decides how many.
//
// The two shapes read here are the ones a migration writes and
// pg_get_constraintdef renders: `col <@ ARRAY[…]` (or `<@ '{a,b}'::text[]`),
// and `cardinality(col)` / `array_length(col, 1)` compared with a constant.
// Anything else leaves the column to plain synthesis, as before.

// arrayShape is the declared shape of one array column's value.
type arrayShape struct {
	// members is the contained-by set every element must come from, in the
	// order the CHECK spells it. nil means the elements are open.
	members []string
	// minLen and maxLen bound the element count; maxLen holds only when
	// hasMax is set.
	minLen, maxLen int
	hasMax         bool
}

// count is how many elements a row carries: want (the synthesizer's natural
// count), raised to the declared minimum and lowered to the declared maximum.
// A contradictory bound (min above max) is one forge cannot satisfy, so it
// is ignored rather than half-honored.
func (s arrayShape) count(want int) int {
	if s.hasMax && s.minLen > s.maxLen {
		return want
	}
	if want < s.minLen {
		want = s.minLen
	}
	if s.hasMax && want > s.maxLen {
		want = s.maxLen
	}
	return want
}

var (
	// arrayCardinalityRE reads an element-count comparison:
	// `(cardinality(scopes) > 0)`, `(array_length(tags, 1) >= 2)`, with the
	// column optionally parenthesized and cast (`(tags)::text[]`).
	arrayCardinalityRE = regexp.MustCompile(`(?:cardinality|array_length)\(\s*\(?"?(\w+)"?\)?(?:::[a-z][a-z0-9_ ]*(?:\[\])?)?\s*(?:,\s*1\s*)?\)\s*(=|>=|<=|<>|!=|>|<)\s*(\d+)`)
	// arrayColumnBeforeRE names the column on the left of a `<@`: the
	// identifier at the end of the text before it, past closing parentheses
	// and a cast (`(scopes)::text[]`).
	arrayColumnBeforeRE = regexp.MustCompile(`"?([A-Za-z_][A-Za-z0-9_]*)"?\)*(?:::[a-z][a-z0-9_ ]*(?:\[\])?)?\)*\s*$`)
)

// arrayShapeOf reads the declared shape of an array column from the CHECKs
// on it alone (the same single-column scope patternsOf and LengthBounds read).
// Several contained-by sets on one column intersect: an element must satisfy
// every one of them.
func arrayShapeOf(t schemadef.Table, col schemadef.Column) arrayShape {
	var s arrayShape
	if !col.IsArray {
		return s
	}
	for _, ck := range t.Checks {
		if len(ck.Columns) != 1 || ck.Columns[0] != col.Name {
			continue
		}
		for _, m := range arrayCardinalityRE.FindAllStringSubmatch(ck.Def, -1) {
			if m[1] != col.Name {
				continue
			}
			n, err := strconv.Atoi(m[3])
			if err != nil {
				continue
			}
			s.bound(m[2], n)
		}
		if members, ok := containedByMembers(ck.Def, col.Name); ok {
			s.members = intersectMembers(s.members, members)
		}
	}
	return s
}

// bound folds one `count <op> n` comparison into the shape.
func (s *arrayShape) bound(op string, n int) {
	raise := func(v int) {
		if v > s.minLen {
			s.minLen = v
		}
	}
	lower := func(v int) {
		if v < 0 {
			v = 0
		}
		if !s.hasMax || v < s.maxLen {
			s.maxLen, s.hasMax = v, true
		}
	}
	switch op {
	case "=":
		raise(n)
		lower(n)
	case ">=":
		raise(n)
	case ">":
		raise(n + 1)
	case "<>", "!=":
		if n == 0 {
			raise(1)
		}
	case "<=":
		lower(n)
	case "<":
		lower(n - 1)
	}
}

// intersectMembers returns the members of b that a also holds, in b's order;
// a nil a is the open set.
func intersectMembers(a, b []string) []string {
	if a == nil {
		return b
	}
	in := make(map[string]bool, len(a))
	for _, v := range a {
		in[v] = true
	}
	out := []string{}
	for _, v := range b {
		if in[v] {
			out = append(out, v)
		}
	}
	return out
}

// containedByMembers reads the set a `<column> <@ <array>` comparison in def
// confines the column to. ok is false when def holds no such comparison on
// the column, or its array is not a list of constants this reads — a partial
// set would let synthesis "satisfy" a constraint it never read.
func containedByMembers(def, column string) ([]string, bool) {
	for at := 0; ; {
		idx := strings.Index(def[at:], "<@")
		if idx < 0 {
			return nil, false
		}
		op := at + idx
		at = op + 2
		if m := arrayColumnBeforeRE.FindStringSubmatch(def[:op]); m == nil || m[1] != column {
			continue
		}
		if members, ok := parseArrayOperand(def[at:]); ok {
			return members, true
		}
	}
}

// parseArrayOperand reads the constant array at the start of s, behind any
// opening parentheses: `ARRAY['a'::text, 'b'::text]`, `ARRAY[1, (2)::bigint]`
// or the literal `'{a,"b c"}'::text[]`.
func parseArrayOperand(s string) ([]string, bool) {
	s = strings.TrimLeft(s, " \t\n\r(")
	if strings.HasPrefix(s, "'") {
		raw, _, ok := readQuoted(s)
		if !ok {
			return nil, false
		}
		return parseArrayText(raw)
	}
	if len(s) < len("ARRAY[") || !strings.EqualFold(s[:len("ARRAY[")], "ARRAY[") {
		return nil, false
	}
	return parseArrayMembers(s[len("ARRAY["):])
}

// parseArrayMembers reads `member, member, …]`: each member a quoted string
// or a number (bare or parenthesized), each with an optional cast.
func parseArrayMembers(s string) ([]string, bool) {
	var out []string
	for {
		s = strings.TrimLeft(s, " \t\n\r")
		var v string
		if strings.HasPrefix(s, "'") {
			raw, rest, ok := readQuoted(s)
			if !ok {
				return nil, false
			}
			v, s = raw, rest
		} else {
			end := strings.IndexAny(s, ",]")
			if end < 0 {
				return nil, false
			}
			tok := strings.TrimSpace(s[:end])
			if num := checkNumMemberRE.FindStringSubmatch(tok); num != nil {
				v = num[1]
			} else if lt := strings.ToLower(tok); lt == "true" || lt == "false" {
				v = lt
			} else {
				return nil, false // NULL, an expression, a nested array
			}
			s = s[end:]
		}
		// A per-member cast (`::text`, `::character varying`) is spelling,
		// not value.
		s = strings.TrimLeft(s, " \t\n\r")
		if strings.HasPrefix(s, "::") {
			end := strings.IndexAny(s, ",]")
			if end < 0 {
				return nil, false
			}
			s = s[end:]
		}
		out = append(out, v)
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case strings.HasPrefix(s, "]"):
			return out, len(out) > 0
		default:
			return nil, false
		}
	}
}

// readQuoted reads the single-quoted SQL string at the start of s, a doubled
// quote inside it being one quote, and returns its value and what follows.
func readQuoted(s string) (raw, rest string, ok bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return b.String(), s[i+1:], true
	}
	return "", "", false
}

// parseArrayText reads the members of a postgres array literal's text,
// `{a,"b c"}`. A member holding a quote, a backslash or a comma inside quotes
// is not a shape this reads, so it declines rather than mis-split.
func parseArrayText(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, false
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" || strings.ContainsAny(body, `\{}`) {
		return nil, false
	}
	var out []string
	for _, m := range strings.Split(body, ",") {
		m = strings.TrimSpace(m)
		if len(m) >= 2 && m[0] == '"' && m[len(m)-1] == '"' {
			m = m[1 : len(m)-1]
		}
		if strings.Contains(m, `"`) {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// arrayLiteral renders a Postgres ARRAY[...] literal for a real array column.
// A uuid[] element is a uuid like any other — the placeholder string postgres
// rejects in a uuid column it rejects just as flatly inside an array of them.
//
// A column whose CHECK confines its elements to a set draws ONE element from
// that set per row (more when a cardinality minimum asks): a one-element array
// satisfies `cardinality(x) > 0` and `x <@ ARRAY[…]` on every row, where two
// elements would need a set of two. Every other array keeps its two invented
// elements, fitted to any declared count bound.
func (p *Plan) arrayLiteral(t schemadef.Table, col schemadef.Column, i int) string {
	shape := arrayShapeOf(t, col)
	var elems []string
	if len(shape.members) > 0 {
		salt := p.cfg.EffectiveSalt()
		vals := permute(SeedEnumChoices(shape.members), salt, t.Name, col.Name)
		start := pick(salt, t.Name, col.Name, i, len(vals))
		for k := 0; k < shape.count(1); k++ {
			// Past the set's size the elements repeat, which `<@` admits.
			elems = append(elems, arrayMemberLiteral(col, vals[(start+k)%len(vals)]))
		}
	} else {
		for k := 0; k < shape.count(2); k++ {
			elems = append(elems, syntheticArrayElement(t, col, i, k))
		}
	}
	if len(elems) == 0 {
		return "'{}'" // an untyped empty array literal coerces to the column's type
	}
	lit := "ARRAY[" + strings.Join(elems, ", ") + "]"
	if isUUIDArrayColumn(col) {
		lit += "::uuid[]"
	}
	return lit
}

// syntheticArrayElement is the element at position pos of an unconstrained
// array column's value at row i: the row's own value, offset by the position,
// so the elements of one array differ from each other.
func syntheticArrayElement(t schemadef.Table, col schemadef.Column, i, pos int) string {
	switch {
	case col.Type == schemadef.TypeInt:
		return strconv.Itoa(i + pos + 1)
	case col.Type == schemadef.TypeFloat:
		return fmt.Sprintf("%.2f", float64(i+pos+1)*1.5)
	case col.Type == schemadef.TypeBool:
		// Alternates by position, so an array of two reads true, false.
		return strconv.FormatBool(pos%2 == 0)
	case isUUIDArrayColumn(col):
		return sqlString(syntheticUUID(t.Name, col.Name, i+pos))
	default: // string / other → text-ish array
		return sqlString(placeholderString(col.Name, i+pos))
	}
}

// arrayMemberLiteral renders one contained-by member for the column's element
// type: bare for numeric and boolean elements, quoted otherwise.
func arrayMemberLiteral(col schemadef.Column, v string) string {
	switch col.Type {
	case schemadef.TypeInt, schemadef.TypeFloat, schemadef.TypeBool:
		return v
	}
	return sqlString(v)
}

// isUUIDArrayColumn reports whether the column is a uuid[]. Introspection
// spells the declared type of an array with its element type and a `[]`
// suffix, which isUUIDColumn (an exact match) deliberately does not accept.
func isUUIDArrayColumn(col schemadef.Column) bool {
	return col.IsArray && strings.EqualFold(strings.TrimSpace(col.DeclType), "UUID[]")
}
