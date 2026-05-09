// Package tools enumerates the MCP tool surface that forge exposes to LLM
// clients: a small set of database, project-context, and Taskfile helpers.
//
// Each tool has a JSON-Schema descriptor (see [Tool]) and an executor.
// The package wraps both behind one [Service] so tests can stub the
// catalog (List) or the dispatcher (Execute) without spinning up the
// whole MCP server.
//
// Data carriers ([Tool]) remain plain types — they describe a tool, they
// do not behave.
package tools

import "encoding/json"

// Service is the behavioral surface of the MCP tools package.
type Service interface {
	// List returns all registered MCP tool descriptors. Order is stable.
	List() []Tool

	// Execute dispatches a tool call by name, returning the formatted
	// string output (or an error if the tool is unknown / fails).
	Execute(name string, arguments json.RawMessage) (string, error)
}

// Deps is the dependency set for the MCP tools Service. Empty today —
// each tool reaches out to its own backend (the connection-manager
// singleton in mcp/database, the project filesystem, etc.) directly.
type Deps struct{}

// New constructs a tools.Service.
func New(_ Deps) Service { return &svc{} }

type svc struct{}

// List delegates to the package-level catalog.
func (s *svc) List() []Tool { return allTools() }

// Execute delegates to the package-level dispatcher.
func (s *svc) Execute(name string, arguments json.RawMessage) (string, error) {
	return executeTool(name, arguments)
}
