package llmprice

import (
	"maps"
	"slices"
	"strings"
)

// ProviderPrefixes maps a GenAI provider (gen_ai.provider.name, after
// ingest normalisation) to the prefix LiteLLM keys that provider's models
// under where the bare model id names another deployment. Providers whose
// models LiteLLM keys bare (openai, anthropic) need none: Candidates always
// tries the bare id.
var ProviderPrefixes = map[string]string{
	"gcp.vertex_ai":      "vertex_ai/",
	"gcp.gemini":         "gemini/",
	"gcp.gen_ai":         "gemini/",
	"aws.bedrock":        "bedrock/",
	"azure.ai.openai":    "azure/",
	"azure.ai.inference": "azure_ai/",
	"x_ai":               "xai/",
	"mistral":            "mistral/",
	"mistral_ai":         "mistral/",
	"deepseek":           "deepseek/",
	"groq":               "groq/",
	"perplexity":         "perplexity/",
	"moonshot_ai":        "moonshot/",
	"openrouter":         "openrouter/",
}

// Candidates are the catalog keys a call may be priced under, most specific
// first: the provider-prefixed id (when ProviderPrefixes names one and the
// id does not carry it), the id as the span names it, and, for an id that
// names a vendor or route ("anthropic/claude-sonnet-4-5"), the part after
// the last slash.
func Candidates(provider, model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	var keys []string
	if p := ProviderPrefixes[strings.TrimSpace(provider)]; p != "" && !strings.HasPrefix(model, p) {
		keys = append(keys, p+model)
	}
	keys = append(keys, model)
	if i := strings.LastIndex(model, "/"); i >= 0 && i < len(model)-1 {
		keys = append(keys, model[i+1:])
	}
	return keys
}

// The tenant table a query-time reader prices from: one row per Price,
// written by the platform from Table.Prices (the vendored table with the
// organization's overrides), read with joinGet. Every rate column is
// Nullable(Float64) so a model the table lacks reads NULL, never 0.
const (
	// TenantTable is the table's name in the tenant database.
	TenantTable = "llm_prices"
	// ColModel is the key: Price.Model.
	ColModel = "Model"
	// ColInputPerToken, ColOutputPerToken, ColCacheReadPerToken and
	// ColCacheWritePerToken are the rates, USD per token.
	ColInputPerToken      = "InputPerToken"
	ColOutputPerToken     = "OutputPerToken"
	ColCacheReadPerToken  = "CacheReadPerToken"
	ColCacheWritePerToken = "CacheWritePerToken"
	// ColSource is Price.Source.
	ColSource = "Source"
)

// SQLKey is a ClickHouse expression for the TenantTable key a call is priced
// under: the first of Candidates(provider, model) the table holds, else the
// model as named (which then prices nothing). providerExpr and modelExpr are
// String expressions. It is the SQL twin of Table.Lookup.
func SQLKey(providerExpr, modelExpr string) string {
	prefixes := slices.Sorted(maps.Keys(ProviderPrefixes))
	vals := make([]string, len(prefixes))
	for i, p := range prefixes {
		vals[i] = sqlLit(ProviderPrefixes[p])
	}
	keys := make([]string, len(prefixes))
	for i, p := range prefixes {
		keys[i] = sqlLit(p)
	}
	prefix := "transform(" + providerExpr + ", [" + strings.Join(keys, ", ") + "], [" + strings.Join(vals, ", ") + "], '')"
	prefixed := "if(" + prefix + " != '' AND NOT startsWith(" + modelExpr + ", " + prefix + "), concat(" + prefix + ", " + modelExpr + "), " + modelExpr + ")"
	tail := "arrayElement(splitByChar('/', " + modelExpr + "), -1)"
	has := func(k string) string {
		return "isNotNull(joinGet(" + sqlLit(TenantTable) + ", " + sqlLit(ColInputPerToken) + ", " + k + "))"
	}
	return "multiIf(" + has(prefixed) + ", " + prefixed + ", " + has(modelExpr) + ", " + modelExpr + ", " + tail + ")"
}

// SQLRate is a ClickHouse expression for one rate column (ColInputPerToken,
// ...) under keyExpr (an SQLKey): NULL when the table does not price it.
func SQLRate(keyExpr, column string) string {
	return "joinGet(" + sqlLit(TenantTable) + ", " + sqlLit(column) + ", " + keyExpr + ")"
}

// sqlLit is a ClickHouse string literal for the plain names this package
// holds (keys and prefixes have no quote or backslash).
func sqlLit(s string) string { return "'" + s + "'" }
