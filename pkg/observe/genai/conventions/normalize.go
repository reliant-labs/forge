package conventions

import (
	"maps"
	"slices"
)

// Normalize applies Rules to one span's attributes and returns the result,
// leaving attrs untouched. traceID is the span's trace id in lowercase hex
// (the TraceID source). It is the reference semantics of the OTTL
// statements: rules apply in order, each adds its key only when absent, and
// nothing present is changed.
func Normalize(traceID string, attrs map[string]string) map[string]string {
	out := maps.Clone(attrs)
	if out == nil {
		out = map[string]string{}
	}
	for _, r := range Rules() {
		if _, ok := out[r.Set]; ok || !holds(out, r.When) {
			continue
		}
		switch {
		case r.Value != "":
			out[r.Set] = r.Value
		case r.Copy == TraceID:
			if traceID != "" {
				out[r.Set] = traceID
			}
		default:
			if v, ok := out[r.Copy]; ok {
				out[r.Set] = v
			}
		}
	}
	return out
}

// holds reports whether every condition holds on attrs.
func holds(attrs map[string]string, when []Cond) bool {
	for _, c := range when {
		v, ok := attrs[c.Key]
		if !ok || (c.Equals != "" && v != c.Equals) {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}
