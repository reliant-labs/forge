package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateFrontendHooks generates TypeScript React Query hook files for
// every Connect-driven service.
//
// Two output modes, picked by cfg.IsFrontendWorkspacesEnabled():
//
//   - workspaces=false (default): one file per service per frontend, at
//     frontends/<name>/src/hooks/<svc>-hooks.ts. Each file imports
//     connectClient from @/lib/connect and proto types from @/gen.
//     Snapshot-stable with projects scaffolded before the flag landed.
//
//   - workspaces=true: one file per service at packages/hooks/src/
//     generated/<svc>-hooks.ts (shared across all frontends). Each file
//     imports connectClient from ../transport and proto types from the
//     project's @<scope>/api workspace. The per-frontend hooks dir is
//     not touched in this mode — frontends consume the workspace
//     package instead.
func generateFrontendHooks(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string) error {
	if len(services) == 0 {
		return nil
	}

	tmplContent, err := templates.FrontendTemplates().Get("hooks.ts.tmpl")
	if err != nil {
		return fmt.Errorf("read hooks template: %w", err)
	}

	tmpl, err := template.New("hooks.ts.tmpl").Funcs(templates.FuncMap()).Parse(string(tmplContent))
	if err != nil {
		return fmt.Errorf("parse hooks template: %w", err)
	}

	if cfg.IsFrontendWorkspacesEnabled() {
		return generateFrontendHooksWorkspace(cfg, services, projectDir, tmpl)
	}

	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") && !strings.EqualFold(fe.Type, "react-native") && !strings.EqualFold(fe.Type, "vite-spa") {
			continue
		}

		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}

		hooksDir := filepath.Join(projectDir, feDir, "src", "hooks")
		if err := os.MkdirAll(hooksDir, 0o755); err != nil {
			return fmt.Errorf("create hooks directory: %w", err)
		}

		var hookFiles []hookFileEntry

		for _, svc := range services {
			data := codegen.ServiceDefToHookData(svc)
			if len(data.Methods) == 0 {
				continue
			}

			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err != nil {
				return fmt.Errorf("render hooks for %s: %w", svc.Name, err)
			}

			fileName := serviceNameToHookFile(svc.Name)
			hookFiles = append(hookFiles, hookFileEntry{
				fileName: fileName,
				// nsAlias is the namespace we re-export the file as when a
				// collision forces namespacing. Derived from the service
				// name so it stays stable and readable: UserService ->
				// userService.
				nsAlias: codegen.ToCamelCaseFromPascalExport(svc.Name),
				symbols: hookFileExportedSymbols(data),
			})

			outPath := filepath.Join(hooksDir, fileName)
			if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
				return fmt.Errorf("write hooks file %s: %w", outPath, err)
			}
		}

		// Generate barrel index.ts
		if len(hookFiles) > 0 {
			indexPath := filepath.Join(hooksDir, "index.ts")
			if err := writeHooksIndex(indexPath, hookFiles); err != nil {
				return fmt.Errorf("write hooks index: %w", err)
			}
		}

		fmt.Printf("  ✅ Generated %d hook file(s) for frontend %s\n", len(hookFiles), fe.Name)
	}

	return nil
}

// hookFileEntry describes one generated hook file for index.ts generation.
type hookFileEntry struct {
	fileName string   // "user-service-hooks.ts"
	nsAlias  string   // "userService" — used when collisions force namespace re-exports
	symbols  []string // exported identifiers: ["useGetUser", "useCreateUser", ...]
}

// hookFileExportedSymbols returns the names of identifiers a rendered
// hooks.ts.tmpl file exposes (one `useXxx` per unary RPC). Keeping this in
// sync with the template is intentional: the template is the only thing
// that decides what gets exported, so we read its output shape here
// rather than re-parsing the rendered .ts.
func hookFileExportedSymbols(data codegen.FrontendHookTemplateData) []string {
	out := make([]string, 0, len(data.Methods))
	for _, m := range data.Methods {
		out = append(out, "use"+m.Name)
	}
	return out
}

// generateFrontendHooksWorkspace emits the workspace-mode hooks: one
// file per service at packages/hooks/src/generated/<svc>-hooks.ts,
// plus a barrel index.ts. Shared by every frontend in the project.
func generateFrontendHooksWorkspace(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string, tmpl *template.Template) error {
	layout := generator.NewFrontendWorkspaceLayout(cfg.Name)
	generatedDir := filepath.Join(projectDir, "packages", "hooks", "src", "generated")
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		return fmt.Errorf("create packages/hooks/src/generated: %w", err)
	}

	var hookFiles []hookFileEntry
	for _, svc := range services {
		data := codegen.ServiceDefToHookData(svc)
		if len(data.Methods) == 0 {
			continue
		}
		data.Workspaces = true
		data.ApiPackage = layout.ApiPackage

		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return fmt.Errorf("render workspace hooks for %s: %w", svc.Name, err)
		}

		fileName := serviceNameToHookFile(svc.Name)
		hookFiles = append(hookFiles, hookFileEntry{
			fileName: fileName,
			nsAlias:  codegen.ToCamelCaseFromPascalExport(svc.Name),
			symbols:  hookFileExportedSymbols(data),
		})

		outPath := filepath.Join(generatedDir, fileName)
		if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write hooks file %s: %w", outPath, err)
		}
	}

	if len(hookFiles) > 0 {
		indexPath := filepath.Join(generatedDir, "index.ts")
		if err := writeHooksIndex(indexPath, hookFiles); err != nil {
			return fmt.Errorf("write hooks index: %w", err)
		}
	}

	fmt.Printf("  ✅ Generated %d hook file(s) at packages/hooks/src/generated\n", len(hookFiles))
	return nil
}

