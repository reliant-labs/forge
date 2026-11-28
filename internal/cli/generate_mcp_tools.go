package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/generator"
)

// stepMCPTools emits pkg/middleware/mcp_tools_gen.go: the rpcs the project
// serves as MCP tools (codegen.MCPTools decides which), each with the JSON
// Schemas of its request and response projected by the pinned
// protoc-gen-connect-openapi in jsonschema format. It is written whenever
// procedures_gen.go is (the scaffolded providers.go references MCPTools); a
// project that serves no tool gets an empty table and runs no plugin.
//
// It reads DeclaredServices, as the open-procedure set does: the tool table
// is part of the API contract, whoever mounts the handler. `api.openapi` is
// independent: this step runs the plugin whether or not it is on.
func stepMCPTools(ctx *pipelineContext) error {
	tools, err := codegen.MCPTools(ctx.DeclaredServices)
	if err != nil {
		return err
	}
	var doc codegen.MCPSchemaDoc
	if len(tools) > 0 {
		if doc, err = projectMCPSchemas(ctx.ProjectDir, tools); err != nil {
			return err
		}
	}
	content, err := codegen.RenderMCPTools(tools, doc, ctx.ModulePath)
	if err != nil {
		return err
	}
	relPath := filepath.FromSlash(codegen.MCPToolsGenPath)
	if _, err := generator.WriteGeneratedFileTier1(ctx.ProjectDir, relPath, content, ctx.Checksums, true); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	fmt.Printf("  ✅ Generated %s (%d MCP tools)\n", relPath, len(tools))
	return nil
}

// mcpSchemaDocName is the merged jsonschema document the plugin writes.
const mcpSchemaDocName = "mcp.jsonschema.json"

// projectMCPSchemas runs protoc-gen-connect-openapi (format=jsonschema)
// over the proto files declaring the tools' services, into a temporary
// directory, and returns the merged document: every message, enum and
// well-known type those files reach. Nothing is left in the project.
func projectMCPSchemas(projectDir string, tools []codegen.MCPToolDef) (codegen.MCPSchemaDoc, error) {
	var doc codegen.MCPSchemaDoc
	files := map[string]bool{}
	for _, tl := range tools {
		f := tl.Service.ProtoFile
		if _, err := os.Stat(filepath.Join(projectDir, "proto", filepath.FromSlash(f))); err == nil {
			f = "proto/" + f
		}
		files[f] = true
	}
	paths := make([]string, 0, len(files))
	for f := range files {
		paths = append(paths, f)
	}
	sort.Strings(paths)

	pluginDir, err := resolveOpenAPIPlugin(context.Background())
	if err != nil {
		return doc, fmt.Errorf("MCP tool schemas: %w", err)
	}
	outDir, err := os.MkdirTemp("", "forge-mcp-schemas-")
	if err != nil {
		return doc, err
	}
	defer func() { _ = os.RemoveAll(outDir) }()

	tmplName := "buf.gen.mcp.yaml"
	tmplPath := filepath.Join(projectDir, tmplName)
	if err := atomicfile.WriteFile(tmplPath, []byte(mcpBufTemplate(outDir)), 0o644); err != nil {
		return doc, fmt.Errorf("write ephemeral MCP schema template: %w", err)
	}
	defer func() { _ = os.Remove(tmplPath) }()

	extra := []string{"--template", tmplName}
	for _, p := range paths {
		extra = append(extra, "--path", p)
	}
	cmd := exec.Command("buf", bufGenerateArgs(extra...)...)
	cmd.Dir = projectDir
	if err := withPinnedTools(cmd, projectDir); err != nil {
		return doc, err
	}
	if pluginDir != "" {
		cmd.Env = pathPrepended(cmd.Env, []string{pluginDir})
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return doc, fmt.Errorf("MCP tool schemas: buf generate: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(outDir, mcpSchemaDocName))
	if err != nil {
		return doc, fmt.Errorf("MCP tool schemas: buf generate wrote no %s: %w", mcpSchemaDocName, err)
	}
	return codegen.ParseMCPSchemaDoc(b)
}

// mcpBufTemplate wires protoc-gen-connect-openapi in jsonschema format, all
// files merged (`path=`) into one document under outDir.
func mcpBufTemplate(outDir string) string {
	return fmt.Sprintf(`version: v2
# Synthesized by forge generate for the MCP tool schemas and removed after
# the run; do not commit this file.
plugins:
  - local: %s
    out: %s
    strategy: all
    opt:
      - format=jsonschema
      - path=%s
`, openAPIPluginBinary, outDir, mcpSchemaDocName)
}
