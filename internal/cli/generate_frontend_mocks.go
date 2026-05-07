package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/jinzhu/inflection"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateFrontendMocks generates TypeScript mock data files and a mock
// transport for each Next.js frontend. Mock data files contain typed arrays
// derived from entity definitions using the same deterministic values as
// db/fixtures. The mock transport intercepts ConnectRPC calls and returns
// fixture data, enabled via NEXT_PUBLIC_MOCK_API=true.
//
// `connect.ts` unconditionally references `@/lib/mock-transport` (it
// dynamic-`require`s the path under NEXT_PUBLIC_MOCK_API=true), so we
// must always emit at least a stub — even when there are no CRUD
// entities and the rich mock-transport would have nothing to dispatch
// on. Without the stub, webpack's static-analysis fails the production
// build with "Module not found: Can't resolve '@/lib/mock-transport'".
func generateFrontendMocks(cfg *config.ProjectConfig, services []codegen.ServiceDef, entities []codegen.EntityDef, projectDir string) error {
	// When the project has no CRUD entities or services, we still need
	// to emit a stub mock-transport for every Next.js frontend so the
	// static import in connect.ts resolves. Skip the rest of the
	// pipeline in that case.
	if len(entities) == 0 || len(services) == 0 {
		return emitMockTransportStubs(cfg, projectDir)
	}

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
		if !strings.EqualFold(fe.Type, "nextjs") {
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
		// When the project has CRUD entities the rich template produces a
		// dispatch table keyed on RPC names. When it doesn't (services
		// exist but only carry bespoke non-CRUD methods) we fall back to
		// the stub so connect.ts's static import still resolves.
		libDir := filepath.Join(projectDir, feDir, "src", "lib")
		if err := os.MkdirAll(libDir, 0o755); err != nil {
			return fmt.Errorf("create lib directory: %w", err)
		}
		outPath := filepath.Join(libDir, "mock-transport.ts")

		if len(transportEntities) > 0 {
			transportData := codegen.MockTransportTemplateData{
				Entities: transportEntities,
			}

			var buf bytes.Buffer
			if err := mockTransportTmpl.Execute(&buf, transportData); err != nil {
				return fmt.Errorf("render mock transport: %w", err)
			}

			if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
				return fmt.Errorf("write mock transport: %w", err)
			}
		} else {
			if err := os.WriteFile(outPath, []byte(mockTransportStubContent), 0o644); err != nil {
				return fmt.Errorf("write mock transport stub: %w", err)
			}
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

// mockTransportStubContent is a minimal mock-transport that returns a
// `Code.Unavailable` ConnectError on every RPC. It exists so connect.ts's
// static import of `@/lib/mock-transport` resolves at build time even when
// the project has no entity-CRUD RPCs to dispatch on.
//
// Crucially, `createMockTransport()` itself MUST NOT throw — Next.js
// prerender (e.g. of `/_not-found`) evaluates `connect.ts` which calls
// `createMockTransport()` at module-eval time when
// `NEXT_PUBLIC_MOCK_API=true` is in `.env.local`. A throw at module-eval
// fails `npm run build` with an opaque `digest: ...` error that doesn't
// finger-point at the env var. Returning a valid `Transport` whose
// `unary` / `stream` reject with `Code.Unavailable` lets pages render
// their existing 3-state error UI (the same UI they show for any
// transient backend outage), which prerender treats as a successful
// render.
const mockTransportStubContent = `// AUTO-GENERATED — DO NOT EDIT
// Generated by forge generate
//
// Stub mock-transport — emitted when the project has no entity-CRUD RPCs
// to dispatch on. The real transport is generated when CRUD-shaped methods
// exist on at least one service. The stub exists so connect.ts's static
// import of '@/lib/mock-transport' resolves at build time AND so that
// NEXT_PUBLIC_MOCK_API=true builds don't fail at prerender.
//
// Every RPC rejects with Code.Unavailable + a clear message. Pages that
// follow the loading/error/success rendering convention will render the
// error banner; prerender treats that as a successful render. To get
// real fixture data, add CRUD methods to a service (so forge generate
// produces a real mock-transport), or set NEXT_PUBLIC_MOCK_API=false
// and run a backend.

import type { Transport } from "@connectrpc/connect";
import { Code, ConnectError } from "@connectrpc/connect";

const STUB_MESSAGE =
  "mock-transport stub: project has no entity-CRUD RPCs; no fixture data " +
  "to dispatch. Add CRUD methods to a service so forge generate produces a " +
  "real mock-transport, or set NEXT_PUBLIC_MOCK_API=false.";

export function createMockTransport(): Transport {
  const reject = () => Promise.reject(new ConnectError(STUB_MESSAGE, Code.Unavailable));
  return {
    unary: reject,
    stream: reject,
  } as unknown as Transport;
}
`

// emitMockTransportStubs writes the no-op mock-transport stub to every
// Next.js frontend. Used when the project has no CRUD entities at all
// (the early-return path of generateFrontendMocks).
func emitMockTransportStubs(cfg *config.ProjectConfig, projectDir string) error {
	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") {
			continue
		}
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		libDir := filepath.Join(projectDir, feDir, "src", "lib")
		if err := os.MkdirAll(libDir, 0o755); err != nil {
			return fmt.Errorf("create lib directory: %w", err)
		}
		outPath := filepath.Join(libDir, "mock-transport.ts")
		if err := os.WriteFile(outPath, []byte(mockTransportStubContent), 0o644); err != nil {
			return fmt.Errorf("write mock transport stub: %w", err)
		}
	}
	return nil
}

// writeMocksIndex writes a barrel index.ts that re-exports all mock data files.
func writeMocksIndex(mocksDir string, slugs []string) error {
	var buf bytes.Buffer
	buf.WriteString("// AUTO-GENERATED — DO NOT EDIT\n")
	buf.WriteString("// Generated by forge generate\n\n")

	for _, slug := range slugs {
		buf.WriteString(fmt.Sprintf("export * from \"./%s\";\n", slug))
	}

	return os.WriteFile(filepath.Join(mocksDir, "index.ts"), buf.Bytes(), 0o644)
}