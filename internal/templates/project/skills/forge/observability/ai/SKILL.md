---
name: observability/ai
description: The built-in AI hub — LLM and workflow monitoring every hosted forge app gets from its OpenTelemetry GenAI spans (pkg/obsbuiltin's AI dashboards, the console's AI group) — runs, success, duration, model calls and tokens by workflow, node, agent, model, provider, user and org, errors and retries, tools; the attribute contract (gen_ai.* plus forge.ai.*), emitting it with pkg/observe/genai, why AI spans are never sampled, and what cost it shows (only a gateway-reported one).
---

# The AI hub

A forge app that calls models gets LLM and workflow monitoring in the console's
**AI** group, beside Product and Services, with nothing declared. Six built-in
dashboards (`pkg/obsbuiltin`, `AIDashboards`) read the OpenTelemetry **GenAI
semantic conventions** out of the environment's own traces (`otel_traces`):

| Dashboard | id | Answers |
|---|---|---|
| AI overview | `ai` | runs, run success rate, p50/p95 run duration, model calls, tokens, prompt-cache hit ratio, gateway-reported cost; runs by outcome, calls and tokens by day |
| AI by workflow | `ai-workflows` | per workflow: runs, success, p50/p95, calls and tokens per run, top model; runs per day; per node and per agent |
| AI models & providers | `ai-models` | tokens and calls by model per day; per provider × model: calls, error rate, p50/p95, time to first token, output tokens/s, cache hit, reported cost; calls served by another model |
| AI by user | `ai-users` | users per day; top users and orgs by tokens |
| AI errors & retries | `ai-errors` | call error rate, retries, failures by `error.type`, backoffs by status, failures by model, failed runs with their first failing call |
| AI tools | `ai-tools` | per tool: calls, error rate, p50/p95, where it ran; calls per day; run time in tools vs model calls; tools of failed runs |

They are hosted-only like the product dashboards (sql panels, which a local
HyperDX cannot draw) and ship in every release bundle. A static site gets none.
Override one with a same-named file (`deploy/observability/dashboards/<project>-ai-models.json`),
exactly as for any generated dashboard (`observability/dashboards`). An older
console lists the AI group under Custom.

## Where the numbers come from

Every panel is one read-only SELECT over raw spans; there is **no rollup**, so
the longest range a panel answers is the environment's trace retention (the
dashboards open on 7 days). Only the spans' attributes matter, so any emitter of
the conventions feeds them: forge's helpers below, Reliant, or another
language's instrumentation.

- **A model call** is a span with `gen_ai.operation.name` = `chat`,
  `text_completion`, `generate_content` or `embeddings`.
  The model is `gen_ai.response.model`, else `gen_ai.request.model`; the provider
  `gen_ai.provider.name`, else the older `gen_ai.system`.
- **Tokens** are `gen_ai.usage.input_tokens` / `output_tokens` as reported
  (input includes cache reads). Cache reads are
  `gen_ai.usage.cache_read.input_tokens` (the older
  `cache_read_input_tokens` is read too).
- **A run** is every span sharing `forge.ai.run.id`, across traces: a run that
  continues in a new trace (continue-as-new, resume) is still one run. Its
  duration is its first span's start to its last span's end, paused time
  included. Its outcome is its last `invoke_workflow` span's: `error.type`
  `canceled` is Cancelled, an error status Failed, else Succeeded; a run whose
  workflow span is not reported yet is In progress. Success rate is succeeded
  over succeeded + failed.
- **A failure** is an error status, except `error.type` = `canceled`: a
  cancellation is never an error.
