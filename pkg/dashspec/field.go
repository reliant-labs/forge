package dashspec

import (
	"fmt"
	"strings"
)

// CheckField reports whether field is a semantic field on signal (the
// vocabulary documented on FieldService). Every renderer maps exactly these
// fields, so a field CheckField accepts is one each can draw.
func CheckField(sig Signal, field string) error {
	switch {
	case field == FieldService, field == FieldEnv:
		return nil
	case strings.HasPrefix(field, "resource.attr."):
		return nil
	}
	switch sig {
	case Traces:
		switch field {
		case "timestamp", "span.name", "span.kind", "span.status", "span.status_message", "span.duration":
			return nil
		}
		if strings.HasPrefix(field, "span.attr.") {
			return nil
		}
	case Logs:
		switch field {
		case "timestamp", "log.severity", "log.body":
			return nil
		}
		if strings.HasPrefix(field, "log.attr.") {
			return nil
		}
	case Metrics:
		if strings.HasPrefix(field, "metric.attr.") {
			return nil
		}
	}
	return fmt.Errorf("field %q is not valid on %s", field, sig)
}
