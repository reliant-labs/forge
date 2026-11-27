package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func listMCPTools(t *testing.T, cs *mcp.ClientSession) []*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Tools
}

func mcpToolNames(tools []*mcp.Tool) []string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// Each group registers exactly the tools its table row names, and the groups
// together are every tool, each once: forge_tools' description and --help are
// built from those rows.
func TestMCPToolGroupsRegisterExactlyTheirTools(t *testing.T) {
	seen := map[string]string{}
	for _, g := range mcpToolGroups {
		cs, _ := connectMCPGroups(t, t.TempDir(), nil, []string{g.name})
		want := sortedCopy(append(slices.Clone(g.tools), "forge_tools"))
		if got := mcpToolNames(listMCPTools(t, cs)); !reflect.DeepEqual(got, want) {
			t.Errorf("--tools %s lists %v, want %v", g.name, got, want)
		}
		for _, name := range g.tools {
			if other, dup := seen[name]; dup {
				t.Errorf("%s is in both %s and %s", name, other, g.name)
			}
			seen[name] = g.name
		}
	}
	if len(seen) != 24 {
		t.Errorf("the groups hold %d tools, want all 24", len(seen))
	}
}

func TestParseMCPTools(t *testing.T) {
	all := mcpToolGroupNames()
	for _, tc := range []struct {
		spec string
		want []string
	}{
		{"all", all},
		{"core", []string{"core"}},
		{"ops, CORE", []string{"core", "ops"}},
		{"core,all", all},
		{"core,core", []string{"core"}},
	} {
		got, err := parseMCPTools(tc.spec)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseMCPTools(%q) = %v, %v; want %v", tc.spec, got, err, tc.want)
		}
	}
	for _, spec := range []string{"", "bogus", "core,,ops", "core,bogus"} {
		_, err := parseMCPTools(spec)
		if err == nil {
			t.Errorf("parseMCPTools(%q) accepted it", spec)
			continue
		}
		for _, want := range []string{"unknown group", "want all", "core, catalog, support, ops, deploy"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("parseMCPTools(%q) error %q does not say %q", spec, err, want)
			}
		}
	}
}

// An unknown --tools value fails the command before it serves anything.
func TestMCPCmdRejectsAnUnknownToolsValue(t *testing.T) {
	cmd := newMCPCmd()
	cmd.SetArgs([]string{"--tools", "everything"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `unknown group "everything"`) {
		t.Fatalf("err = %v, want the unknown group named", err)
	}
	if f := cmd.Flags().Lookup("tools"); f == nil || f.DefValue != "all" {
		t.Errorf("--tools default = %v, want all", f)
	}
}

// --tools core lists the core group and forge_tools, says where the rest
// are, and forge_tools adds a group mid-session with tools/list_changed.
func TestMCP_CoreToolsetEnablesTheRestInSession(t *testing.T) {
	changed := make(chan struct{}, 8)
	cs, _ := connectMCPGroups(t, t.TempDir(), &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed <- struct{}{} },
	}, []string{"core"})

	want := []string{"audit", "forge_tools", "generate", "lint", "shapes", "skill_load", "skill_search"}
	if got := mcpToolNames(listMCPTools(t, cs)); !reflect.DeepEqual(got, want) {
		t.Fatalf("core lists %v, want %v", got, want)
	}
	for _, want := range []string{"Reliant Forge", "Atlassian Forge", "--tools core", "name=forge", "name=adopt", "forge_tools", "--tools all"} {
		if !strings.Contains(cs.InitializeResult().Instructions, want) {
			t.Errorf("core instructions do not mention %q", want)
		}
	}

	out, isErr := callMCP(t, cs, "forge_tools", nil)
	if isErr || !strings.Contains(out, "ops ") || !strings.Contains(out, "obs_status") {
		t.Errorf("forge_tools with no input should list the groups:\n%s", out)
	}
	if out, isErr := callMCP(t, cs, "forge_tools", map[string]any{"enable": []string{"bogus"}}); !isErr || !strings.Contains(out, "unknown group") {
		t.Errorf("enabling an unknown group = %q (error %v), want a refusal", out, isErr)
	}

	out, isErr = callMCP(t, cs, "forge_tools", map[string]any{"enable": []string{"ops"}})
	if isErr || !strings.Contains(out, "Added blast_radius") {
		t.Fatalf("enable ops = %q (error %v)", out, isErr)
	}
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("no notifications/tools/list_changed after enabling a group")
	}
	got := mcpToolNames(listMCPTools(t, cs))
	for _, name := range []string{"obs_status", "db_query", "shapes"} {
		if !slices.Contains(got, name) {
			t.Errorf("after enabling ops the list %v lacks %s", got, name)
		}
	}
	if out, _ := callMCP(t, cs, "forge_tools", map[string]any{"enable": []string{"ops"}}); !strings.Contains(out, "Already enabled") {
		t.Errorf("enabling ops twice = %q, want Already enabled", out)
	}
}

// The default lists every tool and no forge_tools, with the instructions
// unchanged.
func TestMCP_AllToolsetHasNoForgeTools(t *testing.T) {
	cs := connectMCP(t, t.TempDir())
	got := mcpToolNames(listMCPTools(t, cs))
	if len(got) != 24 || slices.Contains(got, "forge_tools") {
		t.Errorf("the default lists %d tools %v, want the 24 without forge_tools", len(got), got)
	}
	if cs.InitializeResult().Instructions != mcpInstructions {
		t.Error("the default's instructions changed")
	}
}

// The lint tool steers iteration to a scoped lint and one full lint at the
// end: a full lint walks every frontend and costs about a minute.
func TestMCP_LintDescriptionSaysIterateScoped(t *testing.T) {
	cs, _ := connectMCPGroups(t, t.TempDir(), nil, []string{"core"})
	for _, tool := range listMCPTools(t, cs) {
		if tool.Name != "lint" {
			continue
		}
		for _, want := range []string{"While iterating, pass scope", "full unscoped lint once before you finish"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("lint description lacks %q: %s", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("no lint tool")
}

// A client resends tools/list on every model call, so its size is a cost
// every agent pays per turn. These budgets keep it from regrowing unnoticed:
// raising one is a deliberate choice, made with the per-tool sizes in hand
// (the failure lists them).
func TestMCPToolListByteBudget(t *testing.T) {
	for _, tc := range []struct {
		groups []string
		budget int
	}{
		{[]string{"core"}, 4900},
		{nil, 19500},
	} {
		cs, _ := connectMCPGroups(t, t.TempDir(), nil, tc.groups)
		tools := listMCPTools(t, cs)
		b, err := json.Marshal(map[string]any{"tools": tools})
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > tc.budget {
			var sizes []string
			for _, tool := range tools {
				tb, _ := json.Marshal(tool)
				sizes = append(sizes, fmt.Sprintf("%s=%d", tool.Name, len(tb)))
			}
			t.Errorf("tools/list with groups %v is %d bytes (~%d tokens), over its %d-byte budget: %s",
				tc.groups, len(b), len(b)/4, tc.budget, strings.Join(sizes, " "))
		}
	}
}