// serviceNameToHookFile converts a service name to a hook file name.
// "UserService" → "user-service-hooks.ts", "EchoService" → "echo-service-hooks.ts",
// "LLMGatewayService" → "llm-gateway-service-hooks.ts" (NOT
// "l-l-m-gateway-service-hooks.ts" — see naming.ToKebabCase for the
// initialism table that keeps acronyms glued).
//
// All file-emitter sites and the re-export indexer below MUST go through
// this single function. If a future caller needs kebab-casing for any
// service-name-derived path, route, or import specifier, use
// `naming.ToKebabCase` directly rather than re-implementing the rule —
// the index.ts re-export stays in lockstep with the on-disk filenames
// only because both go through the same canonical splitter.
func serviceNameToHookFile(name string) string {
	return naming.ToKebabCase(name) + "-hooks.ts"
}

// writeHooksIndex writes a barrel index.ts that re-exports all hook files.
//
// Two emission modes:
//
//   - Flat wildcard re-exports (`export * from "./users-hooks"`) when no
//     two hook files would re-export the same identifier. This is the
//     historic shape and keeps `import { useGetUser } from "@/hooks"`
//     working.
//   - Namespaced re-exports (`export * as userService from "./users-hooks"`)
//     when at least one identifier (e.g. a generic `useList` on two
//     services) is exported by multiple files. In flat mode TypeScript
//     rejects the duplicate `export *`; switching the ENTIRE barrel to
//     namespaces is simpler — and more predictable for consumers — than
//     mixing flat + namespace per file. Callers update to
//     `import { userService } from "@/hooks"; userService.useList(...)`.
//
// The chosen mode is recorded in a comment at the top of index.ts so a
// reader of generated output understands why their imports changed.
func writeHooksIndex(path string, hookFiles []hookFileEntry) error {
	collisions := detectIndexCollisions(hookFiles)

	var buf bytes.Buffer
	buf.WriteString("// AUTO-GENERATED — DO NOT EDIT\n")
	buf.WriteString("// Generated by forge generate\n")
	if len(collisions) > 0 {
		buf.WriteString("//\n")
		buf.WriteString("// Mode: namespaced re-exports.\n")
		buf.WriteString("// Reason: two or more hook files export the same identifier(s):\n")
		for _, c := range collisions {
			buf.WriteString(fmt.Sprintf("//   - %s (from %s)\n", c.symbol, strings.Join(c.files, ", ")))
		}
		buf.WriteString("// To call a hook, namespace it: `import { <alias> } from \"@/hooks\"; <alias>.useFoo(...)`.\n\n")
		for _, f := range hookFiles {
			module := strings.TrimSuffix(f.fileName, ".ts")
			buf.WriteString(fmt.Sprintf("export * as %s from \"./%s\";\n", f.nsAlias, module))
		}
	} else {
		buf.WriteString("// Mode: flat wildcard re-exports (no symbol collisions).\n\n")
		for _, f := range hookFiles {
			module := strings.TrimSuffix(f.fileName, ".ts")
			buf.WriteString(fmt.Sprintf("export * from \"./%s\";\n", module))
		}
	}

	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// indexCollision records one symbol that is exported by 2+ hook files.
type indexCollision struct {
	symbol string
	files  []string // sorted, deterministic
}

// detectIndexCollisions returns every exported identifier that appears in
// more than one hook file. An empty result means the flat wildcard barrel
// will compile. We surface ALL of them — not just the first — so a single
// regen tells the user about every conflict instead of leaving them to
// fix collisions one-by-one across cycles.
func detectIndexCollisions(hookFiles []hookFileEntry) []indexCollision {
	bySymbol := map[string]map[string]struct{}{}
	for _, f := range hookFiles {
		for _, s := range f.symbols {
			set, ok := bySymbol[s]
			if !ok {
				set = map[string]struct{}{}
				bySymbol[s] = set
			}
			set[f.fileName] = struct{}{}
		}
	}

	var out []indexCollision
	for sym, files := range bySymbol {
		if len(files) < 2 {
			continue
		}
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		out = append(out, indexCollision{symbol: sym, files: names})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].symbol < out[j].symbol })
	return out
}
