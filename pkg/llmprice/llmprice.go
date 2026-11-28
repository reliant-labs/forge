// Package llmprice prices model calls at their list price, from LiteLLM's
// model price map vendored into forge (litellm_prices.json, MIT, see
// LITELLM_LICENSE). It is the same map the hosted LLM gateway bills with, at
// the release tag of the LiteLLM image the gateway runs, so an estimate for a
// call made with a customer's own provider key equals what the gateway would
// have charged for it.
//
// A price here is an ESTIMATE. A cost the provider or gateway reported on
// the span (genai.AttrCostUSD) always wins over it; readers show the
// estimate only where none was reported, labelled as estimated, with
// Table.Version. A call whose model the table does not know has no cost,
// never $0. There is no tokenizer: a call with no token counts has no
// estimate.
//
// Only the four base per-token rates are read (input, output, cache read,
// cache write). LiteLLM's tiered rates (above 128k/200k/272k prompt tokens),
// and its batch, flex and priority rates are not applied, so a call past a
// tier is estimated at the base rate.
//
// Rates are per token in US dollars, as LiteLLM states them. Readers that
// compute at query time (the AI hub's sql panels) read them from a tenant
// table filled from Table.Prices (TenantTable and its columns), so a
// corrected price fixes history without rewriting a span.
package llmprice

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
)

//go:embed litellm_prices.json
var vendored []byte

// SourceLiteLLM is Price.Source's prefix for a vendored rate:
// "litellm@<tag>".
const SourceLiteLLM = "litellm"

// SourceOverride is Price.Source of a rate a customer declared.
const SourceOverride = "override"

// Price is one model's resolved list rates, in US dollars per token. A
// cache class the catalog does not price is priced at the input rate, as
// the gateway's own list-price estimates do.
type Price struct {
	// Model is the catalog key ("claude-sonnet-4-5", "vertex_ai/claude-sonnet-4-5").
	Model string
	// Provider is LiteLLM's provider of the entry ("anthropic", "vertex_ai-anthropic_models").
	Provider string
	// Mode is LiteLLM's mode: chat, completion, responses or embedding.
	Mode string
	// The rates, USD per token.
	InputPerToken      float64
	OutputPerToken     float64
	CacheReadPerToken  float64
	CacheWritePerToken float64
	// Source is where the rates came from: "litellm@v1.104.0", or SourceOverride.
	Source string
}

// Usage is one call's token counts in the GenAI conventions' terms:
// InputTokens counts cache reads and writes in (gen_ai.usage.input_tokens).
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// Cost is u's estimated cost in US dollars at p. Input tokens not read from
// or written to the cache are priced at the input rate; a cache count larger
// than the input count is taken as excluded from it (an SDK that follows
// Anthropic's counting) rather than producing a negative.
func (p Price) Cost(u Usage) float64 {
	uncached := u.InputTokens - u.CacheReadInputTokens - u.CacheCreationInputTokens
	if uncached < 0 {
		uncached = u.InputTokens
	}
	return float64(uncached)*p.InputPerToken +
		float64(u.CacheReadInputTokens)*p.CacheReadPerToken +
		float64(u.CacheCreationInputTokens)*p.CacheWritePerToken +
		float64(u.OutputTokens)*p.OutputPerToken
}

// Table is a set of prices by catalog key.
type Table struct {
	version string
	prices  map[string]Price
}

var (
	defaultOnce  sync.Once
	defaultTable *Table
)

// Default is the vendored LiteLLM table. It panics if the vendored file
// does not parse, which the package's tests rule out.
func Default() *Table {
	defaultOnce.Do(func() {
		t, err := Parse(vendored)
		if err != nil {
			panic(fmt.Sprintf("llmprice: vendored litellm_prices.json: %v", err))
		}
		defaultTable = t
	})
	return defaultTable
}

