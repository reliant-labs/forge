package cli

// The KCL edit behind `forge vuln exempt`: add (or update) one
// forge.VulnExemption in an env's main.k, as text.
//
// TEXT, NOT A RE-RENDER. main.k is the project's own file, with its comments,
// its layout and its imports; forge has no KCL printer that would round-trip
// it, and a rewrite that dropped a comment would be a worse change than the
// one asked for. So the edit is the smallest insertion that states the
// exemption, at the one place in the file that declares the gate:
//
//  1. an existing forge.VulnExemption with the same id: replaced in place;
//  2. else the one `vuln_gate = <alias>.VulnPolicy {…}` literal: its
//     `exemptions = [...]` gains an element, or the literal gains
//     `exemptions = [...]`;
//  3. else the one `delivery_policy = <alias>.DeliveryPolicy {…}` literal
//     gains `vuln_gate = <alias>.VulnPolicy {exemptions = [...]}`;
//  4. else the one `<alias>.Bundle {…}` literal gains a delivery_policy.
//
// Anything else — two candidate literals (a list of policies, a lambda), a
// policy built in another file — is refused with the exact lines to add, not
// guessed at. The caller renders the env after writing and restores the file
// unless the rendered policy carries the exemption, so an edit that parsed
// but landed somewhere the env does not read is undone, never kept.

import (
	"fmt"
	"regexp"
	"strings"
)

// vulnExemptionLiteral is the KCL for one exemption, on one line.
func vulnExemptionLiteral(alias, id, reason, expires string) string {
	return fmt.Sprintf("%s.VulnExemption {id = %s, reason = %s, expires = %s}", alias, kclQuote(id), kclQuote(reason), kclQuote(expires))
}

