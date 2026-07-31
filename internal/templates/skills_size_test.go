// skills_size_test.go — guards the delivered size of every shipped skill.
//
// A skill is not delivered as the bytes on disk. `forge generate`
// (internal/cli/generate_skills.go) renders each SKILL.md by inserting a
// generated-by banner plus the shared agent-operation preamble right after
// the YAML frontmatter, so the file that lands in a project's
// .claude/skills/ is roughly 1.1 KB larger than the template here.
//
// The consumer caps a single delivered skill at maxDeliveredSkillBytes and
// truncates anything larger. A truncated skill loses its tail — which is
// exactly where a skill's sub-skill pointers, related-skill links and
// suggested-tools list live — so shipping an oversize skill silently
// severs guidance rather than merely shortening it.
//
// This test measures the REAL delivered size. The banner format string and
// the preamble constant are read out of internal/cli/generate_skills.go at
// test time rather than copied here, so editing the preamble automatically
// re-tightens the template budget instead of leaving a stale constant
// behind. Extraction failures fail the test — a guard that quietly falls
// back to zero overhead is worse than no guard.
package templates_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	// maxDeliveredSkillBytes is the consumer's hard ceiling on one
	// delivered skill. Past it the tail is cut off.
	//
	// It mirrors reliant's MaxSkillBodySize (internal/llm/tools/
	// output_limiter.go), which is itself MaxOutputSize — the general
	// tool-output ceiling. forge cannot import it: reliant depends on forge,
	// not the reverse. So this is a copy, and it is the one number in this
	// file that can silently disagree with the consumer. Raising the
	// consumer's ceiling without raising this one only makes the guard
	// stricter than reality, which is safe; LOWERING it there without
	// lowering it here would let a skill ship that gets cut in the field.
	maxDeliveredSkillBytes = 24_000

	// deliveredSkillHeadroom is the room the rendered file must leave for
	// what the consumer wraps around the capped body.
	//
	// It is small because the cap applies to the BODY ALONE. Both consumer
	// paths — the preload seed in call_llm.go and the skill tool via
	// TruncateOutput — call CapSkillContent on the body and only then write
	// the <skill name=… path=…> envelope into a separate buffer, so the
	// envelope never competes with the body for the ceiling. The skill
	// branch of TruncateOutput returns early precisely so nothing else is
	// appended.
	//
	// 150 bytes covers that envelope with room to grow: measured at 107
	// bytes for the longest name and path a nested sub-skill produces
	// (`forge/dev/render-options`).
	//
	// It was 500, chosen against a belief that sub-skill, related-skill and
	// suggested-tools pointers were appended to the body before capping.
	// They are not — the skill branch of TruncateOutput returns immediately
	// after CapSkillContent — so those 350 bytes were rejecting skills that
	// deliver whole and intact.
	deliveredSkillHeadroom = 150

	// longestPlausibleVersion stands in for the "%s" the banner
	// interpolates. Rendering against a deliberately long version makes
	// the measurement worst-case rather than dev-build-case.
	longestPlausibleVersion = "v100.100.100-rc.1"

	// generateSkillsSrc is the file whose banner + preamble the delivered
	// size depends on.
	generateSkillsSrc = "../cli/generate_skills.go"
)

// skillDeliveryOverhead returns the exact byte count generate_skills.go
// prepends to every skill body: the generated-by banner, a blank line, and
// the shared agent-operation preamble.
func skillDeliveryOverhead(t *testing.T) int {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, generateSkillsSrc, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", generateSkillsSrc, err)
	}

	preamble := constStringValue(t, file, "agentOperationPreamble")
	banner := strings.ReplaceAll(bannerFormat(t, file), "%s", longestPlausibleVersion)

	// renderAgentSkill: banner + "\n\n" + preamble.
	return len(banner) + 2 + len(preamble)
}

// constStringValue evaluates a top-level `const name = "..."` whose value
// is a (possibly `+`-concatenated) chain of string literals.
func constStringValue(t *testing.T, file *ast.File, name string) string {
	t.Helper()
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if ident.Name != name || i >= len(vs.Values) {
					continue
				}
				return evalStringExpr(t, vs.Values[i])
			}
		}
	}
	t.Fatalf("%s: const %s not found — the delivery-overhead guard can no longer measure what forge prepends", generateSkillsSrc, name)
	return ""
}

// bannerFormat returns the fmt.Sprintf format string skillGeneratedBanner
// renders the DO-NOT-EDIT banner from.
func bannerFormat(t *testing.T, file *ast.File) string {
	t.Helper()
	var format string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "skillGeneratedBanner" {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Sprintf" {
				return true
			}
			format = evalStringExpr(t, call.Args[0])
			return false
		})
		return false
	})
	if format == "" {
		t.Fatalf("%s: could not extract the banner format string from skillGeneratedBanner — the delivery-overhead guard can no longer measure what forge prepends", generateSkillsSrc)
	}
	return format
}

