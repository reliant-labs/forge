package obsbuiltin

import (
	"github.com/reliant-labs/forge/pkg/dashspec"
	"github.com/reliant-labs/forge/pkg/llmprice"
	"github.com/reliant-labs/forge/pkg/observe/genai"
)

// AICostID is the AI hub's cost dashboard: what model calls cost, reported
// or estimated from a price table.
//
// Unlike the other hub dashboards it reads a second tenant table,
// llmprice.TenantTable, which the platform fills from pkg/llmprice (LiteLLM's
// list prices, with the organization's overrides). A tenant older than that
// table cannot run it, so it is not one of AIDashboards and never goes into
// a project's spec: the platform serves it as a built-in where the tenant has
// the table. IsAIDashboard does not name it.
const AICostID = "ai-cost"

// The priced model calls in scope, as CTEs: calls carries each call's tokens,
// its reported cost and the price-table key and rates it is priced under;
// priced adds the estimate (NULL without a price or usable usage) and cost, the
// reported cost where there is one, else the estimate.
//
// Input tokens include cache reads and writes (the GenAI conventions), so
// the uncached part is input less both. A cache count larger than the input
// count is taken as excluded from it (an SDK counting the way Anthropic's
// API does) and the whole input is priced at the input rate.
var aiPricedCTEs = `calls AS (
  SELECT *, ` + llmprice.SQLKey("provider", "model") + ` AS price_key,
         ` + llmprice.SQLRate("price_key", llmprice.ColInputPerToken) + ` AS p_in,
         ` + llmprice.SQLRate("price_key", llmprice.ColOutputPerToken) + ` AS p_out,
         ` + llmprice.SQLRate("price_key", llmprice.ColCacheReadPerToken) + ` AS p_cr,
         ` + llmprice.SQLRate("price_key", llmprice.ColCacheWritePerToken) + ` AS p_cw,
         ` + llmprice.SQLRate("price_key", llmprice.ColSource) + ` AS price_source
  FROM (
    SELECT Timestamp AS ts, ` + aiModel + ` AS model, ` + aiProvider + ` AS provider,
           ` + aiIn + ` AS in_tok, ` + aiOut + ` AS out_tok, ` + aiCacheRead + ` AS cr_tok, ` + aiCacheWrite + ` AS cw_tok,
           isNotNull(coalesce(toUInt64OrNull(` + attr(genai.AttrUsageInputTokens) + `), toUInt64OrNull(` + attr(genai.AttrUsageOutputTokens) + `),
                              toUInt64OrNull(` + either(genai.AttrUsageCacheReadInputTokens, genai.AttrUsageCacheReadInputTokensLegacy) + `),
                              toUInt64OrNull(` + either(genai.AttrUsageCacheCreationInputTokens, genai.AttrUsageCacheCreationInputTokensLegacy) + `))) AS has_usage,
           ` + aiCost + ` AS reported, ` + aiWorkflowName + ` AS workflow, ` + aiRun + ` AS run_id,
           ` + attr(genai.AttrUserID) + ` AS user_id, ` + attr(genai.AttrAgentName) + ` AS agent
    FROM otel_traces WHERE ` + aiScope + ` AND ` + aiCall + `)),
priced AS (
  SELECT *,
         if(isNull(p_in) OR NOT has_usage, NULL,
            if(in_tok >= cr_tok + cw_tok, in_tok - cr_tok - cw_tok, in_tok) * p_in
            + cr_tok * p_cr + cw_tok * p_cw + out_tok * p_out) AS estimated,
         ifNull(reported, estimated) AS cost
  FROM calls)`

// The cost figures over priced: the reported part, the estimated part
// (calls with no reported cost), and the calls that have neither.
const (
	aiCostTotal     = "round(sum(cost), 4)"
	aiCostReported  = "round(sum(reported), 4)"
	aiCostEstimated = "round(sumIf(estimated, isNull(reported)), 4)"
	aiCostUnpriced  = "countIf(isNull(cost))"
)

