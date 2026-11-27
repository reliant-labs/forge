package conventions

import (
	"fmt"
	"strings"
)

// kclFile is kcl/genai_normalize_gen.k: OTTL() as a KCL list, so a collector
// config written in KCL reads forge.GENAI_NORMALIZE_SPAN_STATEMENTS instead
// of a copy of this table.
func kclFile() string {
	var b strings.Builder
	b.WriteString(`# GENERATED from pkg/observe/genai/conventions. DO NOT EDIT.
# Regenerate: go test ./pkg/observe/genai/conventions -run TestKCLStatements -update
#
# The LLM span conventions (OpenInference, OpenLLMetry, the GenAI names the
# OpenTelemetry conventions renamed) normalised onto forge's canonical
# gen_ai.* and forge.ai.* keys, as transform-processor statements for a span
# context. Each statement only ADDS a canonical key the span lacks; nothing is
# overwritten or deleted. Run them with error_mode = "ignore":
#
#     "transform/genai_normalize" = {
#         error_mode = "ignore"
#         trace_statements = [{context = "span", statements = forge.GENAI_NORMALIZE_SPAN_STATEMENTS}]
#     }
#
# GENAI_NORMALIZE_VERSION is the table's version (conventions.Version).

`)
	fmt.Fprintf(&b, "GENAI_NORMALIZE_VERSION: int = %d\n\n", Version)
	b.WriteString("GENAI_NORMALIZE_SPAN_STATEMENTS: [str] = [\n")
	for _, s := range OTTL() {
		fmt.Fprintf(&b, "    '%s'\n", s)
	}
	b.WriteString("]\n")
	return b.String()
}