// kclQuote is a KCL double-quoted string literal. The caller refuses what
// it cannot spell safely (a newline, `${`, which KCL would interpolate).
func kclQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// kclCodeMask returns src with every byte inside a string literal or a
// comment replaced by a space (newlines kept), so a pattern or a bracket is
// matched only where it is code.
func kclCodeMask(src string) string {
	b := []byte(src)
	n := len(b)
	blank := func(i int) {
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
	for i := 0; i < n; {
		switch c := b[i]; {
		case c == '#':
			for i < n && b[i] != '\n' {
				blank(i)
				i++
			}
		case c == '"' || c == '\'':
			q := c
			triple := i+2 < n && b[i+1] == q && b[i+2] == q
			if triple {
				blank(i)
				blank(i + 1)
				blank(i + 2)
				i += 3
				for i < n {
					if b[i] == '\\' && i+1 < n {
						blank(i)
						blank(i + 1)
						i += 2
						continue
					}
					if i+2 < n && b[i] == q && b[i+1] == q && b[i+2] == q {
						blank(i)
						blank(i + 1)
						blank(i + 2)
						i += 3
						break
					}
					blank(i)
					i++
				}
				continue
			}
			blank(i)
			i++
			for i < n && b[i] != '\n' {
				if b[i] == '\\' && i+1 < n {
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				if b[i] == q {
					blank(i)
					i++
					break
				}
				blank(i)
				i++
			}
		default:
			i++
		}
	}
	return string(b)
}

// kclMatch returns the index of the bracket closing the one opening at open
// in the masked source, or -1.
func kclMatch(mask string, open int) int {
	pairs := map[byte]byte{'{': '}', '[': ']', '(': ')'}
	want := []byte{pairs[mask[open]]}
	for i := open + 1; i < len(mask); i++ {
		c := mask[i]
		switch c {
		case '{', '[', '(':
			want = append(want, pairs[c])
		case '}', ']', ')':
			if len(want) == 0 || c != want[len(want)-1] {
				return -1
			}
			want = want[:len(want)-1]
			if len(want) == 0 {
				return i
			}
		}
	}
	return -1
}

// kclLiteral is one `<alias>.<Schema> {…}` literal: its alias, and the
// offsets of its braces.
type kclLiteral struct {
	alias       string
	open, close int
}

// kclFindLiterals finds every literal of schema in the masked source, each
// with the field it is assigned to when one is (field = "" for any).
func kclFindLiterals(mask, field, schema string) []kclLiteral {
	prefix := ``
	if field != "" {
		prefix = `\b` + regexp.QuoteMeta(field) + `\s*[:=]\s*`
	}
	re := regexp.MustCompile(prefix + `\b([A-Za-z_][A-Za-z0-9_]*)\.` + regexp.QuoteMeta(schema) + `\s*\{`)
	var out []kclLiteral
	for _, m := range re.FindAllStringSubmatchIndex(mask, -1) {
		open := m[1] - 1
		close := kclMatch(mask, open)
		if close < 0 {
			continue
		}
		out = append(out, kclLiteral{alias: mask[m[2]:m[3]], open: open, close: close})
	}
	return out
}

// lineIndent is the leading whitespace of the line holding offset i.
func lineIndent(src string, i int) string {
	start := strings.LastIndexByte(src[:i], '\n') + 1
	j := start
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	return src[start:j]
}

// kclAddEntry adds `entry` (a `field = value` line) to the config literal
// whose braces are at open/close, matching its layout: one line stays one
// line (`, entry`), a multi-line literal gains a line before its closing
// brace, indented like its other entries.
func kclAddEntry(src, mask string, open, close int, entry string) string {
	body := mask[open+1 : close]
	if !strings.Contains(body, "\n") {
		if strings.TrimSpace(body) == "" {
			return src[:open+1] + entry + src[close:]
		}
		end := close
		for end > open+1 && (src[end-1] == ' ' || src[end-1] == '\t') {
			end--
		}
		sep := ", "
		if strings.HasSuffix(strings.TrimRight(mask[open+1:end], " \t"), ",") {
			sep = " "
		}
		return src[:end] + sep + entry + src[end:]
	}
	closeIndent := lineIndent(src, close)
	indent := closeIndent + "    "
	// The indentation of the literal's first entry, when it has one.
	for _, line := range strings.Split(body, "\n")[1:] {
		if t := strings.TrimSpace(line); t != "" && t != "}" {
			indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			break
		}
	}
	lineStart := strings.LastIndexByte(src[:close], '\n') + 1
	if strings.TrimSpace(src[lineStart:close]) != "" {
		// `...last entry}` on one line: break before the brace.
		return src[:close] + "\n" + indent + entry + "\n" + closeIndent + src[close:]
	}
	return src[:lineStart] + indent + entry + "\n" + src[lineStart:]
}

// kclAddListElement appends elem to the list whose brackets are at
// open/close, matching its layout.
func kclAddListElement(src, mask string, open, close int, elem string) string {
	body := mask[open+1 : close]
	if strings.TrimSpace(body) == "" {
		return src[:open+1] + elem + src[close:]
	}
	if !strings.Contains(body, "\n") {
		end := close
		for end > open+1 && (src[end-1] == ' ' || src[end-1] == '\t') {
			end--
		}
		sep := ", "
		if strings.HasSuffix(strings.TrimRight(mask[open+1:end], " \t"), ",") {
			sep = " "
		}
		return src[:end] + sep + elem + src[end:]
	}
	return kclAddEntry(src, mask, open, close, elem)
}

// errVulnExemptNoAnchor is the refusal when the file has no single place to
// state the exemption.
type errVulnExemptNoAnchor struct {
	why     string
	snippet string
}

func (e *errVulnExemptNoAnchor) Error() string {
	return e.why + "\n  Add it where the env declares its delivery policy:\n\n" + e.snippet
}

// exemptInKCL returns src with the exemption stated, and whether it changed
// anything (false when the same exemption is already there, byte for byte).
func exemptInKCL(src, id, reason, expires string) (string, bool, error) {
	mask := kclCodeMask(src)

	// 1. The same id, already exempt: replace its literal.
	for _, lit := range kclFindLiterals(mask, "", "VulnExemption") {
		inner := src[lit.open+1 : lit.close]
		if !regexp.MustCompile(`\bid\s*=\s*"` + regexp.QuoteMeta(id) + `"`).MatchString(inner) {
			continue
		}
		start := strings.LastIndex(mask[:lit.open], lit.alias+".VulnExemption")
		want := vulnExemptionLiteral(lit.alias, id, reason, expires)
		if src[start:lit.close+1] == want {
			return src, false, nil
		}
		return src[:start] + want + src[lit.close+1:], true, nil
	}

	snippet := func(alias string) string {
		return fmt.Sprintf("    delivery_policy = %[1]s.DeliveryPolicy {\n        vuln_gate = %[1]s.VulnPolicy {\n            exemptions = [\n                %[2]s\n            ]\n        }\n    }\n", alias, vulnExemptionLiteral(alias, id, reason, expires))
	}

	// 2. The vuln_gate literal.
	gates := kclFindLiterals(mask, "vuln_gate", "VulnPolicy")
	if len(gates) > 1 {
		return "", false, &errVulnExemptNoAnchor{why: fmt.Sprintf("this file declares vuln_gate %d times, so forge cannot tell which one the env uses", len(gates)), snippet: snippet(gates[0].alias)}
	}
	if len(gates) == 1 {
		g := gates[0]
		elem := vulnExemptionLiteral(g.alias, id, reason, expires)
		lists := regexp.MustCompile(`\bexemptions\s*=\s*\[`).FindAllStringIndex(mask[g.open:g.close], -1)
		var top []int
		for _, l := range lists {
			at := g.open + l[1] - 1
			if kclDepth(mask, g.open, at) == 1 {
				top = append(top, at)
			}
		}
		switch len(top) {
		case 0:
			return kclAddEntry(src, mask, g.open, g.close, "exemptions = ["+elem+"]"), true, nil
		case 1:
			close := kclMatch(mask, top[0])
			if close < 0 {
				return "", false, fmt.Errorf("vuln_gate's exemptions list does not close")
			}
			return kclAddListElement(src, mask, top[0], close, elem), true, nil
		default:
			return "", false, &errVulnExemptNoAnchor{why: "vuln_gate sets exemptions twice", snippet: snippet(g.alias)}
		}
	}

	// 3. The delivery_policy literal.
	policies := kclFindLiterals(mask, "delivery_policy", "DeliveryPolicy")
	if len(policies) > 1 {
		return "", false, &errVulnExemptNoAnchor{why: fmt.Sprintf("this file declares delivery_policy %d times, so forge cannot tell which one the env uses", len(policies)), snippet: snippet(policies[0].alias)}
	}
	if len(policies) == 1 {
		p := policies[0]
		entry := fmt.Sprintf("vuln_gate = %s.VulnPolicy {exemptions = [%s]}", p.alias, vulnExemptionLiteral(p.alias, id, reason, expires))
		return kclAddEntry(src, mask, p.open, p.close, entry), true, nil
	}

	// 4. The Bundle literal.
	bundles := kclFindLiterals(mask, "", "Bundle")
	if len(bundles) == 1 {
		b := bundles[0]
		entry := fmt.Sprintf("delivery_policy = %[1]s.DeliveryPolicy {vuln_gate = %[1]s.VulnPolicy {exemptions = [%[2]s]}}", b.alias, vulnExemptionLiteral(b.alias, id, reason, expires))
		return kclAddEntry(src, mask, b.open, b.close, entry), true, nil
	}
	return "", false, &errVulnExemptNoAnchor{
		why:     "this file has no single vuln_gate, delivery_policy or Bundle literal to add the exemption to (the env's policy may be built in another file)",
		snippet: snippet("forge"),
	}
}

// kclDepth is how many brackets are open at offset at, counting from from
// (exclusive of the bracket at from itself it counts as 1).
func kclDepth(mask string, from, at int) int {
	depth := 0
	for i := from; i < at; i++ {
		switch mask[i] {
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		}
	}
	return depth
}
