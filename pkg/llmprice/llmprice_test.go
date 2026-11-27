package llmprice

import (
	"math"
	"os"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-12*math.Max(1, math.Abs(b)) }

// The vendored table parses, names its LiteLLM release, and holds the rates
// LiteLLM v1.104.0 states for models the gateway serves.
func TestDefaultIsTheVendoredLiteLLMTable(t *testing.T) {
	tb := Default()
	if tb.Version() != "litellm@v1.104.0" {
		t.Errorf("Version = %q", tb.Version())
	}
	if tb.Len() < 3000 {
		t.Errorf("only %d priced models: the vendored file was cut short", tb.Len())
	}
	for _, c := range []struct {
		key                  string
		in, out, cread, cwri float64
	}{
		// Rates as model_prices_and_context_window.json states them at v1.104.0.
		{"claude-sonnet-4-5", 3e-06, 1.5e-05, 3e-07, 3.75e-06},
		{"vertex_ai/claude-sonnet-4-5", 3e-06, 1.5e-05, 3e-07, 3.75e-06},
		{"claude-sonnet-5", 2e-06, 1e-05, 2e-07, 2.5e-06},
		// gpt-4o prices no cache write: it is priced at the input rate.
		{"gpt-4o", 2.5e-06, 1e-05, 1.25e-06, 2.5e-06},
		{"gemini-2.5-pro", 1.25e-06, 1e-05, 1.25e-07, 1.25e-06},
	} {
		p, ok := tb.Get(c.key)
		if !ok {
			t.Errorf("%s: not priced", c.key)
			continue
		}
		if !near(p.InputPerToken, c.in) || !near(p.OutputPerToken, c.out) || !near(p.CacheReadPerToken, c.cread) || !near(p.CacheWritePerToken, c.cwri) {
			t.Errorf("%s = %+v, want in %g out %g cache read %g cache write %g", c.key, p, c.in, c.out, c.cread, c.cwri)
		}
		if p.Source != "litellm@v1.104.0" {
			t.Errorf("%s: Source %q", c.key, p.Source)
		}
	}
	// sample_spec documents the format; image models price other units.
	for _, k := range []string{"sample_spec", "dall-e-3"} {
		if _, ok := tb.Get(k); ok {
			t.Errorf("%s is priced", k)
		}
	}
}

// The vendored data carries LiteLLM's MIT license beside it.
func TestTheVendoredDataCarriesItsLicense(t *testing.T) {
	b, err := os.ReadFile("LITELLM_LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "MIT") || !strings.Contains(string(b), "Copyright (c) 2023 Berri AI") {
		t.Error("LITELLM_LICENSE is not LiteLLM's MIT license")
	}
}

func TestLookupTriesTheProviderPrefixThenTheIDThenItsTail(t *testing.T) {
	tb := Default()
	for _, c := range []struct{ provider, model, want string }{
		{"anthropic", "claude-sonnet-4-5", "claude-sonnet-4-5"},
		{"gcp.vertex_ai", "claude-sonnet-4-5@20250929", "vertex_ai/claude-sonnet-4-5@20250929"},
		{"gcp.vertex_ai", "vertex_ai/claude-sonnet-4-5", "vertex_ai/claude-sonnet-4-5"},
		{"aws.bedrock", "anthropic.claude-sonnet-4-5-20250929-v1:0", "anthropic.claude-sonnet-4-5-20250929-v1:0"},
		{"gcp.gemini", "gemini-2.5-pro", "gemini/gemini-2.5-pro"},
		{"", "some-router/gpt-4o", "gpt-4o"},
		{"openai", "gpt-4o-2024-08-06", "gpt-4o-2024-08-06"},
	} {
		p, ok := tb.Lookup(c.provider, c.model)
		if !ok || p.Model != c.want {
			t.Errorf("Lookup(%q, %q) = %q, %v; want %q", c.provider, c.model, p.Model, ok, c.want)
		}
	}
	if p, ok := tb.Lookup("openai", "no-such-model-xyz"); ok {
		t.Errorf("an unknown model is priced: %+v", p)
	}
	if _, ok := tb.Lookup("openai", "  "); ok {
		t.Error("an empty model is priced")
	}
}

