# GENERATED from pkg/observe/genai/conventions. DO NOT EDIT.
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

GENAI_NORMALIZE_VERSION: int = 1

GENAI_NORMALIZE_SPAN_STATEMENTS: [str] = [
    'set(attributes["forge.ai.convention"], "openinference") where attributes["forge.ai.convention"] == nil and attributes["openinference.span.kind"] != nil'
    'set(attributes["forge.ai.convention"], "openllmetry") where attributes["forge.ai.convention"] == nil and attributes["traceloop.span.kind"] != nil'
    'set(attributes["forge.ai.convention"], "openllmetry") where attributes["forge.ai.convention"] == nil and attributes["llm.request.type"] != nil'
    'set(attributes["forge.ai.convention"], "openllmetry") where attributes["forge.ai.convention"] == nil and attributes["traceloop.workflow.name"] != nil'
    'set(attributes["forge.ai.convention"], "genai") where attributes["forge.ai.convention"] == nil and attributes["gen_ai.operation.name"] != nil'
    'set(attributes["forge.ai.convention"], "genai") where attributes["forge.ai.convention"] == nil and attributes["gen_ai.provider.name"] != nil'
    'set(attributes["forge.ai.convention"], "genai") where attributes["forge.ai.convention"] == nil and attributes["gen_ai.system"] != nil'
    'set(attributes["forge.ai.convention"], "genai") where attributes["forge.ai.convention"] == nil and attributes["gen_ai.request.model"] != nil'
    'set(attributes["gen_ai.operation.name"], "invoke_agent") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["openinference.span.kind"] == "AGENT"'
    'set(attributes["gen_ai.operation.name"], "embeddings") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["openinference.span.kind"] == "EMBEDDING"'
    'set(attributes["gen_ai.operation.name"], "chat") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["openinference.span.kind"] == "LLM"'
    'set(attributes["gen_ai.operation.name"], "execute_tool") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["openinference.span.kind"] == "TOOL"'
    'set(attributes["gen_ai.operation.name"], "chat") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["llm.request.type"] == "chat"'
    'set(attributes["gen_ai.operation.name"], "text_completion") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["llm.request.type"] == "completion"'
    'set(attributes["gen_ai.operation.name"], "embeddings") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["llm.request.type"] == "embedding"'
    'set(attributes["gen_ai.operation.name"], "invoke_agent") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["traceloop.span.kind"] == "agent"'
    'set(attributes["gen_ai.operation.name"], "execute_tool") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["traceloop.span.kind"] == "tool"'
    'set(attributes["gen_ai.operation.name"], "invoke_workflow") where attributes["gen_ai.operation.name"] == nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["traceloop.span.kind"] == "workflow"'
    'set(attributes["gen_ai.provider.name"], "azure.ai.inference") where attributes["gen_ai.provider.name"] == nil and attributes["gen_ai.system"] == "az.ai.inference"'
    'set(attributes["gen_ai.provider.name"], "azure.ai.openai") where attributes["gen_ai.provider.name"] == nil and attributes["gen_ai.system"] == "az.ai.openai"'
    'set(attributes["gen_ai.provider.name"], "gcp.gemini") where attributes["gen_ai.provider.name"] == nil and attributes["gen_ai.system"] == "gemini"'
    'set(attributes["gen_ai.provider.name"], "gcp.vertex_ai") where attributes["gen_ai.provider.name"] == nil and attributes["gen_ai.system"] == "vertex_ai"'
    'set(attributes["gen_ai.provider.name"], attributes["gen_ai.system"]) where attributes["gen_ai.provider.name"] == nil and attributes["gen_ai.system"] != nil'
    'set(attributes["gen_ai.provider.name"], "aws.bedrock") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.provider"] == "aws"'
    'set(attributes["gen_ai.provider.name"], "gcp.gen_ai") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.provider"] == "google"'
    'set(attributes["gen_ai.provider.name"], "mistral") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.provider"] == "mistralai"'
    'set(attributes["gen_ai.provider.name"], "gcp.vertex_ai") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.provider"] == "vertexai"'
    'set(attributes["gen_ai.provider.name"], "x_ai") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.provider"] == "xai"'
    'set(attributes["gen_ai.provider.name"], attributes["llm.provider"]) where attributes["gen_ai.provider.name"] == nil and attributes["llm.provider"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.provider.name"], "aws.bedrock") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.system"] == "aws"'
    'set(attributes["gen_ai.provider.name"], "gcp.gen_ai") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.system"] == "google"'
    'set(attributes["gen_ai.provider.name"], "mistral") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.system"] == "mistralai"'
    'set(attributes["gen_ai.provider.name"], "gcp.vertex_ai") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.system"] == "vertexai"'
    'set(attributes["gen_ai.provider.name"], "x_ai") where attributes["gen_ai.provider.name"] == nil and attributes["forge.ai.convention"] == "openinference" and attributes["llm.system"] == "xai"'
    'set(attributes["gen_ai.provider.name"], attributes["llm.system"]) where attributes["gen_ai.provider.name"] == nil and attributes["llm.system"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.request.model"], attributes["llm.request.model_name"]) where attributes["gen_ai.request.model"] == nil and attributes["llm.request.model_name"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.request.model"], attributes["llm.model_name"]) where attributes["gen_ai.request.model"] == nil and attributes["llm.model_name"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.response.model"], attributes["llm.response.model_name"]) where attributes["gen_ai.response.model"] == nil and attributes["llm.response.model_name"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.usage.input_tokens"], attributes["gen_ai.usage.prompt_tokens"]) where attributes["gen_ai.usage.input_tokens"] == nil and attributes["gen_ai.usage.prompt_tokens"] != nil'
    'set(attributes["gen_ai.usage.input_tokens"], attributes["llm.token_count.prompt"]) where attributes["gen_ai.usage.input_tokens"] == nil and attributes["llm.token_count.prompt"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.usage.output_tokens"], attributes["gen_ai.usage.completion_tokens"]) where attributes["gen_ai.usage.output_tokens"] == nil and attributes["gen_ai.usage.completion_tokens"] != nil'
    'set(attributes["gen_ai.usage.output_tokens"], attributes["llm.token_count.completion"]) where attributes["gen_ai.usage.output_tokens"] == nil and attributes["llm.token_count.completion"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.usage.cache_read.input_tokens"], attributes["gen_ai.usage.cache_read_input_tokens"]) where attributes["gen_ai.usage.cache_read.input_tokens"] == nil and attributes["gen_ai.usage.cache_read_input_tokens"] != nil'
    'set(attributes["gen_ai.usage.cache_read.input_tokens"], attributes["llm.token_count.prompt_details.cache_read"]) where attributes["gen_ai.usage.cache_read.input_tokens"] == nil and attributes["llm.token_count.prompt_details.cache_read"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.usage.cache_creation.input_tokens"], attributes["gen_ai.usage.cache_creation_input_tokens"]) where attributes["gen_ai.usage.cache_creation.input_tokens"] == nil and attributes["gen_ai.usage.cache_creation_input_tokens"] != nil'
    'set(attributes["gen_ai.usage.cache_creation.input_tokens"], attributes["gen_ai.usage.cache_write.input_tokens"]) where attributes["gen_ai.usage.cache_creation.input_tokens"] == nil and attributes["gen_ai.usage.cache_write.input_tokens"] != nil'
    'set(attributes["gen_ai.usage.cache_creation.input_tokens"], attributes["llm.token_count.prompt_details.cache_write"]) where attributes["gen_ai.usage.cache_creation.input_tokens"] == nil and attributes["llm.token_count.prompt_details.cache_write"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.usage.reasoning.output_tokens"], attributes["gen_ai.usage.reasoning_tokens"]) where attributes["gen_ai.usage.reasoning.output_tokens"] == nil and attributes["gen_ai.usage.reasoning_tokens"] != nil'
    'set(attributes["gen_ai.usage.reasoning.output_tokens"], attributes["llm.usage.reasoning_tokens"]) where attributes["gen_ai.usage.reasoning.output_tokens"] == nil and attributes["llm.usage.reasoning_tokens"] != nil and attributes["forge.ai.convention"] == "openllmetry"'
    'set(attributes["gen_ai.usage.reasoning.output_tokens"], attributes["llm.token_count.completion_details.reasoning"]) where attributes["gen_ai.usage.reasoning.output_tokens"] == nil and attributes["llm.token_count.completion_details.reasoning"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["forge.ai.cost.usd"], attributes["llm.cost.total"]) where attributes["forge.ai.cost.usd"] == nil and attributes["llm.cost.total"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.agent.name"], attributes["agent.name"]) where attributes["gen_ai.agent.name"] == nil and attributes["agent.name"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.agent.name"], attributes["traceloop.entity.name"]) where attributes["gen_ai.agent.name"] == nil and attributes["traceloop.entity.name"] != nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["traceloop.span.kind"] == "agent"'
    'set(attributes["gen_ai.tool.name"], attributes["tool.name"]) where attributes["gen_ai.tool.name"] == nil and attributes["tool.name"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.tool.name"], attributes["traceloop.entity.name"]) where attributes["gen_ai.tool.name"] == nil and attributes["traceloop.entity.name"] != nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["traceloop.span.kind"] == "tool"'
    'set(attributes["gen_ai.tool.call.id"], attributes["tool_call.id"]) where attributes["gen_ai.tool.call.id"] == nil and attributes["tool_call.id"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["forge.ai.node.id"], attributes["graph.node.id"]) where attributes["forge.ai.node.id"] == nil and attributes["graph.node.id"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["forge.ai.workflow.name"], attributes["traceloop.workflow.name"]) where attributes["forge.ai.workflow.name"] == nil and attributes["traceloop.workflow.name"] != nil and attributes["forge.ai.convention"] == "openllmetry"'
    'set(attributes["forge.ai.run.id"], trace_id.string) where attributes["forge.ai.run.id"] == nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["traceloop.workflow.name"] != nil'
    'set(attributes["gen_ai.conversation.id"], attributes["session.id"]) where attributes["gen_ai.conversation.id"] == nil and attributes["session.id"] != nil and attributes["forge.ai.convention"] == "openinference"'
    'set(attributes["gen_ai.conversation.id"], attributes["traceloop.association.properties.session_id"]) where attributes["gen_ai.conversation.id"] == nil and attributes["traceloop.association.properties.session_id"] != nil and attributes["forge.ai.convention"] == "openllmetry"'
    'set(attributes["user.id"], attributes["traceloop.association.properties.user_id"]) where attributes["user.id"] == nil and attributes["traceloop.association.properties.user_id"] != nil and attributes["forge.ai.convention"] == "openllmetry"'
]
