package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	"github.com/jinzhu/inflection"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateFrontendMocks generates TypeScript mock data files and a mock
// transport for each Next.js / Vite frontend. Mock data files contain typed
// arrays derived from entity definitions using the same deterministic values
// as db/fixtures. The mock transport intercepts ConnectRPC calls and returns
// fixture data, enabled via NEXT_PUBLIC_MOCK_API=true / VITE_MOCK_API=true.
//
// In addition to the per-entity mocks, every frontend gets the scenarios
// scaffolding under src/mocks/scenarios/ — the contract type,
// `default.ts` (seeded once, hand-editable), and a regenerated
// `index.ts` registry. See docs/adr/0002-frontend-scenarios.md.
//
// `connect.ts` unconditionally references `@/lib/mock-transport` (it
// dynamic-`require`s the path under NEXT_PUBLIC_MOCK_API=true), so we
// must always emit a real, scenario-capable mock-transport for every
// frontend — even when there are no CRUD entities. Without the file,
// webpack's static-analysis fails the production build with "Module not
// found: Can't resolve '@/lib/mock-transport'".
//
// Scenario support is DECOUPLED from entity-CRUD: the transport's
// scenario-dispatch + hybrid-passthrough + Unimplemented-fallback
// pipeline depends only on the scenarios scaffolding (always emitted),
// not on entity fixtures. The per-entity fixture switch is the ONLY part
// gated on entities — when the project has none, the template simply
// omits that section, leaving a fully scenario-capable transport (never
// a do-nothing stub that would silently drop the scenario mechanism).
func generateFrontendMocks(cfg *config.ProjectConfig, services []codegen.ServiceDef, entities []codegen.EntityDef, projectDir string) error {
	// Load templates
	mockDataTmplContent, err := templates.FrontendTemplates().Get(filepath.Join("mocks", "mock-data.ts.tmpl"))
	if err != nil {
		return fmt.Errorf("read mock-data template: %w", err)
	}
	mockDataTmpl, err := template.New("mock-data.ts.tmpl").Funcs(templates.FuncMap()).Parse(string(mockDataTmplContent))
	if err != nil {
		return fmt.Errorf("parse mock-data template: %w", err)
	}

	mockTransportTmplContent, err := templates.FrontendTemplates().Get(filepath.Join("mocks", "mock-transport.ts.tmpl"))
	if err != nil {
		return fmt.Errorf("read mock-transport template: %w", err)
	}
	mockTransportTmpl, err := template.New("mock-transport.ts.tmpl").Funcs(templates.FuncMap()).Parse(string(mockTransportTmplContent))
	if err != nil {
		return fmt.Errorf("parse mock-transport template: %w", err)
	}

	// Build entity→service mapping: match entity names to the service that owns them
	entityToService := buildEntityServiceMap(services, entities)

	// Build mock transport entities for the transport template
	transportEntities := codegen.ExtractMockTransportEntities(services, entities)

	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") && !strings.EqualFold(fe.Type, "vite-spa") {
			continue
		}

		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}

		mocksDir := filepath.Join(projectDir, feDir, "src", "mocks")
		if err := os.MkdirAll(mocksDir, 0o755); err != nil {
			return fmt.Errorf("create mocks directory: %w", err)
		}

		// Always emit the scenarios scaffolding (types + default + registry)
		// before the per-entity mocks. The registry walks src/mocks/scenarios/
		// for *.ts files and re-exports them; rerunning is idempotent.
		if err := emitScenarioScaffolding(mocksDir, services); err != nil {
			return fmt.Errorf("emit scenario scaffolding: %w", err)
		}

		var mockFileCount int
		var generatedSlugs []string

		// Generate per-entity mock data files
		for _, entity := range entities {
			svc, ok := entityToService[entity.Name]
			if !ok {
				continue
			}

			data := codegen.EntityDefToMockData(entity, svc)

			var buf bytes.Buffer
			if err := mockDataTmpl.Execute(&buf, data); err != nil {
				return fmt.Errorf("render mock data for %s: %w", entity.Name, err)
			}

			slug := codegen.PascalToKebab(inflection.Plural(entity.Name))
			outPath := filepath.Join(mocksDir, slug+".ts")
			if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
				return fmt.Errorf("write mock data file %s: %w", outPath, err)
			}
			generatedSlugs = append(generatedSlugs, slug)
			mockFileCount++
		}

		// Generate mock transport.
		//
		// The rich template is ALWAYS used — scenario dispatch is decoupled
		// from entity-CRUD. When the project has CRUD entities it also
		// produces the per-entity fixture dispatch table; when it has none
		// (no entities at all, or services that carry only bespoke non-CRUD
		// methods), the template omits the fixture switch but still emits
		// the full scenario-dispatch + hybrid-passthrough +
		// Unimplemented-fallback pipeline. connect.ts's static import
		// resolves either way, and the scenario mechanism is never dropped.
		libDir := filepath.Join(projectDir, feDir, "src", "lib")
		if err := os.MkdirAll(libDir, 0o755); err != nil {
			return fmt.Errorf("create lib directory: %w", err)
		}
		outPath := filepath.Join(libDir, "mock-transport.ts")

		transportData := codegen.MockTransportTemplateData{
			Entities:           transportEntities,
			SchemaImportGroups: codegen.BuildMockTransportSchemaImportGroups(transportEntities),
		}

		var buf bytes.Buffer
		if err := mockTransportTmpl.Execute(&buf, transportData); err != nil {
			return fmt.Errorf("render mock transport: %w", err)
		}

		if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write mock transport: %w", err)
		}

		// Generate barrel index.ts for mocks
		if mockFileCount > 0 {
			if err := writeMocksIndex(mocksDir, generatedSlugs); err != nil {
				return fmt.Errorf("write mocks index: %w", err)
			}
		}

		fmt.Printf("  ✅ Generated %d mock data file(s) + transport for frontend %s\n", mockFileCount, fe.Name)
	}

	return nil
}

