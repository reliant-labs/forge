package conventions

import "github.com/reliant-labs/forge/pkg/observe/genai"

// Version is the table's version. It moves whenever Rules changes what a
// span becomes, so a consumer that generated its collector config from an
// older table can say which one it runs.
const Version = 1

// The values of genai.AttrConvention.
const (
	// GenAI is the OpenTelemetry GenAI conventions, forge's own.
	GenAI = "genai"
	// OpenInference is Arize's OpenInference.
	OpenInference = "openinference"
	// OpenLLMetry is Traceloop's OpenLLMetry.
	OpenLLMetry = "openllmetry"
)

// TraceID is the Rule.Copy source that means the span's trace id (lowercase
// hex) rather than an attribute.
const TraceID = "$trace_id"

// Cond is one condition on a span attribute: present, when Equals is empty,
// else equal to Equals.
type Cond struct {
	Key    string
	Equals string
}

// Rule adds one canonical attribute. It applies when the span does not carry
// Set yet and every When condition holds; it then copies the value of the
// attribute Copy (and applies only when Copy is present), or sets the
// constant Value. Exactly one of Copy and Value is non-empty.
type Rule struct {
	// Set is the canonical key the rule adds.
	Set string
	// Copy is the source attribute, or TraceID.
	Copy string
	// Value is the constant the rule sets.
	Value string
	// When are the conditions, all of which must hold.
	When []Cond
	// Unverified marks a source name forge could not check against the
	// convention's own source; Note says what.
	Unverified bool
	// Note explains the rule where its name alone does not.
	Note string
}

// The attribute names this table reads, by convention. Only those the rules
// below use are named.
const (
	oiSpanKind          = "openinference.span.kind"
	oiModelName         = "llm.model_name"
	oiRequestModelName  = "llm.request.model_name"
	oiResponseModelName = "llm.response.model_name"
	oiProvider          = "llm.provider"
	oiSystem            = "llm.system"
	oiTokensPrompt      = "llm.token_count.prompt"
	oiTokensCompletion  = "llm.token_count.completion"
	oiTokensCacheRead   = "llm.token_count.prompt_details.cache_read"
	oiTokensCacheWrite  = "llm.token_count.prompt_details.cache_write"
	oiTokensReasoning   = "llm.token_count.completion_details.reasoning"
	oiCostTotal         = "llm.cost.total"
	oiSessionID         = "session.id"
	oiAgentName         = "agent.name"
	oiToolName          = "tool.name"
	oiToolCallID        = "tool_call.id"
	oiGraphNodeID       = "graph.node.id"

	tlSpanKind     = "traceloop.span.kind"
	tlEntityName   = "traceloop.entity.name"
	tlWorkflowName = "traceloop.workflow.name"
	tlRequestType  = "llm.request.type"
	tlReasoning    = "llm.usage.reasoning_tokens"
	tlAssocSession = "traceloop.association.properties.session_id"
	tlAssocUser    = "traceloop.association.properties.user_id"

	// The GenAI conventions' renamed and draft names.
	gaPromptTokens     = "gen_ai.usage.prompt_tokens"
	gaCompletionTokens = "gen_ai.usage.completion_tokens"
	gaReasoningTokens  = "gen_ai.usage.reasoning_tokens"
	gaCacheWrite       = "gen_ai.usage.cache_write.input_tokens"
)

