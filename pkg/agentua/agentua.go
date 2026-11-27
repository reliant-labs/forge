// Package agentua classifies HTTP User-Agent strings: it names AI agents,
// crawlers, tools and monitors, and verifies a claimed agent against the IP
// ranges its operator publishes.
//
// The list is agents.yaml, embedded in this package, so it ships with forge
// releases: a new agent reaches a consumer when it moves its forge pin.
// Version names the list a classification came from; store it beside each
// classified row (the ClassifierVersion column of edge telemetry) together
// with the raw User-Agent, and a later list can reclassify old rows by
// re-running Classify on them.
//
// A User-Agent is a claim, not an identity: anyone can send
// "ClaudeBot/1.0". Classify reports what the string claims; Verifier.Verified
// reports whether the request came from the operator's published ranges.
// Show an unverified match as "claims to be". An agent whose operator
// publishes no ranges (Agent.RangesURL == "") is never verified.
package agentua

import (
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Class is what kind of client a User-Agent claims to be.
type Class string

// The classes. Each answers a different robots.txt question.
const (
	ClassBrowser       Class = "browser"        // a person's browser
	ClassSearchCrawler Class = "search_crawler" // builds a search index
	ClassAICrawler     Class = "ai_crawler"     // collects training data
	ClassAISearch      Class = "ai_search"      // indexes for an answer engine
	ClassAIUser        Class = "ai_user"        // fetches because a person asked an assistant
	ClassTool          Class = "tool"           // a library or CLI
	ClassMonitor       Class = "monitor"        // uptime and link checkers
	ClassOtherBot      Class = "other_bot"      // a self-declared bot not otherwise classed
	ClassUnknown       Class = "unknown"        // nothing matched
)

// Valid reports whether c is one of the declared classes.
func (c Class) Valid() bool {
	switch c {
	case ClassBrowser, ClassSearchCrawler, ClassAICrawler, ClassAISearch, ClassAIUser,
		ClassTool, ClassMonitor, ClassOtherBot, ClassUnknown:
		return true
	}
	return false
}

// Agent is one entry of the embedded list.
type Agent struct {
	Token     string `yaml:"token"`    // product token matched in the UA, case-insensitive (e.g. "ClaudeBot")
	Name      string `yaml:"name"`     // display name (e.g. "ClaudeBot")
	Operator  string `yaml:"operator"` // e.g. "Anthropic"
	Class     Class  `yaml:"class"`
	RangesURL string `yaml:"ranges"` // operator's published IP-range JSON, "" when none
}

// Match is the result of Classify.
type Match struct {
	Class    Class
	Agent    string // Name of the matched list entry; "" when none matched
	Operator string // "" when none matched
}

//go:embed agents.yaml
var agentsYAML []byte

type listFile struct {
	Version string  `yaml:"version"`
	Agents  []Agent `yaml:"agents"`
}

var (
	list        listFile
	lowerTokens []string // lowercased Token, index-aligned with list.Agents
)

func init() {
	l, err := parseList(agentsYAML)
	if err != nil {
		// The file is embedded and tested; a bad one never ships.
		panic(err)
	}
	list = l
	lowerTokens = make([]string, len(l.Agents))
	for i, a := range l.Agents {
		lowerTokens[i] = strings.ToLower(a.Token)
	}
}

func parseList(b []byte) (listFile, error) {
	var l listFile
	if err := yaml.Unmarshal(b, &l); err != nil {
		return l, fmt.Errorf("agentua: parse agents.yaml: %w", err)
	}
	if l.Version == "" {
		return l, fmt.Errorf("agentua: agents.yaml has no version")
	}
	for i, a := range l.Agents {
		if a.Token == "" || a.Name == "" || a.Operator == "" || !a.Class.Valid() || a.Class == ClassUnknown {
			return l, fmt.Errorf("agentua: agents.yaml entry %d (%q) needs token, name, operator and a class", i, a.Token)
		}
	}
	return l, nil
}

// Version is the embedded list's version, e.g. "2026-11-27.1". It changes
// whenever agents.yaml does.
func Version() string { return list.Version }

// Agents returns a copy of the embedded list, in match order.
func Agents() []Agent {
	out := make([]Agent, len(list.Agents))
	copy(out, list.Agents)
	return out
}

// Lookup returns the list entry named name.
func Lookup(name string) (Agent, bool) {
	for _, a := range list.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// stackUA bounds the UA length lowercased without a heap allocation; longer
// UAs (rare) are lowercased with strings.ToLower.
const stackUA = 512

// Classify names what userAgent claims to be. The first list entry whose
// token appears in it on word boundaries wins; failing that, a UA that
// self-declares as a bot is ClassOtherBot, a Mozilla-style UA is
// ClassBrowser, and anything else (including "") is ClassUnknown.
func Classify(userAgent string) Match {
	if userAgent == "" {
		return Match{Class: ClassUnknown}
	}
	var buf [stackUA]byte
	var ua string
	if len(userAgent) <= stackUA {
		b := buf[:len(userAgent)]
		for i := 0; i < len(userAgent); i++ {
			c := userAgent[i]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			b[i] = c
		}
		ua = unsafeString(b)
	} else {
		ua = strings.ToLower(userAgent)
	}
	for i, tok := range lowerTokens {
		if containsToken(ua, tok) {
			a := &list.Agents[i]
			return Match{Class: a.Class, Agent: a.Name, Operator: a.Operator}
		}
	}
	for _, w := range botWords {
		if strings.Contains(ua, w) {
			return Match{Class: ClassOtherBot}
		}
	}
	if strings.HasPrefix(ua, "mozilla/") || strings.HasPrefix(ua, "opera/") {
		return Match{Class: ClassBrowser}
	}
	return Match{Class: ClassUnknown}
}

// botWords mark a self-declared bot not on the list.
var botWords = []string{"bot", "crawl", "spider", "scraper", "fetcher", "slurp"}

// containsToken reports whether tok occurs in ua with a word boundary on both
// sides. Before the token a boundary is anything but a letter, digit, '-' or
// '_' (so "user" never matches inside "claude-user"); after it, anything but
// a letter or digit (so "googlebot" matches "googlebot-image" but "applebot"
// never matches "applebotx"). Both strings are lowercase.
func containsToken(ua, tok string) bool {
	for off := 0; ; {
		i := strings.Index(ua[off:], tok)
		if i < 0 {
			return false
		}
		start := off + i
		end := start + len(tok)
		if (start == 0 || !isWordByte(ua[start-1]) && ua[start-1] != '-' && ua[start-1] != '_') &&
			(end == len(ua) || !isWordByte(ua[end])) {
			return true
		}
		off = start + 1
	}
}

func isWordByte(c byte) bool {
	return 'a' <= c && c <= 'z' || '0' <= c && c <= '9'
}