// buildEntityServiceMap creates a mapping from entity name to the service
// that owns the CRUD RPCs for that entity.
func buildEntityServiceMap(services []codegen.ServiceDef, entities []codegen.EntityDef) map[string]codegen.ServiceDef {
	result := make(map[string]codegen.ServiceDef, len(entities))
	for _, svc := range services {
		pages := codegen.ExtractCRUDEntities(svc)
		for _, page := range pages {
			for _, entity := range entities {
				if entity.Name == page.EntityName {
					result[entity.Name] = svc
				}
			}
		}
	}
	return result
}

// writeMocksIndex writes a barrel index.ts that re-exports all mock data files.
func writeMocksIndex(mocksDir string, slugs []string) error {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by forge. DO NOT EDIT.\n")
	buf.WriteString("// forge-owned: regenerated every run — do not edit (forge disown to take ownership)\n\n")

	for _, slug := range slugs {
		buf.WriteString(fmt.Sprintf("export * from \"./%s\";\n", slug))
	}

	return os.WriteFile(filepath.Join(mocksDir, "index.ts"), buf.Bytes(), 0o644)
}

// emitScenarioScaffolding writes the scenarios files into
// <mocksDir>/scenarios/:
//
//   - scenario-types.ts (always regenerated — the contract type)
//   - scenario-rpcs_gen.ts (always regenerated — typed per-RPC handler map
//     derived from the proto descriptor; empty-but-valid when the project
//     has no services yet)
//   - default.ts (seeded once; safe to hand-edit)
//   - index.ts (regenerated by walking the directory for *.ts files)
//
// scenario-types.ts and scenario-rpcs_gen.ts are emitted one level up at
// <mocksDir>/ so user scenarios can `import { defineScenario } from
// "../scenario-types"`. The regeneration is idempotent: running
// `forge generate` twice produces the same output.
//
// User-authored scenarios (created with `forge add scenario`) are NEVER
// touched here — the registry just re-exports them.
func emitScenarioScaffolding(mocksDir string, services []codegen.ServiceDef) error {
	scenariosDir := filepath.Join(mocksDir, "scenarios")
	if err := os.MkdirAll(scenariosDir, 0o755); err != nil {
		return fmt.Errorf("create scenarios directory: %w", err)
	}

	// 1) scenario-types.ts (always written — generator-owned).
	typesContent, err := templates.FrontendTemplates().Get(filepath.Join("mocks", "scenarios", "scenario-types.ts.tmpl"))
	if err != nil {
		return fmt.Errorf("read scenario-types template: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mocksDir, "scenario-types.ts"), typesContent, 0o644); err != nil {
		return fmt.Errorf("write scenario-types.ts: %w", err)
	}

	// 1b) scenario-rpcs_gen.ts (always written — generator-owned). The
	// typed handler map every Scenario.handlers literal checks against.
	rpcsTmplContent, err := templates.FrontendTemplates().Get(filepath.Join("mocks", "scenarios", "scenario-rpcs.ts.tmpl"))
	if err != nil {
		return fmt.Errorf("read scenario-rpcs template: %w", err)
	}
	rpcsTmpl, err := template.New("scenario-rpcs.ts.tmpl").Funcs(templates.FuncMap()).Parse(string(rpcsTmplContent))
	if err != nil {
		return fmt.Errorf("parse scenario-rpcs template: %w", err)
	}
	var rpcsBuf bytes.Buffer
	if err := rpcsTmpl.Execute(&rpcsBuf, codegen.BuildScenarioRpcData(services)); err != nil {
		return fmt.Errorf("render scenario-rpcs_gen: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mocksDir, "scenario-rpcs_gen.ts"), rpcsBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write scenario-rpcs_gen.ts: %w", err)
	}

	// 2) default.ts — seeded once, hand-editable. Skip if it already exists.
	defaultPath := filepath.Join(scenariosDir, "default.ts")
	if _, statErr := os.Stat(defaultPath); os.IsNotExist(statErr) {
		defaultContent, err := templates.FrontendTemplates().Get(filepath.Join("mocks", "scenarios", "default-scenario.ts.tmpl"))
		if err != nil {
			return fmt.Errorf("read default-scenario template: %w", err)
		}
		if err := os.WriteFile(defaultPath, defaultContent, 0o644); err != nil {
			return fmt.Errorf("write default.ts: %w", err)
		}
	}

	// 3) index.ts — regenerated by walking the directory.
	if err := writeScenariosIndex(scenariosDir); err != nil {
		return fmt.Errorf("write scenarios index: %w", err)
	}

	return nil
}

// scenarioIndexEntry is the per-file shape consumed by
// scenarios-index.ts.tmpl. Slug is the filename without the `.ts`
// extension; Ident is a valid TS identifier derived from the slug
// (kebab-case → camelCase, suffixed with "Scenario").
type scenarioIndexEntry struct {
	Slug  string
	Ident string
}

// writeScenariosIndex walks <scenariosDir> for *.ts files (excluding
// index.ts itself) and renders scenarios-index.ts.tmpl with one entry
// per file. Ordering is alphabetical for deterministic output —
// re-running `forge generate` on the same set of files produces the
// same bytes.
func writeScenariosIndex(scenariosDir string) error {
	tmplContent, err := templates.FrontendTemplates().Get(filepath.Join("mocks", "scenarios", "scenarios-index.ts.tmpl"))
	if err != nil {
		return fmt.Errorf("read scenarios-index template: %w", err)
	}
	tmpl, err := template.New("scenarios-index.ts.tmpl").Funcs(templates.FuncMap()).Parse(string(tmplContent))
	if err != nil {
		return fmt.Errorf("parse scenarios-index template: %w", err)
	}

	entries, err := os.ReadDir(scenariosDir)
	if err != nil {
		return fmt.Errorf("read scenarios directory: %w", err)
	}

	var slugs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "index.ts" {
			continue
		}
		if !strings.HasSuffix(name, ".ts") {
			continue
		}
		// Skip the registry barrel and the `default.ts` placeholder is
		// handled the same as any other scenario — its `name` field is
		// "default" so byName("default") works.
		slug := strings.TrimSuffix(name, ".ts")
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	indexEntries := make([]scenarioIndexEntry, 0, len(slugs))
	for _, slug := range slugs {
		indexEntries = append(indexEntries, scenarioIndexEntry{
			Slug:  slug,
			Ident: scenarioImportIdent(slug),
		})
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		Scenarios []scenarioIndexEntry
	}{Scenarios: indexEntries}); err != nil {
		return fmt.Errorf("render scenarios-index: %w", err)
	}

	return os.WriteFile(filepath.Join(scenariosDir, "index.ts"), buf.Bytes(), 0o644)
}

// scenarioImportIdent converts a kebab-case scenario slug into a valid
// TS identifier suitable for use as the local binding of a default
// import. Examples: "default" -> "scenario_default",
// "github-connected" -> "scenario_githubConnected". The "scenario_"
// prefix avoids collisions with reserved words (default) and with the
// `defaultScenario` named re-export emitted by scenarios-index.ts.tmpl.
//
// Parts that begin with a digit (e.g. "1") are passed through as-is —
// the leading-letter rule on the slug itself (validated by
// validateScenarioName) guarantees the resulting identifier is valid.
func scenarioImportIdent(slug string) string {
	parts := strings.Split(slug, "-")
	var out strings.Builder
	out.WriteString("scenario_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i == 0 {
			out.WriteString(p)
			continue
		}
		r, sz := utf8.DecodeRuneInString(p)
		out.WriteRune(unicode.ToUpper(r))
		out.WriteString(p[sz:])
	}
	return out.String()
}
