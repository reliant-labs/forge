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
// must always emit at least a stub — even when there are no CRUD
// entities and the rich mock-transport would have nothing to dispatch
// on. Without the stub, webpack's static-analysis fails the production
// build with "Module not found: Can't resolve '@/lib/mock-transport'".
func generateFrontendMocks(cfg *config.ProjectConfig, services []codegen.ServiceDef, entities []codegen.EntityDef, projectDir string) error {
	// When the project has no CRUD entities or services, we still need
	// to emit a stub mock-transport for every frontend so the static
	// import in connect.ts resolves, plus the scenarios scaffolding so
	// the stub can dispatch via `?scenario=`.
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
		if err := emitScenarioScaffolding(mocksDir); err != nil {
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

// mockTransportStubContent is a minimal mock-transport with scenario
// support that returns a `Code.Unavailable` ConnectError on every RPC
// when no scenario handler matches. It exists so connect.ts's static
// import of `@/lib/mock-transport` resolves at build time even when the
// project has no entity-CRUD RPCs to dispatch on.
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
//
// Scenario support: even with no CRUD entities, projects can ship a
// scenario file (e.g. `src/mocks/scenarios/seeded.ts`) that overrides
// specific RPCs. The stub reads `?scenario=` once at module init, runs
// the optional `setup()`, and dispatches matching handlers. Anything
// unmatched still rejects with Code.Unavailable.
const mockTransportStubContent = `// AUTO-GENERATED — DO NOT EDIT
// Generated by forge generate
//
// Stub mock-transport — emitted when the project has no entity-CRUD RPCs
// to dispatch on. The real transport is generated when CRUD-shaped methods
// exist on at least one service. The stub exists so connect.ts's static
// import of '@/lib/mock-transport' resolves at build time AND so that
// NEXT_PUBLIC_MOCK_API=true builds don't fail at prerender.
//
// Scenario overlay (see docs/adr/0002-frontend-scenarios.md): the stub
// reads ?scenario=<name> once at module init and dispatches matching
// handlers from the named scenario. RPCs not covered by a scenario
// handler reject with Code.Unavailable + a clear message.

import type { Transport, UnaryResponse } from "@connectrpc/connect";
import { Code, ConnectError } from "@connectrpc/connect";
import * as scenarios from "@/mocks/scenarios";

const STUB_MESSAGE =
  "mock-transport stub: project has no entity-CRUD RPCs; no fixture data " +
  "to dispatch. Add CRUD methods to a service so forge generate produces a " +
  "real mock-transport, or set NEXT_PUBLIC_MOCK_API=false.";

// Resolve the active scenario once at module init. Reading the URL here
// (vs. on every RPC) means a navigation away from ?scenario= keeps the
// scenario active until a full reload — matching the agent-driven flow
// where the URL is the single source of truth.
const requested =
  typeof globalThis !== "undefined" && globalThis.location
    ? new URLSearchParams(globalThis.location.search).get("scenario")
    : null;
// Use a ternary instead of && so the empty-string falsy branch doesn't
// leak '' into the inferred union type. With ?? alone, requested && X
// narrows to '' | Scenario | undefined, and the empty-string survives ??
// because it only replaces null/undefined — so subsequent access on
// active.setup / active.handlers would error under strict tsc.
const active =
  (requested ? scenarios.byName(requested) : undefined) ??
  scenarios.defaultScenario;

// setup() runs once before any RPC fires. Reserved for non-RPC state
// (localStorage flags, sessionStorage). Synchronous; no network calls.
active.setup?.();

function makeUnaryResponse<T>(
  service: { typeName: string },
  method: { name: string },
  message: T,
): UnaryResponse<never, never> {
  return {
    service: service as never,
    method: method as never,
    stream: false,
    header: new Headers(),
    message: message as never,
    trailer: new Headers(),
  };
}

export function createMockTransport(): Transport {
  return {
    async unary(
      service: { typeName: string },
      method: { name: string },
      _signal: AbortSignal | undefined,
      _timeoutMs: number | undefined,
      _header: HeadersInit | undefined,
      message: unknown,
    ) {
      const key = ` + "`${service.typeName}/${method.name}`" + `;

      // Scenario overlay — first crack at every unary RPC.
      const handler = active.handlers[key];
      if (handler) {
        const result = await handler(message);
        return makeUnaryResponse(service, method, result);
      }

      // No handler — same behavior as the pre-scenarios stub.
      return Promise.reject(new ConnectError(STUB_MESSAGE, Code.Unavailable));
    },
    // Streaming is not supported in v1. See ADR 0002.
    stream() {
      throw new ConnectError(
        "mock-transport: scenarios don't support streaming yet",
        Code.Unimplemented,
      );
    },
  } as unknown as Transport;
}

// Re-export the active scenario so tests + dev tools can introspect it.
export const activeScenario = active;
`

// emitMockTransportStubs writes the no-op mock-transport stub to every
// frontend, along with the scenarios scaffolding. Used when the project
// has no CRUD entities at all (the early-return path of
// generateFrontendMocks).
func emitMockTransportStubs(cfg *config.ProjectConfig, projectDir string) error {
	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") && !strings.EqualFold(fe.Type, "vite-spa") {
			continue
		}
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}

		// Scenarios scaffolding lives under src/mocks/ — emit it before
		// the stub so the stub's `import * as scenarios from "@/mocks/scenarios"`
		// resolves on a fresh scaffold with no other mocks.
		mocksDir := filepath.Join(projectDir, feDir, "src", "mocks")
		if err := os.MkdirAll(mocksDir, 0o755); err != nil {
			return fmt.Errorf("create mocks directory: %w", err)
		}
		if err := emitScenarioScaffolding(mocksDir); err != nil {
			return fmt.Errorf("emit scenario scaffolding: %w", err)
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

// emitScenarioScaffolding writes the three scenarios files into
// <mocksDir>/scenarios/:
//
//   - scenario-types.ts (always regenerated — the contract type)
//   - default.ts (seeded once; safe to hand-edit)
//   - index.ts (regenerated by walking the directory for *.ts files)
//
// scenario-types.ts is emitted one level up at <mocksDir>/scenario-types.ts
// so user scenarios can `import { defineScenario } from "../scenario-types"`.
// The regeneration is idempotent: running `forge generate` twice produces
// the same output.
//
// User-authored scenarios (created with `forge add scenario`) are NEVER
// touched here — the registry just re-exports them.
func emitScenarioScaffolding(mocksDir string) error {
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
