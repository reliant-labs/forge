package conventions

import (
	"maps"
	"slices"
)

// Normalize applies Rules to one span's attributes and returns the result,
// leaving attrs untouched. traceID is the span's trace id in lowercase hex
// (the TraceID source). It is the reference semantics of the OTTL
// statements: rules apply in order, each adds its key only when absent, and
// nothing present is changed. This legacy entry point handles string-only
// attributes; real OTLP spans should use NormalizeAttributes.
//
// Deprecated: use NormalizeAttributes for typed OTLP attributes.
func Normalize(traceID string, attrs map[string]string) map[string]string {
	return normalize(traceID, attrs)
}

// NormalizeAttributes applies Rules while preserving OTLP value types. Integer
// usage counts are int64 and costs are float64; string conditions match only
// string values. The input map is not modified.
func NormalizeAttributes(traceID string, attrs map[string]any) map[string]any {
	return normalize(traceID, attrs)
}

func normalize[V any](traceID string, attrs map[string]V) map[string]V {
	out := maps.Clone(attrs)
	if out == nil {
		out = map[string]V{}
	}
	for _, r := range Rules() {
		if _, ok := out[r.Set]; ok || !holds(out, r.When) {
			continue
		}
		switch {
		case r.Value != "":
			if v, ok := any(r.Value).(V); ok {
				out[r.Set] = v
			}
		case r.Copy == TraceID:
			if v, ok := any(traceID).(V); ok && traceID != "" {
				out[r.Set] = v
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
func holds[V any](attrs map[string]V, when []Cond) bool {
	for _, c := range when {
		v, ok := attrs[c.Key]
		text, isString := any(v).(string)
		if !ok || any(v) == nil || (c.Equals != "" && (!isString || text != c.Equals)) {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}
