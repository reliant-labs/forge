package conventions

import (
	"flag"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/observe/genai"
)

var update = flag.Bool("update", false, "rewrite kcl/genai_normalize_gen.k from Rules")

// Every golden vector is what Normalize makes of its input.
func TestNormalizeSatisfiesTheGoldenVectors(t *testing.T) {
	vs := Vectors()
	if len(vs) == 0 {
		t.Fatal("no golden vectors")
	}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			got := Normalize(v.TraceID, v.Input)
			if !reflect.DeepEqual(got, v.Want) {
				t.Errorf("Normalize:\n got %v\nwant %v\nadded %v", got, v.Want, added(v.Input, got))
			}
		})
	}
}

func added(in, out map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range out {
		if _, ok := in[k]; !ok {
			m[k] = v
		}
	}
	return m
}

// Normalisation is additive: every input attribute survives with its value,
// and the input map itself is never modified.
func TestNormalizeIsAdditive(t *testing.T) {
	for _, v := range Vectors() {
		before := maps.Clone(v.Input)
		got := Normalize(v.TraceID, v.Input)
		if !reflect.DeepEqual(v.Input, before) {
			t.Errorf("%s: Normalize modified its input", v.Name)
		}
		for k, want := range v.Input {
			if got[k] != want {
				t.Errorf("%s: %s = %q after normalisation, want the original %q", v.Name, k, got[k], want)
			}
		}
	}
}

// Normalising twice is normalising once: a span that passes two collectors
// (an agent and the gateway) is not changed by the second.
func TestNormalizeIsIdempotent(t *testing.T) {
	for _, v := range Vectors() {
		once := Normalize(v.TraceID, v.Input)
		if twice := Normalize(v.TraceID, once); !reflect.DeepEqual(once, twice) {
			t.Errorf("%s: a second pass changed the span:\n once %v\ntwice %v", v.Name, once, twice)
		}
	}
}

// Every rule adds a canonical key: a name package genai declares. A rule
// that set a convention's own name would be a rename, which this table must
// never do.
func TestEveryRuleSetsACanonicalKey(t *testing.T) {
	canonical := map[string]bool{
		genai.AttrConvention: true, genai.AttrOperationName: true, genai.AttrProviderName: true,
		genai.AttrRequestModel: true, genai.AttrResponseModel: true,
		genai.AttrUsageInputTokens: true, genai.AttrUsageOutputTokens: true,
		genai.AttrUsageCacheReadInputTokens: true, genai.AttrUsageCacheCreationInputTokens: true,
		genai.AttrUsageReasoningOutputTokens: true, genai.AttrCostUSD: true,
		genai.AttrAgentName: true, genai.AttrToolName: true, genai.AttrToolCallID: true,
		genai.AttrNodeID: true, genai.AttrWorkflowName: true, genai.AttrRunID: true,
		genai.AttrConversationID: true, genai.AttrUserID: true,
	}
	for i, r := range Rules() {
		if !canonical[r.Set] {
			t.Errorf("rule %d sets %q, which is not a canonical genai key", i, r.Set)
		}
		if (r.Copy == "") == (r.Value == "") {
			t.Errorf("rule %d (%s): exactly one of Copy and Value must be set", i, r.Set)
		}
		if r.Unverified && r.Note == "" {
			t.Errorf("rule %d (%s): an unverified rule says what is unverified in Note", i, r.Set)
		}
	}
}

// Every rule that reads a convention's generic keys is confined to spans of
// that convention, so a browser span's session.id is never a conversation.
func TestGenericKeysAreReadOnlyOnTheirConvention(t *testing.T) {
	generic := map[string]bool{"session.id": true, "agent.name": true, "tool.name": true,
		"tool_call.id": true, "graph.node.id": true}
	for i, r := range Rules() {
		if !generic[r.Copy] {
			continue
		}
		if !slices.ContainsFunc(r.When, func(c Cond) bool { return c.Key == genai.AttrConvention && c.Equals != "" }) {
			t.Errorf("rule %d copies the generic %q without a convention condition", i, r.Copy)
		}
	}
}

// The table is plain names and values: nothing a KCL, YAML or OTTL string
// would have to escape.
func TestTheTableNeedsNoEscaping(t *testing.T) {
	plain := regexp.MustCompile(`^[A-Za-z0-9_.:@$-]+$`)
	for i, r := range Rules() {
		for _, s := range []string{r.Set, r.Copy, r.Value} {
			if s != "" && !plain.MatchString(s) {
				t.Errorf("rule %d: %q needs escaping", i, s)
			}
		}
		for _, c := range r.When {
			for _, s := range []string{c.Key, c.Equals} {
				if s != "" && !plain.MatchString(s) {
					t.Errorf("rule %d: condition %q needs escaping", i, s)
				}
			}
		}
	}
}

// One statement per rule, each guarded so it only ever adds.
func TestOTTLStatements(t *testing.T) {
	stmts := OTTL()
	if len(stmts) != len(Rules()) {
		t.Fatalf("%d statements for %d rules", len(stmts), len(Rules()))
	}
	for i, s := range stmts {
		r := Rules()[i]
		guard := `attributes["` + r.Set + `"] == nil`
		if !strings.HasPrefix(s, `set(attributes["`+r.Set+`"], `) || !strings.Contains(s, " where "+guard) {
			t.Errorf("statement %d does not guard its own key: %s", i, s)
		}
		if strings.ContainsAny(s, `'\`) {
			t.Errorf("statement %d holds a quote or backslash: %s", i, s)
		}
	}
	want := `set(attributes["gen_ai.request.model"], attributes["llm.model_name"]) where attributes["gen_ai.request.model"] == nil and attributes["llm.model_name"] != nil and attributes["forge.ai.convention"] == "openinference"`
	if !slices.Contains(stmts, want) {
		t.Errorf("no statement\n%s", want)
	}
	want = `set(attributes["forge.ai.run.id"], trace_id.string) where attributes["forge.ai.run.id"] == nil and attributes["forge.ai.convention"] == "openllmetry" and attributes["traceloop.workflow.name"] != nil`
	if !slices.Contains(stmts, want) {
		t.Errorf("no statement\n%s", want)
	}
}

// kcl/genai_normalize_gen.k is OTTL() for the KCL module, so a collector
// config written in KCL (the hosted gateway's) is generated from this
// table. `go test ./pkg/observe/genai/conventions -run TestKCLStatements -update`
// rewrites it.
func TestKCLStatementsMatchTheTable(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "kcl", "genai_normalize_gen.k")
	want := kclFile()
	if *update {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s is stale: run go test ./pkg/observe/genai/conventions -run TestKCLStatements -update", path)
	}
}
