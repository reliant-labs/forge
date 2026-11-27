// Package conventions maps the other LLM span conventions onto the
// canonical attributes of package genai (the OpenTelemetry GenAI semantic
// conventions plus forge.ai.*), so every reader — the AI hub, release
// health, the trace view — reads one shape whatever SDK emitted the span.
//
// The mapping is data: Rules, an ordered table. It is applied at INGEST, by
// the collector, never at query time:
//
//   - OTTL compiles the table into transform-processor statements. The
//     hosted gateway runs them (control-plane's transform/genai_normalize),
//     and forge's KCL module carries the same list as
//     forge.GENAI_NORMALIZE_SPAN_STATEMENTS (kcl/genai_normalize_gen.k), so
//     the collector config is generated from this table, never hand-copied.
//   - NormalizeAttributes applies the table to typed OTLP attributes in Go.
//     It is the reference semantics the golden vectors (TypedVectors) pin;
//     a consumer running the OTTL statements proves its collector agrees by
//     feeding it those same int64 token counts and float64 costs. Normalize
//     and Vectors remain available for legacy string-only callers.
//
// # Additive, never destructive
//
// A rule adds a canonical key only when the span does not carry it already.
// Nothing is overwritten, renamed or deleted: the original attributes stay,
// so a customer's own queries over OpenInference or OpenLLMetry names keep
// working, and a span that already carries a canonical key keeps its value.
// Every normalised span also gets genai.AttrConvention, naming the
// convention it arrived in.
//
// # Conventions read
//
//   - genai: the OpenTelemetry GenAI conventions, including the names they
//     renamed (gen_ai.system, gen_ai.usage.prompt_tokens, ...) and the draft
//     spellings of the cache and reasoning token counts.
//   - openinference: Arize's OpenInference (openinference.span.kind, llm.*,
//     session.id, agent.name, tool.name, graph.node.id).
//   - openllmetry: Traceloop's OpenLLMetry (traceloop.*, llm.request.type,
//     and the older gen_ai.* names it still emits).
//
// A convention's generic keys (session.id, user.id, agent.name, tool.name)
// are mapped only on a span already identified as that convention, so a
// browser span's session.id never becomes a conversation id.
//
// Names forge could not verify against the convention's own source are
// marked Rule.Unverified, with what is unverified in Rule.Note.
//
// # Not here yet
//
// Message content (OpenInference's llm.input_messages.*, OpenLLMetry's
// gen_ai.prompt.*) is not mapped: content capture is a later, opt-in phase
// with its own storage and gateway enforcement, and reassembling indexed
// message lists needs a custom OTTL function. Langfuse's langfuse.* and the
// Vercel AI SDK's ai.* attributes are later rows of the same table.
package conventions