// AICost is the AI hub's cost dashboard: each model call's reported cost, or
// where none was reported, its list price from the platform's price table,
// labelled estimated; by day, model, workflow, user and agent.
func AICost(in Input) dashspec.Dashboard {
	d := aiDashboard(in, AICostID, "AI cost",
		"Model call cost: the cost reported on the call, else an estimate at the list price the Reliant LLM gateway bills with "+
			"(LiteLLM's base per-token rates; no tiers). An unpriced call has no cost, never $0. Not an invoice.",
		[]dashspec.Section{
			{ID: "glance", Title: "At a glance", Description: "The range's cost, reported and estimated, and the calls nothing prices."},
			{ID: "daily", Title: "By day", Description: "Cost per UTC day, reported and estimated."},
			{ID: "breakdown", Title: "Where it goes", Description: "Cost by model, workflow, user and agent."},
		})
	from := "\nFROM priced"
	d.Panels = []dashspec.Panel{
		figure("glance", "cost", "Cost (USD)",
			"Reported cost where the call carries one, else the estimate at list price, summed over the range's model calls. Unpriced calls add nothing. The trend is each UTC day's.",
			with(aiPricedCTEs)+"\nSELECT "+aiCostTotal+` AS "Cost"`+from,
			with(aiPricedCTEs)+"\nSELECT toDate(ts) AS day, "+aiCostTotal+` AS "Cost"`+from+" GROUP BY day ORDER BY day",
			dashspec.UnitNone, colorCost),
		figure("glance", "estimated", "Estimated (USD)",
			"The part of the cost estimated at list price: calls with no reported cost (a provider called with your own key, an SDK that reports none) whose model the price table prices.",
			with(aiPricedCTEs)+"\nSELECT "+aiCostEstimated+` AS "Estimated"`+from, "", dashspec.UnitNone, colorCost),
		figure("glance", "reported", "Reported (USD)",
			"The part of the cost the provider or gateway reported on the call (forge.ai.cost.usd, or a convention's own cost field normalised onto it at ingest). Never an estimate.",
			with(aiPricedCTEs)+"\nSELECT "+aiCostReported+` AS "Reported"`+from, "", dashspec.UnitNone, colorCost),
		exact(figure("glance", "unpriced", "Unpriced calls",
			"Model calls with no reported cost and no usable token counts or no model price. They are in no cost figure; the by-model table names them.",
			with(aiPricedCTEs)+"\nSELECT "+aiCostUnpriced+` AS "Unpriced calls"`+from, "", dashspec.UnitCount, colorDanger)),
		daily(chart("daily", "cost-by-day", "Cost by day",
			"Each UTC day's reported cost and, for calls with none, the estimate at list price.",
			with(aiPricedCTEs)+`
SELECT toDate(ts) AS day, `+aiCostReported+` AS "Reported", `+aiCostEstimated+` AS "Estimated"`+from+`
GROUP BY day ORDER BY day`,
			bars(true, dashspec.SeriesColors{"Reported": colorCost, "Estimated": colorCache}), 24, 8)),

		table("breakdown", "by-model", "By model",
			"One row per provider and model that served the call: tokens, the cost reported and estimated, the calls nothing priced, the price-table key "+
				"the estimate used, its rates in USD per million tokens, and where they came from (the LiteLLM release, or override).",
			with(aiPricedCTEs)+`
SELECT provider AS "Provider", model AS "Model", count() AS "Calls",
       sum(in_tok) AS "Input tokens", sum(out_tok) AS "Output tokens",
       `+aiCostReported+` AS "Reported (USD)", `+aiCostEstimated+` AS "Estimated (USD)", `+aiCostTotal+` AS "Cost (USD)",
       `+aiCostUnpriced+` AS "Unpriced calls",
       anyIf(price_key, isNotNull(p_in)) AS "Priced as",
       round(any(p_in) * 1e6, 4) AS "Input / 1M", round(any(p_out) * 1e6, 4) AS "Output / 1M",
       anyIf(price_source, isNotNull(p_in)) AS "Price source"`+from+`
GROUP BY provider, model ORDER BY "Cost (USD)" DESC NULLS LAST, "Calls" DESC LIMIT 100`, 24, 9),
		table("breakdown", "by-workflow", "By workflow",
			"Each workflow's runs, model calls, cost and cost per run. Calls outside any workflow are the (none) row.",
			with(aiPricedCTEs)+`
SELECT if(workflow = '', '(none)', workflow) AS "Workflow", uniqExactIf(run_id, run_id != '') AS "Runs", count() AS "Calls",
       `+aiCostTotal+` AS "Cost (USD)", round(sum(cost) / nullIf(uniqExactIf(run_id, run_id != ''), 0), 4) AS "Cost / run",
       `+aiCostUnpriced+` AS "Unpriced calls"`+from+`
GROUP BY "Workflow" ORDER BY "Cost (USD)" DESC NULLS LAST, "Calls" DESC LIMIT 50`, 12, 9),
		table("breakdown", "by-user", "By user",
			"Each user's (user.id) model calls and cost. Calls with no user are the (none) row.",
			with(aiPricedCTEs)+`
SELECT if(user_id = '', '(none)', user_id) AS "User", count() AS "Calls", `+aiCostTotal+` AS "Cost (USD)",
       `+aiCostUnpriced+` AS "Unpriced calls"`+from+`
GROUP BY "User" ORDER BY "Cost (USD)" DESC NULLS LAST, "Calls" DESC LIMIT 50`, 12, 9),
		table("breakdown", "by-agent", "By agent",
			"Each agent's (gen_ai.agent.name) model calls and cost. Calls made outside an agent turn are the (none) row.",
			with(aiPricedCTEs)+`
SELECT if(agent = '', '(none)', agent) AS "Agent", count() AS "Calls", `+aiCostTotal+` AS "Cost (USD)",
       `+aiCostUnpriced+` AS "Unpriced calls"`+from+`
GROUP BY "Agent" ORDER BY "Cost (USD)" DESC NULLS LAST, "Calls" DESC LIMIT 50`, 24, 9),
	}
	return d
}