// Rules is the mapping, in the order it is applied. Order matters twice:
// the convention is detected first, from the span's original attributes, so
// the later rules can be confined to one convention; and of several sources
// for one key, the first present wins (every rule skips a key already set).
func Rules() []Rule {
	var r []Rule
	add := func(rs ...Rule) { r = append(r, rs...) }
	isOI := Cond{Key: genai.AttrConvention, Equals: OpenInference}
	isTL := Cond{Key: genai.AttrConvention, Equals: OpenLLMetry}

	// 1. Which convention the span arrived in. OpenLLMetry before GenAI:
	// its spans carry gen_ai.* names too.
	add(
		detect(OpenInference, oiSpanKind),
		detect(OpenLLMetry, tlSpanKind),
		detect(OpenLLMetry, tlRequestType),
		detect(OpenLLMetry, tlWorkflowName),
		detect(GenAI, genai.AttrOperationName),
		detect(GenAI, genai.AttrProviderName),
		detect(GenAI, genai.AttrSystem),
		detect(GenAI, genai.AttrRequestModel),
	)

	// 2. The operation, which is what makes a span an AI span to every
	// reader (and to the gateway's keep-AI-traces sampling policy).
	add(
		mapValues(genai.AttrOperationName, oiSpanKind, []Cond{isOI}, map[string]string{
			"LLM": genai.OpChat, "EMBEDDING": genai.OpEmbeddings,
			"AGENT": genai.OpInvokeAgent, "TOOL": genai.OpExecuteTool,
		}, "CHAIN, RETRIEVER, RERANKER, GUARDRAIL, EVALUATOR and the other kinds have no GenAI operation and stay unmapped")...,
	)
	add(
		mapValues(genai.AttrOperationName, tlRequestType, []Cond{isTL}, map[string]string{
			"chat": genai.OpChat, "completion": genai.OpTextCompletion, "embedding": genai.OpEmbeddings,
		}, "rerank has no GenAI operation")...,
	)
	add(
		mapValues(genai.AttrOperationName, tlSpanKind, []Cond{isTL}, map[string]string{
			"workflow": genai.OpInvokeWorkflow, "agent": genai.OpInvokeAgent, "tool": genai.OpExecuteTool,
		}, "task has no GenAI operation")...,
	)

	// 3. The provider. gen_ai.system's renamed values first (the
	// conventions' deprecated registry), then the rest as they are.
	add(mapValues(genai.AttrProviderName, genai.AttrSystem, nil, map[string]string{
		"az.ai.inference": "azure.ai.inference", "az.ai.openai": "azure.ai.openai",
		"vertex_ai": "gcp.vertex_ai", "gemini": "gcp.gemini",
	}, "the values gen_ai.system renamed when it became gen_ai.provider.name")...)
	add(copyRule(genai.AttrProviderName, genai.AttrSystem))
	// OpenInference names the hosting provider (llm.provider) and the API
	// shape (llm.system); the provider wins. Values with no single GenAI
	// equivalent (azure) are copied as they are.
	oiProviders := map[string]string{
		"aws": "aws.bedrock", "google": "gcp.gen_ai", "vertexai": "gcp.vertex_ai",
		"xai": "x_ai", "mistralai": "mistral",
	}
	add(mapValues(genai.AttrProviderName, oiProvider, []Cond{isOI}, oiProviders, "")...)
	add(copyRule(genai.AttrProviderName, oiProvider, isOI))
	add(mapValues(genai.AttrProviderName, oiSystem, []Cond{isOI}, oiProviders, "")...)
	add(copyRule(genai.AttrProviderName, oiSystem, isOI))

	// 4. The model.
	add(
		copyRule(genai.AttrRequestModel, oiRequestModelName, isOI),
		copyRule(genai.AttrRequestModel, oiModelName, isOI),
		copyRule(genai.AttrResponseModel, oiResponseModelName, isOI),
	)

	// 5. Tokens.
	add(
		copyRule(genai.AttrUsageInputTokens, gaPromptTokens),
		copyRule(genai.AttrUsageInputTokens, oiTokensPrompt, isOI),
		copyRule(genai.AttrUsageOutputTokens, gaCompletionTokens),
		copyRule(genai.AttrUsageOutputTokens, oiTokensCompletion, isOI),
		copyRule(genai.AttrUsageCacheReadInputTokens, genai.AttrUsageCacheReadInputTokensLegacy),
		copyRule(genai.AttrUsageCacheReadInputTokens, oiTokensCacheRead, isOI),
		copyRule(genai.AttrUsageCacheCreationInputTokens, genai.AttrUsageCacheCreationInputTokensLegacy),
		Rule{Set: genai.AttrUsageCacheCreationInputTokens, Copy: gaCacheWrite, Unverified: true,
			Note: "the GenAI conventions' own repository (semantic-conventions-genai) names the cache-write count cache_write.input_tokens where semconv v1.40.0 named it cache_creation.input_tokens; whether that is a formal rename is unverified"},
		copyRule(genai.AttrUsageCacheCreationInputTokens, oiTokensCacheWrite, isOI),
		copyRule(genai.AttrUsageReasoningOutputTokens, gaReasoningTokens),
		copyRule(genai.AttrUsageReasoningOutputTokens, tlReasoning, isTL),
		copyRule(genai.AttrUsageReasoningOutputTokens, oiTokensReasoning, isOI),
	)

	// 6. A cost the instrumentation reported. Reported cost always wins
	// over an estimate, which readers compute at query time.
	add(copyRule(genai.AttrCostUSD, oiCostTotal, isOI))

	// 7. Agents, tools, workflows and nodes.
	add(
		copyRule(genai.AttrAgentName, oiAgentName, isOI),
		copyRule(genai.AttrAgentName, tlEntityName, isTL, Cond{Key: tlSpanKind, Equals: "agent"}),
		copyRule(genai.AttrToolName, oiToolName, isOI),
		copyRule(genai.AttrToolName, tlEntityName, isTL, Cond{Key: tlSpanKind, Equals: "tool"}),
		copyRule(genai.AttrToolCallID, oiToolCallID, isOI),
		copyRule(genai.AttrNodeID, oiGraphNodeID, isOI),
		copyRule(genai.AttrWorkflowName, tlWorkflowName, isTL),
		Rule{Set: genai.AttrRunID, Copy: TraceID, When: []Cond{isTL, {Key: tlWorkflowName}},
			Note: "OpenLLMetry has no run id: a workflow run is one trace, so the trace id groups it the way the AI hub groups a run"},
	)

	// 8. Who the call was for.
	add(
		copyRule(genai.AttrConversationID, oiSessionID, isOI),
		Rule{Set: genai.AttrConversationID, Copy: tlAssocSession, When: []Cond{isTL}, Unverified: true,
			Note: "OpenLLMetry's association properties: the traceloop.association.properties prefix is verified, the session_id key name is not"},
		Rule{Set: genai.AttrUserID, Copy: tlAssocUser, When: []Cond{isTL}, Unverified: true,
			Note: "OpenLLMetry's association properties: the traceloop.association.properties prefix is verified, the user_id key name is not"},
	)
	// OpenInference's user.id is already the canonical key: nothing to add.
	return r
}

// detect is the rule that names a span's convention when key is present.
func detect(convention, key string) Rule {
	return Rule{Set: genai.AttrConvention, Value: convention, When: []Cond{{Key: key}}}
}

// copyRule copies from into set when the conditions hold.
func copyRule(set, from string, when ...Cond) Rule {
	return Rule{Set: set, Copy: from, When: when}
}

// mapValues is one constant rule per value of from: set gets values[v] when
// from equals v. Values are applied in sorted order so the table is stable.
func mapValues(set, from string, when []Cond, values map[string]string, note string) []Rule {
	out := make([]Rule, 0, len(values))
	for _, v := range sortedKeys(values) {
		w := append(append([]Cond(nil), when...), Cond{Key: from, Equals: v})
		out = append(out, Rule{Set: set, Value: values[v], When: w, Note: note})
	}
	return out
}
