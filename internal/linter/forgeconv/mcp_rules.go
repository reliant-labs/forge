// File: internal/linter/forgeconv/mcp_rules.go
//
// The MCP lint lane (docs/proposals/mcp-from-rpcs.md §6): three rules over
// the rpcs a project opts in to serving as MCP tools.
//
//   - forgeconv-mcp-described: a model picks a tool from its name and
//     description alone, so an opted-in rpc needs a leading comment of at
//     least mcpMinDescriptionWords words that says more than its name, and
//     every field of its request needs a comment.
//   - forgeconv-mcp-mutating: an opted-in rpc that mutates and lacks
//     `mcp_mutating: true` is not served (this says why), and
//     `mcp_mutating` on an rpc that does not mutate declares nothing.
//   - forgeconv-mcp-shape: `mcp` on a streaming rpc, two rpcs projecting to
//     one tool name, and (a warning) more than mcpMaxToolsPerListener tools
//     on one listener.
//
// ── Source of truth ──────────────────────────────────────────────────────
//
// gen/forge_descriptor/, whose methods carry MCP (the effective opt-in,
// mcp_mutating, and activity.IsMutating's verdict) and Description, as
// `forge generate` recorded them from the COMPILED descriptors — the same
// inputs codegen.MCPTools decides the served set from, so lint cannot
// disagree with what is served. The raw proto scan places each finding on
// its rpc's line and reads the request fields' comments, which the
// descriptor does not carry.
//
// No descriptor yields nothing (every other lane already says generate
// first). A descriptor that records no MCP declaration while the source
// declares one predates the fields and yields one finding asking for
// `forge generate`. Each rule is warn-first through the rollout table.

package forgeconv

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

// The stable rule IDs.
const (
	ruleMCPDescribed = "forgeconv-mcp-described"
	ruleMCPMutating  = "forgeconv-mcp-mutating"
	ruleMCPShape     = "forgeconv-mcp-shape"
)

// mcpMinDescriptionWords is the shortest rpc comment that can tell a model
// when to call the tool.
const mcpMinDescriptionWords = 8

// mcpMaxToolsPerListener is the tool count past which clients degrade at
// choosing among them (§6: serve the 5 to 30 operations an agent needs).
const mcpMaxToolsPerListener = 40

const mcpSkill = " (forge skill load api-mcp)"

// reMCPOption finds an MCP declaration in proto source.
var reMCPOption = regexp.MustCompile(`\bmcp(_mutating)?\s*:`)

// mcpRestateStopwords are the words a description may add to an rpc's name
// and still say nothing more than the name.
var mcpRestateStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "this": true, "that": true, "rpc": true, "it": true,
	"of": true, "for": true, "by": true, "and": true, "or": true, "to": true, "in": true,
	"all": true, "returns": true, "return": true, "given": true, "one": true, "is": true,
}