// evalStringExpr evaluates a string literal or a `+` chain of them.
func evalStringExpr(t *testing.T, expr ast.Expr) string {
	t.Helper()
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			t.Fatalf("%s: expected a string literal, got %s", generateSkillsSrc, e.Kind)
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			t.Fatalf("%s: unquote string literal: %v", generateSkillsSrc, err)
		}
		return v
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			t.Fatalf("%s: expected string concatenation, got operator %s", generateSkillsSrc, e.Op)
		}
		return evalStringExpr(t, e.X) + evalStringExpr(t, e.Y)
	case *ast.ParenExpr:
		return evalStringExpr(t, e.X)
	default:
		t.Fatalf("%s: cannot evaluate %T as a string constant", generateSkillsSrc, expr)
		return ""
	}
}

// TestShippedSkillsFitDeliveryBudget fails when any shipped SKILL.md would
// be truncated on delivery. The fix is to tighten the skill or split a
// separable topic into a sibling `<skill>/<subtopic>/SKILL.md` — never to
// raise the cap, which just moves the cliff.
func TestShippedSkillsFitDeliveryBudget(t *testing.T) {
	t.Parallel()

	overhead := skillDeliveryOverhead(t)
	if overhead <= 0 || overhead >= maxDeliveredSkillBytes {
		t.Fatalf("measured delivery overhead of %d bytes is implausible — the extraction from %s has drifted", overhead, generateSkillsSrc)
	}
	budget := maxDeliveredSkillBytes - deliveredSkillHeadroom

	type oversize struct {
		rel       string
		delivered int
	}
	var over []oversize
	for rel, content := range shippedSkills(t) {
		if delivered := len(content) + overhead; delivered > budget {
			over = append(over, oversize{rel: rel, delivered: delivered})
		}
	}
	sort.Slice(over, func(i, j int) bool { return over[i].delivered > over[j].delivered })

	for _, o := range over {
		t.Errorf("skills/%s: delivered size %d bytes exceeds the %d-byte budget by %d (template %d + %d banner/preamble; hard cap %d)\n"+
			"  Cut redundancy, or move a separable topic into a sibling %s/<subtopic>/SKILL.md and leave a one-line pointer.",
			o.rel, o.delivered, budget, o.delivered-budget, o.delivered-overhead, overhead,
			maxDeliveredSkillBytes, strings.TrimSuffix(o.rel, "/SKILL.md"))
	}
}

// TestSkillDeliveryOverheadIsMeasured is a self-test: if the AST
// extraction silently stops finding the banner or preamble, the budget
// guard above would go slack. This pins the measurement to a sane range
// and to the text forge actually prepends.
func TestSkillDeliveryOverheadIsMeasured(t *testing.T) {
	t.Parallel()

	overhead := skillDeliveryOverhead(t)
	if overhead < 800 || overhead > 4000 {
		t.Fatalf("delivery overhead measured at %d bytes, outside the plausible 800–4000 range — extraction from %s has drifted", overhead, generateSkillsSrc)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, generateSkillsSrc, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", generateSkillsSrc, err)
	}
	if got := bannerFormat(t, file); !strings.Contains(got, "Code generated by forge") {
		t.Errorf("extracted banner format does not look like the DO-NOT-EDIT banner: %q", got)
	}
	if got := constStringValue(t, file, "agentOperationPreamble"); !strings.Contains(got, "Operating forge as an agent") {
		t.Errorf("extracted preamble does not look like the agent-operation preamble: %q", firstLine(got))
	}
}

// TestShippedSkillPathsAreWellFormed keeps a split skill reachable: every
// SKILL.md must sit under skills/forge/... or skills/general/..., because
// listForgeShippedSkills derives a skill's path from exactly those two
// roots and skips anything else. A subtopic dropped in the wrong place
// would vanish from the catalog rather than fail loudly.
func TestShippedSkillPathsAreWellFormed(t *testing.T) {
	t.Parallel()

	for rel, content := range shippedSkills(t) {
		if !strings.HasPrefix(rel, "forge/") && !strings.HasPrefix(rel, "general/") {
			t.Errorf("skills/%s: shipped skills must live under skills/forge/ or skills/general/ — anything else is skipped by the catalog", rel)
		}
		if filepath.Base(rel) != "SKILL.md" {
			t.Errorf("skills/%s: expected a SKILL.md leaf", rel)
		}
		if !strings.HasPrefix(content, "---\n") {
			t.Errorf("skills/%s: must open with YAML frontmatter at byte 0 (the skill loader requires it)", rel)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
