//go:build ignore

package middleware

import (
	"fmt"
	"reflect"
	"strings"
)

const redactedPlaceholder = "[REDACTED]"

// RedactFields returns a map representation of the struct v with the named
// fields replaced by "[REDACTED]". Non-struct values are returned as-is
// (wrapped in a map with key "value"). Unknown field names are silently
// ignored so callers don't need to track struct evolution.
//
// This is a convenience for structured logging — pass the result to
// slog.Any("payload", RedactFields(req, "password", "ssn")) to log the
// request without leaking PII.
//
// Only exported fields are included. Nested structs are not recursively
// redacted; pass them separately if needed.
func RedactFields(v any, fields ...string) map[string]any {
	if v == nil {
		return nil
	}

	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return map[string]any{"value": fmt.Sprint(v)}
	}

	redactSet := make(map[string]bool, len(fields))
	for _, f := range fields {
		redactSet[strings.ToLower(f)] = true
	}

	rt := rv.Type()
	out := make(map[string]any, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		key := sf.Name
		if redactSet[strings.ToLower(key)] {
			out[key] = redactedPlaceholder
		} else {
			out[key] = rv.Field(i).Interface()
		}
	}
	return out
}