// checkMCP runs the three MCP rules over the tree rooted at rootDir (the
// project's proto/ directory) and returns findings keyed by each file's
// path relative to rootDir.
func checkMCP(rootDir string, protoFiles []string) map[string][]Finding {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil
	}
	services, err := codegen.ParseDeclaredServices(filepath.Dir(absRoot))
	if err != nil || len(services) == 0 {
		// forgeconv-activity-declared already reports an unreadable
		// descriptor; no descriptor means nothing generated yet.
		return nil
	}

	lines := map[string]int{}
	sources := map[string]string{} // rel file → content
	declared := false
	for _, f := range protoFiles {
		content, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absRoot, absPath(f))
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		sources[rel] = string(content)
		if reMCPOption.MatchString(stripLineComments(string(content))) {
			declared = true
		}
		for _, svc := range parseProtoFile(f, string(content)).Services {
			for _, m := range svc.Methods {
				lines[rel+"|"+svc.Name+"|"+m.Name] = m.Line
			}
		}
	}

	out := map[string][]Finding{}
	add := func(file string, f Finding) {
		key := filepath.FromSlash(file)
		f.File = key
		out[key] = append(out[key], f)
	}

	recorded := false
	type served struct {
		svc  codegen.ServiceDef
		m    codegen.Method
		name string
	}
	var tools []served
	byName := map[string]string{}
	for _, svc := range services {
		file := filepath.ToSlash(svc.ProtoFile)
		for _, m := range svc.Methods {
			if m.MCP == nil {
				continue
			}
			recorded = true
			line := lines[file+"|"+svc.Name+"|"+m.Name]
			at := func(rule string, sev Severity, msg, fix string) {
				add(file, Finding{Rule: rule, Severity: sev, Line: line, Message: msg, Remediation: fix + mcpSkill})
			}
			rpc := svc.Name + "." + m.Name

			// forgeconv-mcp-mutating
			switch {
			case m.MCP.OptIn && m.MCP.Mutating && !m.MCP.MutatingAck:
				at(ruleMCPMutating, SeverityError,
					fmt.Sprintf("rpc %s opts in to MCP but mutates (its name is not a read verb and it is not NO_SIDE_EFFECTS), so it is NOT served: a mutating rpc is served only with mcp_mutating: true", rpc),
					"if an agent should be able to call it, add mcp_mutating: true to its (forge.v1.method) option (it is announced destructiveHint); otherwise set mcp: false on it")
			case m.MCP.MutatingAck && !m.MCP.Mutating:
				at(ruleMCPMutating, SeverityError,
					fmt.Sprintf("rpc %s declares mcp_mutating: true but does not mutate (a read verb or NO_SIDE_EFFECTS), so the acknowledgement declares nothing", rpc),
					"remove mcp_mutating from its (forge.v1.method) option")
			}

			if !m.MCP.OptIn {
				continue
			}
			// forgeconv-mcp-shape: streaming.
			if m.ClientStreaming || m.ServerStreaming {
				at(ruleMCPShape, SeverityError,
					fmt.Sprintf("rpc %s is streaming and opts in to MCP; MCP tools are request/response, so `forge generate` refuses it", rpc),
					"set (forge.v1.method).mcp = false on it")
				continue
			}

			// forgeconv-mcp-described
			if msg := describeProblem(m.Name, m.Description); msg != "" {
				at(ruleMCPDescribed, SeverityError, fmt.Sprintf("rpc %s is an MCP tool and %s; a model chooses a tool from its name and description alone", rpc, msg),
					"write the rpc's leading comment as when-to-call guidance: what it does, when an agent should call it, and its error cases")
			}
			if missing := uncommentedRequestFields(sources, m); len(missing) > 0 {
				at(ruleMCPDescribed, SeverityError,
					fmt.Sprintf("rpc %s is an MCP tool, but its request fields %s have no comment, so a model has only their names to fill them from", rpc, strings.Join(missing, ", ")),
					"comment each field of "+shortName(m)+": what it holds, its format and its default")
			}

			if m.MCP.Mutating && !m.MCP.MutatingAck {
				continue
			}
			// forgeconv-mcp-shape: name collision.
			name := codegen.MCPToolName(svc.Name, m.Name)
			if prev, ok := byName[name]; ok {
				at(ruleMCPShape, SeverityError,
					fmt.Sprintf("rpc %s projects to MCP tool name %q, already %s's; `forge generate` refuses two tools with one name", rpc, name, prev),
					"rename one rpc, or opt one out with mcp: false")
				continue
			}
			byName[name] = rpc
			tools = append(tools, served{svc, m, name})
		}
	}

	if declared && !recorded {
		out[""] = append(out[""], Finding{
			Rule:        ruleMCPShape,
			Severity:    SeverityError,
			Message:     "gen/forge_descriptor/ predates MCP declarations, so they cannot be checked",
			Remediation: "run `forge generate`",
		})
	}

	// forgeconv-mcp-shape: tools per listener. A listener mounts the
	// services of one audience, so tools are counted per audience.
	byAudience := map[string][]served{}
	for _, t := range tools {
		aud := t.svc.Audience
		if aud == "" {
			aud = "user"
		}
		byAudience[aud] = append(byAudience[aud], t)
	}
	auds := make([]string, 0, len(byAudience))
	for a := range byAudience {
		auds = append(auds, a)
	}
	sort.Strings(auds)
	for _, aud := range auds {
		ts := byAudience[aud]
		if len(ts) <= mcpMaxToolsPerListener {
			continue
		}
		first := ts[mcpMaxToolsPerListener]
		file := filepath.ToSlash(first.svc.ProtoFile)
		add(file, Finding{
			Rule:     ruleMCPShape,
			Severity: SeverityWarning,
			Line:     lines[file+"|"+first.svc.Name+"|"+first.m.Name],
			Message: fmt.Sprintf("the %q listener serves %d MCP tools (more than %d, from %s on); a model chooses worse among many tools and every tool's schema costs context",
				aud, len(ts), mcpMaxToolsPerListener, first.name),
			Remediation: "serve the 5 to 30 operations an agent needs, not the API: prefer coarse, task-shaped rpcs over a CRUD quintet per entity, and set mcp: false on the rest" + mcpSkill,
		})
	}
	return out
}