func TestCost(t *testing.T) {
	p := Price{InputPerToken: 3e-06, OutputPerToken: 1.5e-05, CacheReadPerToken: 3e-07, CacheWritePerToken: 3.75e-06}
	// 1000 input of which 600 cache reads and 100 cache writes: 300 uncached.
	got := p.Cost(Usage{InputTokens: 1000, OutputTokens: 200, CacheReadInputTokens: 600, CacheCreationInputTokens: 100})
	want := 300*3e-06 + 600*3e-07 + 100*3.75e-06 + 200*1.5e-05
	if !near(got, want) {
		t.Errorf("Cost = %g, want %g", got, want)
	}
	// Cache counts larger than the input: the input excludes them.
	got = p.Cost(Usage{InputTokens: 100, CacheReadInputTokens: 600})
	if want := 100*3e-06 + 600*3e-07; !near(got, want) {
		t.Errorf("Cost (cache excluded from input) = %g, want %g", got, want)
	}
	if got := p.Cost(Usage{}); got != 0 {
		t.Errorf("Cost of nothing = %g", got)
	}
}

func TestOverrides(t *testing.T) {
	base := Default()
	cr := 0.1
	tb, err := base.WithOverrides([]Override{
		{Model: "claude-sonnet-4-5", InputPerMTok: 2, OutputPerMTok: 10, CacheReadPerMTok: &cr},
		{Model: "our-finetune", InputPerMTok: 1, OutputPerMTok: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := tb.Get("claude-sonnet-4-5")
	if !near(p.InputPerToken, 2e-06) || !near(p.OutputPerToken, 1e-05) || !near(p.CacheReadPerToken, 1e-07) || !near(p.CacheWritePerToken, 2e-06) || p.Source != SourceOverride {
		t.Errorf("override = %+v", p)
	}
	if p.Provider != "anthropic" || p.Mode != "chat" {
		t.Errorf("an override keeps the entry's provider and mode: %+v", p)
	}
	if _, ok := tb.Get("our-finetune"); !ok {
		t.Error("an override adds a model the table lacks")
	}
	if b, _ := base.Get("claude-sonnet-4-5"); b.Source == SourceOverride {
		t.Error("WithOverrides changed its receiver")
	}
	if tb.Version() != base.Version() || tb.Len() != base.Len()+1 {
		t.Errorf("version %q len %d", tb.Version(), tb.Len())
	}
	for _, bad := range []Override{{Model: " "}, {Model: "m", InputPerMTok: -1}, {Model: "m", OutputPerMTok: math.NaN()}} {
		if _, err := base.WithOverrides([]Override{bad}); err == nil {
			t.Errorf("override %+v accepted", bad)
		}
	}
}

func TestParseRefusesWhatCannotPrice(t *testing.T) {
	for name, data := range map[string]string{
		"not json":     `{`,
		"other source": `{"source":"x","version":"v1","models":{"m":{"input":1,"output":1}}}`,
		"no version":   `{"source":"litellm","models":{"m":{"input":1,"output":1}}}`,
		"none usable":  `{"source":"litellm","version":"v1","models":{"m":{"input":-1,"output":1},"n":{"mode":"chat","input":1}}}`,
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	tb, err := Parse([]byte(`{"source":"litellm","version":"v9","models":{"e":{"mode":"embedding","input":1e-7},"c":{"mode":"chat","input":1e-6}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tb.Get("e"); !ok {
		t.Error("an embedding model with no output rate is priced (output is free)")
	}
	if _, ok := tb.Get("c"); ok {
		t.Error("a chat model with no output rate is priced")
	}
}

// SQLKey is Lookup's SQL twin: it tries the same candidates in the same order.
func TestSQLKeyNamesTheTenantTableAndEveryPrefix(t *testing.T) {
	sql := SQLKey("provider", "model")
	if !strings.Contains(sql, "joinGet('"+TenantTable+"', '"+ColInputPerToken+"'") {
		t.Errorf("SQLKey does not probe %s: %s", TenantTable, sql)
	}
	for provider, prefix := range ProviderPrefixes {
		if !strings.Contains(sql, "'"+provider+"'") || !strings.Contains(sql, "'"+prefix+"'") {
			t.Errorf("SQLKey misses %s → %s", provider, prefix)
		}
	}
	if strings.ContainsAny(strings.Join(Candidates("gcp.vertex_ai", "a/b"), ""), "'\\") {
		t.Error("a candidate needs escaping")
	}
	if got := SQLRate("k", ColOutputPerToken); got != "joinGet('llm_prices', 'OutputPerToken', k)" {
		t.Errorf("SQLRate = %s", got)
	}
}
