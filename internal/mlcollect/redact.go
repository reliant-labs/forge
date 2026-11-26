package mlcollect

// Redaction: what a snapshot keeps of a ConfigMap's data and a custom
// object's spec. A snapshot is written to .forge/state and may be shared as a
// fixture or with a bug report, so it must not carry a credential someone put
// in a config file.
//
// The rules (the ML proposal's coverage addendum, "Safety"):
//
//   - JSON or YAML that parses to a map or a list is kept, with every scalar
//     under a key matching SensitiveKey replaced by Redacted, at any depth
//     (a map under `headers:` has all its values replaced, its keys kept);
//   - a DCGM counters CSV (lines of `DCGM_FI_…, type, help`) is kept, minus
//     its comment lines;
//   - anything else (a bare string, a script, a PEM block, binaryData) is
//     dropped: the key stays, its value becomes Dropped;
//   - a data key that itself matches SensitiveKey is replaced whole;
//   - container env literal values are blanked unless the name is in
//     EnvAllowlist, in pod specs and in custom objects' `env` lists alike.

import (
	"encoding/json"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Redacted replaces a sensitive scalar; Dropped replaces data the collector
// cannot parse and therefore cannot redact.
const (
	Redacted = "REDACTED"
	Dropped  = "DROPPED: not JSON, YAML or a DCGM counters CSV"
)

// SensitiveKey matches a key whose values are never kept.
var SensitiveKey = regexp.MustCompile(`(?i)(token|password|passwd|secret|key|auth|authorization|header|credential|bearer|cookie)`)

// EnvAllowlist is the env var names whose literal values are kept: they name
// a counters file, switch on pod attribution, or set an interval, and carry
// nothing sensitive.
var EnvAllowlist = map[string]bool{
	"DCGM_EXPORTER_COLLECTORS": true,
	"DCGM_EXPORTER_KUBERNETES": true,
	"DCGM_EXPORTER_INTERVAL":   true,
}

// RedactConfigMap applies the rules to a ConfigMap read by name, in place.
func RedactConfigMap(cm *corev1.ConfigMap) {
	strip(&cm.ObjectMeta)
	for k, v := range cm.Data {
		if SensitiveKey.MatchString(k) {
			cm.Data[k] = Redacted
			continue
		}
		cm.Data[k] = RedactText(v)
	}
	if len(cm.BinaryData) > 0 {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		for k := range cm.BinaryData {
			cm.Data[k] = Dropped
		}
		cm.BinaryData = nil
	}
}

// RedactText is one data value, redacted: structured JSON/YAML with sensitive
// values replaced, a DCGM CSV without comments, or Dropped.
func RedactText(v string) string {
	if strings.TrimSpace(v) == "" {
		return v
	}
	if csv, ok := dcgmCSV(v); ok {
		return csv
	}
	trimmed := strings.TrimSpace(v)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var doc any
		if json.Unmarshal([]byte(trimmed), &doc) == nil && structured(doc) {
			b, err := json.MarshalIndent(redactValue(doc, false), "", "  ")
			if err == nil {
				return string(b)
			}
		}
	}
	var doc any
	if yaml.Unmarshal([]byte(v), &doc) == nil && structured(doc) {
		if b, err := yaml.Marshal(redactValue(doc, false)); err == nil {
			return string(b)
		}
	}
	return Dropped
}

func structured(doc any) bool {
	switch doc.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// redactValue walks a decoded document. Inside a sensitive key every scalar
// is replaced; a string that is itself a YAML/JSON document (an
// OpenTelemetryCollector's `config: |`) is redacted as one.
func redactValue(v any, sensitive bool) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = redactValue(x, sensitive || SensitiveKey.MatchString(k))
		}
		redactEnvList(out)
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redactValue(x, sensitive)
		}
		return out
	case nil:
		return nil
	case string:
		if sensitive {
			return Redacted
		}
		if strings.Contains(t, "\n") && strings.Contains(t, ":") {
			var doc any
			if yaml.Unmarshal([]byte(t), &doc) == nil && structured(doc) {
				if b, err := yaml.Marshal(redactValue(doc, false)); err == nil {
					return string(b)
				}
			}
		}
		return t
	default:
		if sensitive {
			return Redacted
		}
		return t
	}
}

// redactEnvList blanks the literal values of an `env: [{name, value}]` list in
// a decoded object (a ClusterPolicy's spec.dcgmExporter.env, a collector CR's
// spec.env), keeping EnvAllowlist.
func redactEnvList(m map[string]any) {
	env, ok := m["env"].([]any)
	if !ok {
		return
	}
	for _, e := range env {
		item, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name, _ := item["name"].(string)
		if _, has := item["value"]; has && !EnvAllowlist[name] {
			item["value"] = ""
		}
	}
}

// RedactObject redacts a custom object's content (everything but its
// metadata, which strip already handled).
func RedactObject(obj map[string]any) map[string]any {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if k == "metadata" || k == "apiVersion" || k == "kind" {
			out[k] = v
			continue
		}
		out[k] = redactValue(v, SensitiveKey.MatchString(k))
	}
	return out
}

var dcgmCSVLine = regexp.MustCompile(`^\s*DCGM_[A-Z0-9_]+\s*,`)

// dcgmCSV recognises a DCGM counters CSV: every non-blank, non-comment line
// starts with a DCGM field name and a comma. Comment lines are dropped.
func dcgmCSV(v string) (string, bool) {
	var kept []string
	for _, line := range strings.Split(v, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if !dcgmCSVLine.MatchString(t) {
			return "", false
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		return "", false
	}
	return strings.Join(kept, "\n") + "\n", true
}