// describeProblem says what is wrong with an MCP tool's description, or "".
func describeProblem(rpcName, desc string) string {
	words := strings.Fields(desc)
	if len(words) == 0 {
		return "has no leading comment"
	}
	if restatesName(rpcName, words) {
		return fmt.Sprintf("its comment %q only restates its name", strings.TrimSpace(desc))
	}
	if len(words) < mcpMinDescriptionWords {
		return fmt.Sprintf("its comment %q is %d words (under %d)", strings.TrimSpace(desc), len(words), mcpMinDescriptionWords)
	}
	return ""
}

// restatesName reports a description whose words, past the rpc's own name
// words and filler, add fewer than two ("ListOrders lists orders").
func restatesName(rpcName string, words []string) bool {
	nameWords := map[string]bool{}
	for _, w := range strings.Split(naming.ToSnakeCase(rpcName), "_") {
		nameWords[stem(w)] = true
	}
	nameWords[stem(strings.ToLower(rpcName))] = true
	extra := 0
	for _, w := range words {
		w = strings.ToLower(strings.Trim(w, ".,;:!?()`'\""))
		if w == "" || mcpRestateStopwords[w] || nameWords[stem(w)] {
			continue
		}
		extra++
	}
	return extra < 2
}

// stem folds the plural and third-person -s/-es endings a restated name
// takes ("lists orders" restates ListOrder).
func stem(w string) string {
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "es") && len(w) > 3:
		return strings.TrimSuffix(w[:len(w)-2], "e")
	case strings.HasSuffix(w, "s") && len(w) > 2:
		return strings.TrimSuffix(w[:len(w)-1], "e")
	}
	return strings.TrimSuffix(w, "e")
}

// shortName is the request message's name within its package.
func shortName(m codegen.Method) string {
	n := m.InputType
	if m.InputTypeFQ != "" {
		n = m.InputTypeFQ
	}
	if i := strings.LastIndexByte(n, '.'); i >= 0 {
		n = n[i+1:]
	}
	return n
}

var reFieldDecl = regexp.MustCompile(`^\s*(?:optional\s+|repeated\s+)?(?:map\s*<[^>]*>|[\w.]+)\s+(\w+)\s*=\s*\d+`)

// uncommentedRequestFields names the top-level fields of m's request
// message that carry no comment (a // or /* */ comment on the line above,
// or a trailing // on the same line). An empty request, or one whose file
// is not in the tree, has none.
func uncommentedRequestFields(sources map[string]string, m codegen.Method) []string {
	if m.InputType == "google.protobuf.Empty" || m.InputTypeFQ == "google.protobuf.Empty" {
		return nil
	}
	src, ok := sources[filepath.ToSlash(m.InputProtoFile)]
	if !ok {
		return nil
	}
	reMsg := regexp.MustCompile(`^\s*message\s+` + regexp.QuoteMeta(shortName(m)) + `\s*\{`)
	ls := strings.Split(src, "\n")
	start := -1
	for i, l := range ls {
		if reMsg.MatchString(l) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var missing []string
	depth := 0
	for i := start; i < len(ls); i++ {
		code := ls[i]
		comment := ""
		if j := strings.Index(code, "//"); j >= 0 {
			code, comment = code[:j], code[j:]
		}
		if i > start && depth == 1 {
			if fm := reFieldDecl.FindStringSubmatch(code); fm != nil {
				prev := ""
				if i > 0 {
					prev = strings.TrimSpace(ls[i-1])
				}
				commented := strings.TrimSpace(strings.TrimPrefix(comment, "//")) != "" ||
					strings.HasPrefix(prev, "//") || strings.HasSuffix(prev, "*/")
				if !commented {
					missing = append(missing, fm[1])
				}
			}
		}
		depth += strings.Count(code, "{") - strings.Count(code, "}")
		if i > start && depth <= 0 {
			break
		}
	}
	return missing
}

// stripLineComments drops // comments so a comment mentioning `mcp:` is not
// read as a declaration.
func stripLineComments(s string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		if j := strings.Index(l, "//"); j >= 0 {
			ls[i] = l[:j]
		}
	}
	return strings.Join(ls, "\n")
}
