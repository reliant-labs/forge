package orm

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/lib/pq"
)

// Array support for generated ORM code.
//
// Postgres stores slice columns as native arrays (TEXT[], BIGINT[]).
// ArrayValue wraps the slice as a postgres array parameter (pq.Array,
// which both lib/pq and pgx-stdlib bind correctly); StringArray/Int64Array
// scan the postgres text format `{a,b}` (and tolerate JSON `["a","b"]`
// from legacy data), so generated scan code is driver-agnostic.

// ArrayValue converts a slice for use as a SQL parameter. forge is
// postgres-pinned: the slice is wrapped with pq.Array, which renders the
// postgres array literal both lib/pq and pgx-stdlib bind to a native
// array column (TEXT[], BIGINT[]). A bare []string is NOT a valid
// database/sql driver value — passing it through unwrapped makes the
// driver reject the INSERT.
//
// The Dialect argument is retained for the generated call sites (and the
// Phase-2 engine swap); postgres is the only dialect.
func ArrayValue(d Dialect, v any) any {
	return pq.Array(v)
}

// StringArray scans a string-array column from either encoding.
type StringArray []string

// Scan implements sql.Scanner.
func (a *StringArray) Scan(src any) error {
	s, ok := arrayText(src)
	if !ok {
		return fmt.Errorf("orm: cannot scan %T into StringArray", src)
	}
	if s == "" || s == "null" {
		*a = nil
		return nil
	}
	if strings.HasPrefix(s, "[") {
		return json.Unmarshal([]byte(s), a)
	}
	if strings.HasPrefix(s, "{") {
		parts, err := parsePGTextArray(s)
		if err != nil {
			return err
		}
		*a = parts
		return nil
	}
	return fmt.Errorf("orm: unrecognized array encoding %q", s)
}

// Int64Array scans an integer-array column from either encoding.
type Int64Array []int64

// Scan implements sql.Scanner.
func (a *Int64Array) Scan(src any) error {
	s, ok := arrayText(src)
	if !ok {
		return fmt.Errorf("orm: cannot scan %T into Int64Array", src)
	}
	if s == "" || s == "null" {
		*a = nil
		return nil
	}
	if strings.HasPrefix(s, "[") {
		return json.Unmarshal([]byte(s), a)
	}
	if strings.HasPrefix(s, "{") {
		parts, err := parsePGTextArray(s)
		if err != nil {
			return err
		}
		out := make([]int64, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return fmt.Errorf("orm: array element %q is not an integer: %w", p, err)
			}
			out = append(out, n)
		}
		*a = out
		return nil
	}
	return fmt.Errorf("orm: unrecognized array encoding %q", s)
}

func arrayText(src any) (string, bool) {
	switch v := src.(type) {
	case nil:
		return "", true
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	return "", false
}

// parsePGTextArray parses the postgres text array format `{a,"b c",d}`.
// Multidimensional arrays are not supported (forge never emits them).
func parsePGTextArray(s string) ([]string, error) {
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("orm: malformed postgres array %q", s)
	}
	body := s[1 : len(s)-1]
	if body == "" {
		return nil, nil
	}
	var (
		out []string
		cur strings.Builder
	)
	i := 0
	for i < len(body) {
		switch body[i] {
		case '"':
			i++
			for i < len(body) {
				if body[i] == '\\' && i+1 < len(body) {
					cur.WriteByte(body[i+1])
					i += 2
					continue
				}
				if body[i] == '"' {
					i++
					break
				}
				cur.WriteByte(body[i])
				i++
			}
		case ',':
			out = append(out, cur.String())
			cur.Reset()
			i++
		default:
			cur.WriteByte(body[i])
			i++
		}
	}
	out = append(out, cur.String())
	for j, p := range out {
		if p == "NULL" {
			out[j] = ""
		}
	}
	return out, nil
}
