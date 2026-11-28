package codegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// MCPToolsGenPath is the generated tool table, relative to the project root.
const MCPToolsGenPath = "pkg/middleware/mcp_tools_gen.go"

// MCPSchemaDoc is protoc-gen-connect-openapi's `format=jsonschema` document:
// one JSON Schema (draft 2020-12) per message, enum and well-known type the
// projected protos reach, under $defs keyed by fully-qualified name, each
// referencing the others as "#/$defs/<name>".
type MCPSchemaDoc struct {
	Defs map[string]json.RawMessage `json:"$defs"`
}

// ParseMCPSchemaDoc decodes the plugin's jsonschema document.
func ParseMCPSchemaDoc(b []byte) (MCPSchemaDoc, error) {
	var d MCPSchemaDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("parse jsonschema document: %w", err)
	}
	return d, nil
}

const mcpDefsRef = "#/$defs/"

// Bundle returns the schema of message fq as a self-contained JSON Schema:
// the message's own schema at the root, with every definition it reaches
// (transitively, itself included when it recurses) under the root's own
// $defs, so the plugin's "#/$defs/<name>" references resolve inside it.
// The root must be an object schema, as MCP requires of a tool's input and
// output. The output is compact JSON with sorted keys, so a regenerate is
// byte-identical.
func (d MCPSchemaDoc) Bundle(fq string) (json.RawMessage, error) {
	decode := func(name string) (map[string]any, error) {
		raw, ok := d.Defs[name]
		if !ok {
			return nil, fmt.Errorf("jsonschema document has no definition of %s", name)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("decode the jsonschema definition of %s: %w", name, err)
		}
		return v, nil
	}
	root, err := decode(fq)
	if err != nil {
		return nil, err
	}
	if root["type"] != "object" {
		return nil, fmt.Errorf("%s projects to a JSON Schema of type %v, but an MCP tool schema must be an object", fq, root["type"])
	}
	defs := map[string]any{}
	queue := mcpRefs(root, nil)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, done := defs[name]; done {
			continue
		}
		def, err := decode(name)
		if err != nil {
			return nil, err
		}
		defs[name] = def
		queue = mcpRefs(def, queue)
	}
	if len(defs) > 0 {
		root["$defs"] = defs
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes())), nil
}

// mcpRefs appends the definition names every "#/$defs/<name>" $ref under v
// names. A reference anywhere else (another file) is not followed; the
// plugin writes none.
func mcpRefs(v any, out []string) []string {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok && strings.HasPrefix(ref, mcpDefsRef) {
			out = append(out, strings.TrimPrefix(ref, mcpDefsRef))
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = mcpRefs(t[k], out)
		}
	case []any:
		for _, e := range t {
			out = mcpRefs(e, out)
		}
	}
	return out
}

// mcpToolEntry is one rendered row of the tool table; the string fields are
// Go expressions.
type mcpToolEntry struct {
	Name, Const, Description, InputSchema, OutputSchema string
	ReadOnly, Destructive, Idempotent, Public           bool
}

type mcpToolsTemplateData struct {
	Imports []OpenProcedureImport
	Tools   []mcpToolEntry
}

// RenderMCPTools renders pkg/middleware/mcp_tools_gen.go from the tools
// MCPTools decided and the plugin's schema document. No tools renders an
// empty table: the scaffolded providers.go references MCPTools whether or
// not the project serves any, so a project that opts in later gets /mcp.
func RenderMCPTools(tools []MCPToolDef, doc MCPSchemaDoc, modulePath string) ([]byte, error) {
	var data mcpToolsTemplateData
	seen := map[string]string{} // path -> alias
	for _, tl := range tools {
		alias, path := connectPackageFor(tl.Service, modulePath)
		if path == "" {
			return nil, fmt.Errorf("MCP tool %s: cannot resolve the connect package of %s", tl.Name, tl.Service.Name)
		}
		if a, ok := seen[path]; ok {
			alias = a
		} else {
			seen[path] = alias
			data.Imports = append(data.Imports, OpenProcedureImport{Alias: alias, Path: path})
		}
		in, err := doc.Bundle(mcpTypeFQ(tl.Service, tl.Method.InputTypeFQ, tl.Method.InputType))
		if err != nil {
			return nil, fmt.Errorf("MCP tool %s input schema: %w", tl.Name, err)
		}
		out, err := doc.Bundle(mcpTypeFQ(tl.Service, tl.Method.OutputTypeFQ, tl.Method.OutputType))
		if err != nil {
			return nil, fmt.Errorf("MCP tool %s output schema: %w", tl.Name, err)
		}
		data.Tools = append(data.Tools, mcpToolEntry{
			Name:         tl.Name,
			Const:        alias + "." + naming.ConnectProcedureConst(tl.Service.Name, tl.Method.Name),
			Description:  strconv.Quote(tl.Description),
			InputSchema:  goStringLiteral(string(in)),
			OutputSchema: goStringLiteral(string(out)),
			ReadOnly:     tl.ReadOnly,
			Destructive:  tl.Destructive,
			Idempotent:   tl.Idempotent,
			Public:       tl.Public,
		})
	}
	sort.Slice(data.Imports, func(i, j int) bool { return data.Imports[i].Path < data.Imports[j].Path })
	content, err := templates.ProjectTemplates().Render("middleware-mcp-tools.go.tmpl", data)
	if err != nil {
		return nil, fmt.Errorf("render middleware-mcp-tools.go.tmpl: %w", err)
	}
	formatted, err := format.Source(content)
	if err != nil {
		return nil, fmt.Errorf("format %s: %w", MCPToolsGenPath, err)
	}
	return formatted, nil
}

// mcpTypeFQ is a method message's fully-qualified name, from a descriptor
// older than InputTypeFQ/OutputTypeFQ by the service's package.
func mcpTypeFQ(svc ServiceDef, fq, short string) string {
	if fq != "" {
		return fq
	}
	return svc.Package + "." + short
}
