package agentua

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// agentsYAMLSHA256 pins agents.yaml with flow mappings compacted. When you edit
// the list, bump its `version` AND update this hash, in the same change.
// Refreshed for Prettier's whitespace-only compact mappings; the list is unchanged.
const agentsYAMLSHA256 = "cad7117d18d6492cb029f3689d8be068002d374a7ce71f9b6043bcede8f1c0e7"

// agentsYAMLVersion is the version that hash belongs to.
const agentsYAMLVersion = "2026-11-27.1"

func TestVersionBumpedWithList(t *testing.T) {
	// Prettier expands flow mappings; that whitespace-only change does not
	// change the classification version stored on rows. Hash the compact form.
	compact := strings.NewReplacer(
		"{\n      ", "{",
		",\n      ", ", ",
		",\n    }", "}",
	).Replace(string(agentsYAML))
	sum := sha256.Sum256([]byte(compact))
	got := hex.EncodeToString(sum[:])
	if got != agentsYAMLSHA256 || Version() != agentsYAMLVersion {
		t.Fatalf("agents.yaml changed (sha256 %s, version %q) but the pins say (sha256 %s, version %q).\n"+
			"Bump `version` in agents.yaml (YYYY-MM-DD.N), then set agentsYAMLSHA256 and agentsYAMLVersion here to match: "+
			"stored rows carry the version, so a changed list under an old version cannot be told apart.",
			got, Version(), agentsYAMLSHA256, agentsYAMLVersion)
	}
}

// UA strings taken verbatim from reliantlabs.io CDN logs (cdn-ai.json,
// cdn-agent.json, 2026-11). The spoofed ones (bare "GPTBot", "ClaudeBot",
// secret probes) still classify by name: Verifier is what tells them apart.
func TestClassifyRealUserAgents(t *testing.T) {
	cases := []struct {
		ua    string
		class Class
		agent string
	}{
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)", ClassAICrawler, "ClaudeBot"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +claudebot@anthropic.com)", ClassAIUser, "Claude-User"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +claude-user@anthropic.com)", ClassAIUser, "Claude-User"},
		{"Claude-User (claude-code/2.1.331; +https://support.anthropic.com/)", ClassAIUser, "Claude-User"},
		{"Claude-User", ClassAIUser, "Claude-User"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-SearchBot/1.0; +claudebot@anthropic.com)", ClassAISearch, "Claude-SearchBot"},
		{"ClaudeBot", ClassAICrawler, "ClaudeBot"},
		{"Mozilla/5.0 (compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)", ClassAISearch, "PerplexityBot"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Perplexity-User/1.0; +https://perplexity.ai/perplexity-user)", ClassAIUser, "Perplexity-User"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0; +https://openai.com/bot", ClassAIUser, "ChatGPT-User"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.4; +https://openai.com/gptbot)", ClassAICrawler, "GPTBot"},
		{"GPTBot", ClassAICrawler, "GPTBot"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; OAI-SearchBot/1.0; +https://openai.com/searchbot", ClassAISearch, "OAI-SearchBot"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36; compatible; OAI-SearchBot/1.4; robots.txt; +https://openai.com/searchbot", ClassAISearch, "OAI-SearchBot"},
		{"ResourcePlanetReview/1.0 (ChatGPT-User; ordinary public source qualification)", ClassAIUser, "ChatGPT-User"},
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", ClassSearchCrawler, "Googlebot"},
		{"Mozilla/5.0 (Linux; Android 6.0.1; Nexus 5X Build/MMB29P) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.8103.78 Mobile Safari/537.36 (compatible; GoogleOther)", ClassOtherBot, "GoogleOther"},
		{"Cloud-CDN-Google (GFE/2.0)", ClassOtherBot, "Cloud-CDN-Google"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm) Chrome/116.0.1938.76 Safari/537.36", ClassSearchCrawler, "bingbot"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15 (Applebot/0.1; +http://www.apple.com/go/applebot)", ClassSearchCrawler, "Applebot"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 7_1_2 like Mac OS X) AppleWebKit/537.36 (KHTML, like Gecko) Version/7.0 Mobile Safari/537.36 (compatible; Bytespider; https://bytedance.sg.larkoffice.com/docx/K5bxdypulop3IIxrJb0lOjLVgFe)", ClassAICrawler, "Bytespider"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36 (compatible; meta-externalagent/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler))", ClassAICrawler, "meta-externalagent"},
		{"Mozilla/5.0 (compatible; Meta-WebIndexer/1.0; +https://developers.facebook.com/docs/sharing/webmasters/crawler)", ClassSearchCrawler, "Meta-WebIndexer"},
		{"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)", ClassOtherBot, "facebookexternalhit"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Amazonbot/0.1; +https://developer.amazon.com/support/amazonbot) Chrome/119.0.6045.214 Safari/537.36", ClassAICrawler, "Amazonbot"},
		{"DuckAssistBot/1.2; (+http://duckduckgo.com/duckassistbot.html)", ClassAIUser, "DuckAssistBot"},
		{"DuckDuckBot/1.0; (+http://duckduckgo.com/duckduckbot.html)", ClassSearchCrawler, "DuckDuckBot"},
		{"Mozilla/5.0 (compatible; YandexBot/3.0; +http://yandex.com/bots)", ClassSearchCrawler, "YandexBot"},
		{"Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)", ClassOtherBot, "AhrefsBot"},
		{"Mozilla/5.0 (compatible; ExaSearchBot/1.0; +https://crawler.exa.ai/)", ClassAISearch, "ExaSearchBot"},
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; AzureAI-SearchBot/1.0;", ClassAISearch, "AzureAI-SearchBot"},
		{"curl/8.5.0", ClassTool, "curl"},
		{"undici", ClassTool, "undici"},
		{"fasthttp", ClassTool, "fasthttp"},
		{"LinkChecker/1.0", ClassMonitor, "LinkChecker"},
		// Not on the list: generic fallbacks.
		{"code-docs-corpus/0.1 (+dataset crawler)", ClassOtherBot, ""},
		{"DomainMetaDataBot/1.0 (+https://domainmetadata.com/bot)", ClassOtherBot, ""},
		{"KodokFetcher/1.0 (news reader; respects robots and conditional GET)", ClassOtherBot, ""},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Claude/6.28899.6 Chrome/156.0.8078.100 Safari/537.36", ClassBrowser, ""},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:123.0) Gecko/20100101 Firefox/123", ClassBrowser, ""},
		{"addrbook-probe/1 (+https://addrbook.com/probe)", ClassUnknown, ""},
		{"", ClassUnknown, ""},
	}
	for _, tc := range cases {
		got := Classify(tc.ua)
		if got.Class != tc.class || got.Agent != tc.agent {
			t.Errorf("Classify(%q) = %+v, want class %s agent %q", tc.ua, got, tc.class, tc.agent)
		}
		if tc.agent != "" && got.Operator == "" {
			t.Errorf("Classify(%q): matched %q with no operator", tc.ua, got.Agent)
		}
	}
}

