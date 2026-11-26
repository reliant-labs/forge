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
//   - a Grafana Alloy (River) config is kept as its component block headers
//     alone (`pyroscope.ebpf "default" {}`): every attribute, and so every
//     URL, password and header it sets, is dropped;
//   - container env literal values are blanked unless the name is in
//     EnvAllowlist, in pod specs and in custom objects' `env` lists alike;
//   - container command and args keep their shape (discovery reads model
//     URLs, config paths and flags from them) with every credential in them
//     replaced (RedactArgs), and HTTP probe and lifecycle header values are
//     replaced, their names kept.

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// Redacted replaces a sensitive scalar; Dropped replaces data the collector
// cannot parse and therefore cannot redact.
const (
	Redacted = "REDACTED"
	Dropped  = "DROPPED: not JSON, YAML, a DCGM counters CSV or an Alloy config"
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
	if skel, ok := alloySkeleton(v); ok {
		return skel
	}
	return Dropped
}

// alloyBlock matches an Alloy component block header, `name "label" {`, as
// mldiscover.ParseAlloyConfig does.
var alloyBlock = regexp.MustCompile(`(?m)^\s*([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)\s+"([^"]*)"\s*\{`)

// alloyLabel is a label kept as it is; any other becomes "".
var alloyLabel = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// AlloySkeletonHeader opens what is kept of an Alloy config.
const AlloySkeletonHeader = "// Grafana Alloy config: forge ml discover keeps only its component blocks; every attribute was dropped."

// alloySkeleton is an Alloy (River) config reduced to its component block
// headers, each an empty block: the components it runs are what discovery
// reads (mldiscover.ParseAlloyConfig), and nothing inside a block is kept,
// so neither is a credential. ok is false when v declares no component.
func alloySkeleton(v string) (string, bool) {
	ms := alloyBlock.FindAllStringSubmatch(v, -1)
	if len(ms) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString(AlloySkeletonHeader + "\n")
	for _, m := range ms {
		label := m[2]
		if !alloyLabel.MatchString(label) {
			label = ""
		}
		b.WriteString(m[1] + " " + strconv.Quote(label) + " {}\n")
	}
	return b.String(), true
}

// sensitiveArg names what an argument must never carry a value for:
// SensitiveKey's words plus a presigned URL's signature.
var sensitiveArg = `(?:token|password|passwd|pwd|secret|key|auth|authorization|header|credential|bearer|cookie|signature)`

// argValue is a value as a shell or a URL delimits it.
const argValue = `("[^"]*"|'[^']*'|[^\s"'&;]+)`

var (
	// --token=v, HF_TOKEN=v, ?X-Amz-Signature=v
	argAssign = regexp.MustCompile(`(?i)((?:^|[\s"'?&;(])-{0,2}[A-Za-z0-9_.-]*` + sensitiveArg + `[A-Za-z0-9_.-]*=)` + argValue)
	// --token v, inside one argument (a shell script)
	argSpaced = regexp.MustCompile(`(?i)((?:^|\s)-{1,2}[A-Za-z0-9_.-]*` + sensitiveArg + `[A-Za-z0-9_.-]*\s+)("[^"]*"|'[^']*'|[^\s"'-][^\s"']*)`)
	// Authorization: Bearer v, X-Api-Key: v
	argHeader = regexp.MustCompile(`(?i)([A-Za-z0-9_-]*` + sensitiveArg + `[A-Za-z0-9_-]*\s*:\s*)[^"'\n;]+`)
	// scheme://user:password@host
	argUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/\s:@"']*:)[^/\s@"']+@`)
	// a bare flag that takes the next argument as its value: --token, -p
	argSensitiveFlag = regexp.MustCompile(`(?i)^-{1,2}[A-Za-z0-9_.-]*` + sensitiveArg + `[A-Za-z0-9_.-]*$`)
)

// RedactArgs is a container's command or args with every credential in them
// replaced by Redacted: the value of a flag or variable named like a
// credential (`--token=v`, `--token v`, `HF_TOKEN=v`, `?X-Amz-Signature=v`),
// a sensitive HTTP header written into a command (`Authorization: Bearer
// v`), and the password of a URL's userinfo. Everything else is kept: model
// URLs, config paths and flags are what discovery reads.
func RedactArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		if i > 0 && argSensitiveFlag.MatchString(args[i-1]) && !strings.HasPrefix(a, "-") {
			out[i] = Redacted
			continue
		}
		out[i] = RedactArgText(a)
	}
	return out
}

// RedactArgText is one argument (or a shell script in one) with its
// credentials replaced, by RedactArgs' rules.
func RedactArgText(a string) string {
	a = argUserinfo.ReplaceAllString(a, "${1}"+Redacted+"@")
	a = argHeader.ReplaceAllString(a, "${1}"+Redacted)
	a = argAssign.ReplaceAllString(a, "${1}"+Redacted)
	a = argSpaced.ReplaceAllString(a, "${1}"+Redacted)
	return a
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
// OpenTelemetryCollector's `config: |`) is redacted as one; any other string
// has its credentials replaced by RedactArgText's rules.
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
		// A custom object's args, a collector's endpoint URL: the same
		// credentials a container's command can carry.
		return RedactArgText(t)
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