- **Cost**: forge never prices a call. A figure appears only where the span
  carries the cost its provider or gateway **reported** (`forge.ai.cost.usd`,
  or Reliant's older `reliant.cost_usd`), labelled "reported"; a range with
  none shows no figure, never $0. It is not an invoice: billing is the
  control plane's.

## The attribute contract

`pkg/observe/genai` (`reliant forge project libraries observe/genai`) holds every
name as a constant. A semver-1 contract: a name is never renamed in place.

| Attribute | On | Meaning |
|---|---|---|
| `gen_ai.operation.name` | every AI span | `chat`, `invoke_agent`, `execute_tool`, `invoke_workflow`, ... |
| `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.response.id`, `gen_ai.response.finish_reasons` | model call | |
| `gen_ai.usage.input_tokens`, `.output_tokens`, `.cache_read.input_tokens`, `.cache_creation.input_tokens`, `.reasoning.output_tokens` | model call | |
| `gen_ai.retries`, event `gen_ai.retry_wait` (`attempt`, `http.response.status_code`, `reason`, `delay_ms`), event `gen_ai.first_token` | model call | retries; one event per backoff; time to first token |
| `gen_ai.agent.name`, `gen_ai.agent.id`, `gen_ai.conversation.id` | agent turn (name and conversation flow down) | |
| `gen_ai.tool.name`, `gen_ai.tool.call.id`, `gen_ai.tool.type`, `forge.ai.tool.location` | tool call | location: `local`, `daemon`, `remote` |
| `forge.ai.workflow.name`, `forge.ai.run.id`, `forge.ai.root_run_id` | every span of a run | |
| `forge.ai.workflow.ref`, `forge.ai.workflow.trigger`, `forge.ai.parent_run_id` | workflow segment | |
| `forge.ai.node.id` (flows down), `forge.ai.node.type`, `forge.ai.node.iteration` | node | |
| `forge.ai.cost.usd` | model call | the REPORTED cost, never an estimate |
| `user.id`, `org.id` | any | opaque ids, the keys product analytics uses |
| `error.type` | any failed or cancelled span | `canceled` is not a failure |

The `forge.ai.*` names cover what the conventions do not name yet. When OTel
standardises one, forge emits both and reads both.

## Emitting it from Go

```go
import "github.com/reliant-labs/forge/pkg/observe/genai"

ctx = genai.WithScope(ctx, attribute.String(genai.AttrUserID, uid), attribute.String(genai.AttrOrgID, org))
ctx, wf := genai.StartWorkflow(ctx, genai.Workflow{Name: "triage", RunID: runID, Trigger: "schedule"})
defer func() { genai.End(wf, err) }()
ctx, node := genai.StartNode(ctx, genai.Node{ID: "classify"})
ctx, turn := genai.StartAgent(ctx, genai.Agent{Name: "triager", ConversationID: chatID})
ctx, span := genai.StartChat(ctx, genai.Chat{Provider: "anthropic", Model: model})
msg, err := client.Messages.New(ctx, params)
// SetResponse(span, Response{Model, ID, FinishReasons, Usage, CostUSD}), then
genai.End(span, err)
```

Each `Start*` puts what it names in the context, so the chat span three levels
down carries the workflow, run, node and agent without a join. Only what you
need: an app that calls a model once needs `StartChat` alone. The package doc
has the exact anthropic-go and openai-go usage mappings (Anthropic's
`input_tokens` excludes cache reads; add them). `genai.End` records a
cancellation as `error.type=canceled` with no error status.

## In release health

`reliant forge obs check <env>` judges each service's GenAI spans across a
promotion, spans only (no content, no scores), with three findings that
**warn** unless the env's `release_health.fail_on` lists them:

| Finding | Fires when |
|---|---|
| `ai_error_rate` | the share of failed model and tool calls rose by more than `ai_max_error_rate_increase` (0.05), with at least `ai_min_errors` (5) failures after; `error.type=canceled` is no failure |
| `ai_latency` | the model calls' p95 grew by more than `ai_max_p95_ratio` (1.5) |
| `ai_tool_errors` | one tool's error rate rose by more than `ai_tool_max_error_rate_increase` (0.1); names the tool |

A service or tool needs `ai_min_calls` (50) calls in both windows; a thinner
one is listed under `not_evaluated` with the `ai:` prefix. On a hosted env the
after window counts only the judged release's own `service.version`. The
thresholds are `--threshold` overrides (`--threshold ai_min_calls=20`); the
JSON's `ai` array holds each service's figures. `ai_cost` comes later.

## Spans from other SDKs (OpenInference, OpenLLMetry)

An app instrumented with OpenInference (Arize/Phoenix) or OpenLLMetry
(Traceloop), or one still on the GenAI names OTel renamed (`gen_ai.system`,
`gen_ai.usage.prompt_tokens`), needs no change: point its OTLP exporter at the
env (`reliant forge obs init <env>`). Reliant hosting's gateway normalises at
ingest from one forge table, `pkg/observe/genai/conventions`: it ADDS the
canonical keys above where a span lacks them (`openinference.span.kind=LLM` →
`gen_ai.operation.name=chat`, `llm.token_count.prompt` →
`gen_ai.usage.input_tokens`, `session.id` → `gen_ai.conversation.id`, ...),
never overwrites or deletes, and stamps `forge.ai.convention` (`genai`,
`openinference`, `openllmetry`) so you can see where a span came from. The
original attributes stay queryable. An OpenLLMetry workflow's run is its trace
(`forge.ai.run.id` = the trace id). The rules, the names forge marks
unverified and the golden vectors are in the package
(`reliant forge project libraries observe/genai/conventions`). Message content
is not mapped (see "Not yet").

## Never sampled

Token totals are sums over spans, so a sampled-away call is a missing cost.
`observe.Setup` wraps the trace sampler (still read from `OTEL_TRACES_SAMPLER`)
in `genai.KeepSampler`, which keeps every span that starts with
`gen_ai.operation.name` or a `forge.ai.*` attribute; every `Start*` sets them at
start. A process that builds its own tracer provider wraps its sampler itself:
`sdktrace.WithSampler(genai.KeepSampler(base))`. A gateway that tail-samples
must keep them too (the hosted platform's gateway policy).

## Not yet

- Estimated cost is on the AI cost dashboard only (`ai-cost`), which the
  platform serves where your tenant has the price table: a call with no
  reported cost is priced at LiteLLM's list rates (`pkg/llmprice`, the rates
  the Reliant LLM gateway bills with; base per-token rates, no tiers), labelled
  estimated. A model the table does not know has no cost, never $0. The other
  hub dashboards show the reported cost only.
- No prompt or completion content: a later, opt-in phase with its own storage,
  redaction and retention. Never put content in span attributes.
- No run explorer page yet; a trace id in Failed runs opens the trace.
