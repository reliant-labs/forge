package conventions

import (
	"fmt"
	"strings"
)

// OTTL compiles Rules into OpenTelemetry Transformation Language statements
// for a transform processor's span context, one per rule, in order:
//
//	set(attributes["gen_ai.request.model"], attributes["llm.model_name"]) where attributes["gen_ai.request.model"] == nil and attributes["llm.model_name"] != nil and attributes["forge.ai.convention"] == "openinference"
//
// Attribute-to-attribute set preserves OTLP types, including int64 token
// counts and double costs; do not wrap copies in String or parse integers
// through floating point. TypedVectors supplies typed collector test inputs.
// Run them with error_mode ignore: a statement that cannot apply to one span
// must never drop the span. The result has no single quote or backslash, so
// it embeds in KCL or YAML single-quoted strings as is.
func OTTL() []string {
	rules := Rules()
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, statement(r))
	}
	return out
}

func statement(r Rule) string {
	var value string
	conds := []string{attr(r.Set) + " == nil"}
	switch {
	case r.Value != "":
		value = lit(r.Value)
	case r.Copy == TraceID:
		value = "trace_id.string"
	default:
		value = attr(r.Copy)
		conds = append(conds, attr(r.Copy)+" != nil")
	}
	for _, c := range r.When {
		if c.Equals == "" {
			conds = append(conds, attr(c.Key)+" != nil")
		} else {
			conds = append(conds, attr(c.Key)+" == "+lit(c.Equals))
		}
	}
	return fmt.Sprintf("set(%s, %s) where %s", attr(r.Set), value, strings.Join(conds, " and "))
}

func attr(key string) string { return "attributes[" + lit(key) + "]" }

// lit is an OTTL string literal. The table holds attribute names and enum
// values only; validate (conventions_test.go) refuses anything that would
// need escaping.
func lit(s string) string { return `"` + s + `"` }
