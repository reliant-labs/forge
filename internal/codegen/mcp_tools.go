package codegen

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/naming"
)

// MCP tools from rpcs (docs/proposals/mcp-from-rpcs.md).
//
// An rpc opts in with `(forge.v1.method).mcp` or its service's
// `(forge.v1.service).mcp`; a mutating rpc (activity.IsMutating) is served
// only with `mcp_mutating: true` as well. MCPTools is the one place that
// decides the served set and each tool's annotations: the generated tool
// table, the lint lane and `forge mcp app` all read its answer.

// MCPToolDef is one rpc served as an MCP tool.
type MCPToolDef struct {
	// Name is `<service>_<rpc>` in snake case, the service's `Service`
	// suffix dropped: "orders_list_orders".
	Name string
	// Service is the ServiceDef the rpc belongs to; Method is the rpc.
	Service ServiceDef
	Method  Method
	// Description is the rpc's leading proto comment.
	Description string
	// ReadOnly: the rpc declares idempotency_level = NO_SIDE_EFFECTS.
	ReadOnly bool
	// Destructive: a mutating rpc served with mcp_mutating: true.
	Destructive bool
	// Idempotent: (forge.v1.method).idempotent or IDEMPOTENT.
	Idempotent bool
	// Public: the rpc declares auth_required: false.
	Public bool
}

// MCPToolNameMax is the longest tool name current MCP clients accept.
const MCPToolNameMax = 64

var mcpToolNameRE = regexp.MustCompile(`^[a-z0-9_]+$`)

// MCPToolName is the tool name of rpc method on service svc.
func MCPToolName(svc, method string) string {
	return naming.ToSnakeCase(strings.TrimSuffix(svc, "Service")) + "_" + naming.ToSnakeCase(method)
}

// MCPTools decides the MCP tools the services serve, in service then
// declaration order. It refuses (returns an error naming every defect) what
// cannot be served: an explicit `(forge.v1.method).mcp: true` on a streaming
// rpc (one inheriting the service default is skipped), a tool name that is not
// [a-z0-9_]{1,64}, and two rpcs projecting to one name. A mutating rpc
// without mcp_mutating is not an error here, only absent: lint says why.
func MCPTools(services []ServiceDef) ([]MCPToolDef, error) {
	var tools []MCPToolDef
	var errs []string
	byName := map[string]string{}
	for _, svc := range services {
		for _, m := range svc.Methods {
			if m.MCP == nil || !m.MCP.OptIn {
				continue
			}
			where := fmt.Sprintf("%s.%s.%s", svc.Package, svc.Name, m.Name)
			if m.ClientStreaming || m.ServerStreaming {
				// The service default covers unary rpcs only.
				if !m.MCP.Explicit {
					continue
				}
				errs = append(errs, fmt.Sprintf("%s: mcp is set on a streaming rpc; MCP tools are request/response, so set (forge.v1.method).mcp = false on it", where))
				continue
			}
			if m.MCP.Mutating && !m.MCP.MutatingAck {
				continue
			}
			name := MCPToolName(svc.Name, m.Name)
			if len(name) > MCPToolNameMax || !mcpToolNameRE.MatchString(name) {
				errs = append(errs, fmt.Sprintf("%s: MCP tool name %q is not 1-%d characters of [a-z0-9_]", where, name, MCPToolNameMax))
				continue
			}
			if prev, ok := byName[name]; ok {
				errs = append(errs, fmt.Sprintf("%s: MCP tool name %q is already %s's; rename one rpc or opt one out with mcp = false", where, name, prev))
				continue
			}
			byName[name] = where
			tools = append(tools, MCPToolDef{
				Name:        name,
				Service:     svc,
				Method:      m,
				Description: m.Description,
				ReadOnly:    m.NoSideEffects,
				Destructive: m.MCP.Mutating,
				Idempotent:  m.MCP.Idempotent,
				Public:      !m.AuthRequired,
			})
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("MCP tools cannot be served:\n  %s", strings.Join(errs, "\n  "))
	}
	return tools, nil
}
