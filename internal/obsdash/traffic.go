package obsdash

import (
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/pkg/obsquery"
)

// The frontend dashboard's edge groups, for a project with a frontend some
// environment binds to Reliant hosting (Input.HostedStaticFrontends): Traffic
// and Bots & agents, read from the CDN's request logs (the tenant table
// edge_requests) the control plane writes for a hosted site. Every panel is a
// sql panel over pkg/obsquery's statements, so the console, `forge obs
// dashboards` and `forge obs traffic` count the same rows the same way. A
// local stack has no edge table, and HyperDX draws no sql panel, so the local
// dashboards are unchanged.

const (
	sectionTraffic = "traffic"
	sectionBots    = "bots"
)

func edgeSections(in Input) []Section {
	sites := strings.Join(in.HostedStaticFrontends, ", ")
	return []Section{
		{ID: sectionTraffic, Title: "Traffic", Description: fmt.Sprintf("Every request the CDN served for %s, cache hits and agents included, from its request logs (Reliant hosting only).", sites)},
		{ID: sectionBots, Title: "Bots & agents", Description: "Which crawlers and AI agents read the site, whether their requests came from the operator's published ranges, and what they fetched."},
	}
}

func edgePanels() []Panel {
	sql := func(section, id, title, desc, stmt string, unit Unit, w, h int) Panel {
		return Panel{Section: section, ID: id, Title: title, Description: desc, Kind: KindSQL, SQL: stmt, Unit: unit, Width: w, Height: h}
	}
	return []Panel{
		sql(sectionTraffic, "edge-summary", "Edge requests", "Requests, cache-hit ratio, 4xx, 5xx and bytes across the env's sites, each request counted by its sample weight.", obsquery.TrafficSummarySQL(), UnitNone, 24, 4),
		sql(sectionTraffic, "edge-by-class", "Requests by class", "Requests over time, one line per user-agent class (browser, search_crawler, ai_crawler, ai_search, ai_user, tool, monitor, other_bot, unknown).", obsquery.TrafficByClassSQL(), UnitCount, 24, 7),
		sql(sectionTraffic, "edge-top-pages", "Top pages", "The most requested pages, split into humans (browsers), agents (AI search and assistant fetches, tools) and crawlers (search and AI crawlers, monitors, other bots). Unknown user agents count in requests only.", obsquery.TrafficTopPagesSQL(), UnitCount, 24, 8),
		sql(sectionTraffic, "edge-404s", "404s: scanner probes and real", "Not-found responses over time. Scanner probes are requests for secrets and exploits any host gets (.env, .git, /@fs/, /proc/, wp-, .php, keys); real ones are broken links worth fixing.", obsquery.TrafficNotFoundSQL(), UnitCount, 12, 7),
		sql(sectionTraffic, "edge-top-404s", "Top 404 paths", "The most requested missing paths, each marked scanner or real.", obsquery.TrafficTopNotFoundSQL(), UnitCount, 12, 7),
		sql(sectionTraffic, "edge-cache-hit", "Cache-hit ratio", "The share of requests the CDN answered from its cache.", obsquery.TrafficCacheSQL(), UnitPercent, 12, 7),
		sql(sectionTraffic, "edge-bytes", "Bytes served", "Response bytes over time, by audience.", obsquery.TrafficBytesSQL(), UnitBytes, 12, 7),
		sql(sectionBots, "bots-by-agent", "Requests by agent", "Each known agent with its operator and class; a request from outside the operator's published IP ranges is its own row, verified no. .md and llms.txt count its fetches of Markdown twins and llms.txt.", obsquery.BotsByAgentSQL(), UnitCount, 24, 8),
		sql(sectionBots, "bots-verified", "Verified and claimed", "Requests per agent name: verified came from the operator's published ranges; claimed only is the user agent with nothing behind it, the shape of a spoofed probe.", obsquery.BotsVerifiedSQL(), UnitCount, 12, 8),
		sql(sectionBots, "bots-discovery", "llms.txt, .md twins and robots.txt", "Which agents fetched the files written for them.", obsquery.BotsDiscoverySQL(), UnitCount, 12, 8),
		sql(sectionBots, "bots-top-pages", "Top pages per agent", "Each agent's three most requested pages, the busiest agents first.", obsquery.BotsTopPagesSQL(), UnitCount, 24, 8),
	}
}

// addEdgeGroups adds the Traffic and Bots & agents groups to d when the
// project has a hosted static frontend.
func addEdgeGroups(d *Dashboard, in Input) {
	if len(in.HostedStaticFrontends) == 0 {
		return
	}
	d.Sections = append(d.Sections, edgeSections(in)...)
	d.Panels = append(d.Panels, edgePanels()...)
}