// Parse reads a table in the vendored file's format (written by
// pkg/llmprice/internal/refresh). An entry with no input rate, a chat-like
// entry with no output rate, or any rate that is negative or not finite is
// left out: that model is unpriced.
func Parse(data []byte) (*Table, error) {
	var f struct {
		Source  string `json:"source"`
		Version string `json:"version"`
		Models  map[string]struct {
			Provider   string   `json:"provider"`
			Mode       string   `json:"mode"`
			Input      *float64 `json:"input"`
			Output     *float64 `json:"output"`
			CacheRead  *float64 `json:"cache_read"`
			CacheWrite *float64 `json:"cache_write"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Source != SourceLiteLLM || f.Version == "" {
		return nil, fmt.Errorf("not a LiteLLM price table (source %q, version %q)", f.Source, f.Version)
	}
	t := &Table{version: SourceLiteLLM + "@" + f.Version, prices: make(map[string]Price, len(f.Models))}
	for key, m := range f.Models {
		if m.Input == nil || !usable(*m.Input) {
			continue
		}
		out := 0.0
		switch {
		case m.Output != nil:
			out = *m.Output
		case m.Mode != "embedding":
			continue
		}
		p := Price{Model: key, Provider: m.Provider, Mode: m.Mode, InputPerToken: *m.Input, OutputPerToken: out,
			CacheReadPerToken: orInput(m.CacheRead, *m.Input), CacheWritePerToken: orInput(m.CacheWrite, *m.Input), Source: t.version}
		if !usable(p.OutputPerToken) || !usable(p.CacheReadPerToken) || !usable(p.CacheWritePerToken) {
			continue
		}
		t.prices[key] = p
	}
	if len(t.prices) == 0 {
		return nil, fmt.Errorf("no usable price in the table")
	}
	return t, nil
}

func usable(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

func orInput(v *float64, input float64) float64 {
	if v == nil {
		return input
	}
	return *v
}

// Version names the table's source, "litellm@v1.104.0". A table with
// overrides keeps its base's version; each overridden Price says
// SourceOverride.
func (t *Table) Version() string { return t.version }

// Len is how many models the table prices.
func (t *Table) Len() int { return len(t.prices) }

// Get is the price under exactly key.
func (t *Table) Get(key string) (Price, bool) {
	p, ok := t.prices[key]
	return p, ok
}

// Lookup prices a call by the provider and model a span names
// (gen_ai.provider.name, and gen_ai.response.model else
// gen_ai.request.model), trying Candidates in order.
func (t *Table) Lookup(provider, model string) (Price, bool) {
	for _, k := range Candidates(provider, model) {
		if p, ok := t.prices[k]; ok {
			return p, true
		}
	}
	return Price{}, false
}

// Prices is every price, by catalog key: what a tenant's TenantTable holds.
func (t *Table) Prices() []Price {
	out := make([]Price, 0, len(t.prices))
	for _, k := range slices.Sorted(maps.Keys(t.prices)) {
		out = append(out, t.prices[k])
	}
	return out
}

// Override is a negotiated price a customer declares for one model, per
// million tokens in US dollars. A cache rate left nil is priced at the input
// rate.
type Override struct {
	// Model is the catalog key it replaces or adds, matched exactly; name
	// every key a model is called under (see Candidates).
	Model             string
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  *float64
	CacheWritePerMTok *float64
}

// WithOverrides is t with each override replacing (or adding) its model's
// price. t is unchanged. An override with no model or an unusable rate is
// an error, never a silent $0.
func (t *Table) WithOverrides(overrides []Override) (*Table, error) {
	out := &Table{version: t.version, prices: make(map[string]Price, len(t.prices)+len(overrides))}
	maps.Copy(out.prices, t.prices)
	for _, o := range overrides {
		key := strings.TrimSpace(o.Model)
		if key == "" {
			return nil, fmt.Errorf("llmprice: an override names no model")
		}
		in := o.InputPerMTok / 1e6
		p := Price{Model: key, Provider: t.prices[key].Provider, Mode: t.prices[key].Mode,
			InputPerToken: in, OutputPerToken: o.OutputPerMTok / 1e6,
			CacheReadPerToken: perToken(o.CacheReadPerMTok, in), CacheWritePerToken: perToken(o.CacheWritePerMTok, in),
			Source: SourceOverride}
		for _, v := range []float64{p.InputPerToken, p.OutputPerToken, p.CacheReadPerToken, p.CacheWritePerToken} {
			if !usable(v) {
				return nil, fmt.Errorf("llmprice: override for %q has an unusable rate", key)
			}
		}
		out.prices[key] = p
	}
	return out, nil
}

func perToken(perMTok *float64, input float64) float64 {
	if perMTok == nil {
		return input
	}
	return *perMTok / 1e6
}