func TestEveryEntryIsComplete(t *testing.T) {
	agents := Agents()
	if len(agents) < 50 {
		t.Fatalf("list has %d agents; expected the full list", len(agents))
	}
	seen := map[string]bool{}
	for _, a := range agents {
		if a.Token == "" || a.Name == "" || a.Operator == "" {
			t.Errorf("incomplete entry %+v", a)
		}
		if !a.Class.Valid() || a.Class == ClassUnknown {
			t.Errorf("%s: invalid class %q", a.Name, a.Class)
		}
		if a.RangesURL != "" && !strings.HasPrefix(a.RangesURL, "https://") {
			t.Errorf("%s: ranges URL %q is not https", a.Name, a.RangesURL)
		}
		lt := strings.ToLower(a.Token)
		if seen[lt] {
			t.Errorf("duplicate token %q", a.Token)
		}
		seen[lt] = true
	}
}

// Every token, alone and inside a typical crawler UA, classifies as its own
// entry, and no other entry's token matches it — so list order cannot let one
// agent shadow another (Claude-User vs ClaudeBot, Applebot-Extended vs Applebot).
func TestEveryTokenMatchesOnlyItsOwnAgent(t *testing.T) {
	agents := Agents()
	for _, a := range agents {
		for _, ua := range []string{
			a.Token,
			a.Token + "/1.0",
			"Mozilla/5.0 (compatible; " + a.Token + "/2.1; +https://example.com/bot)",
		} {
			if got := Classify(ua); got.Agent != a.Name {
				t.Errorf("Classify(%q).Agent = %q, want %q", ua, got.Agent, a.Name)
			}
		}
		for _, b := range agents {
			if b.Token == a.Token {
				continue
			}
			// b's token inside a's token is fine only when b comes later in
			// the list (a then wins): the Classify check above proves that.
			// What must never happen is a matching b that sits earlier.
			if containsToken(strings.ToLower(a.Token), strings.ToLower(b.Token)) && indexOf(agents, b) < indexOf(agents, a) {
				t.Errorf("token %q (%s) matches inside %q (%s) and comes first", b.Token, b.Name, a.Token, a.Name)
			}
		}
	}
}

func indexOf(agents []Agent, a Agent) int {
	for i := range agents {
		if agents[i].Token == a.Token {
			return i
		}
	}
	return -1
}

func TestTokenBoundaries(t *testing.T) {
	cases := []struct {
		ua, tok string
		want    bool
	}{
		{"googlebot-image/1.0", "googlebot", true},
		{"claude-user/1.0", "user", false},
		{"chatgpt-user/1.0", "gptbot", false},
		{"applebotx", "applebot", false},
		{"xcurl/1", "curl", false},
		{"(compatible; ccbot/2.0)", "ccbot", true},
		{"java/17", "java", true},
		{"javascript", "java", false},
	}
	for _, tc := range cases {
		if got := containsToken(tc.ua, tc.tok); got != tc.want {
			t.Errorf("containsToken(%q, %q) = %v, want %v", tc.ua, tc.tok, got, tc.want)
		}
	}
}

func TestAgentsReturnsACopy(t *testing.T) {
	a := Agents()
	a[0].Name = "mutated"
	if Agents()[0].Name == "mutated" {
		t.Fatal("Agents exposed the embedded list")
	}
	if _, ok := Lookup("ClaudeBot"); !ok {
		t.Fatal("Lookup(ClaudeBot) failed")
	}
}

func TestClassifyLongUserAgent(t *testing.T) {
	ua := strings.Repeat("x", 600) + " GPTBot/1.0"
	if got := Classify(ua); got.Agent != "GPTBot" {
		t.Fatalf("long UA: %+v", got)
	}
}

func TestClassifyAllocations(t *testing.T) {
	ua := "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)"
	if n := testing.AllocsPerRun(100, func() { _ = Classify(ua) }); n != 0 {
		t.Fatalf("Classify allocates %.0f times per call; want 0", n)
	}
}

func BenchmarkClassify(b *testing.B) {
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Classify(ua)
	}
}
