// Package cli — `forge generate --explain` provenance log.
//
// Explain mode runs the normal generate pipeline and then prints a per-file
// traceability log: for each forge-tracked output, which source files /
// proto descriptors / contract.go inputs drove its generation, plus a
// "reason" field saying whether the file was rewritten or skipped because
// the input was unchanged.
//
// Implementation choice: rather than instrument every individual generator
// (mock_gen, middleware_gen, crud_gen, …) with a callback, we read the
// post-generate state from forge_descriptor.json, the project filesystem,
// and .forge/checksums.json. The derived provenance is "approximate but
// useful" — exactly what the LLM caller wants. We can tighten it later by
// threading a callback through the codegen package, but we don't need to
// pay that cost up front to ship the most-useful 80%.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/generator"
)

// ExplainEntry is one row in the per-file provenance log.
type ExplainEntry struct {
	OutputPath string   `json:"output_path"`
	Sources    []string `json:"sources,omitempty"`
	Kind       string   `json:"kind"` // "service-handler", "service-mock", "entity-orm", "config", "contract", "frontend"
	Notes      []string `json:"notes,omitempty"`
	Skipped    bool     `json:"skipped,omitempty"`
}

// printExplainLog walks the post-generate project, computes provenance for
// every tracked output, and prints a human-readable log. Called from
// generate's RunE only when --explain is set.
//
// preChecksums is the checksum map captured BEFORE the pipeline ran. We
// diff it against the post-generate state to label each entry as written
// or skipped (idempotent no-op).
func printExplainLog(projectDir string, preChecksums map[string]string) error {
	cs, err := generator.LoadChecksums(projectDir)
	if err != nil {
		return fmt.Errorf("load checksums: %w", err)
	}

	// Pull descriptor data. If the descriptor doesn't exist (degenerate
	// project) we just skip the proto-driven explain section.
	desc, _ := loadForgeDescriptor(projectDir)

	// Build a service-name → ServiceDef index keyed by both proto name and
	// snake-case package name (for handlers/<svc>/).
	svcByName := map[string]codegen.ServiceDef{}
	if desc != nil {
		for _, s := range desc.Services {
			svcByName[s.Name] = s
			svcByName[serviceNameToPackage(s.Name)] = s
		}
	}

	// Walk every tracked file, classify it, attach sources.
	tracked := make([]string, 0, len(cs.Files))
	for k := range cs.Files {
		tracked = append(tracked, k)
	}
	sort.Strings(tracked)

	var entries []ExplainEntry
	for _, rel := range tracked {
		entry := ExplainEntry{OutputPath: rel}
		entry.Kind, entry.Sources, entry.Notes = classifyGeneratedFile(rel, projectDir, svcByName, desc)

		// Was this file rewritten this run? Compare pre vs post checksum.
		if pre, hadPre := preChecksums[rel]; hadPre && pre == cs.Files[rel].Hash {
			entry.Skipped = true
			entry.Notes = append(entry.Notes, "no input change since last gen (idempotent skip)")
		} else if !cs.IsFileModified(projectDir, rel) {
			entry.Notes = append(entry.Notes, "rewritten this run")
		}
		entries = append(entries, entry)
	}

	// Render to stdout. Group by kind so the output stays scannable.
	fmt.Println()
	fmt.Println("📋 Generation provenance:")
	fmt.Println()
	if len(entries) == 0 {
		fmt.Println("  (no tracked outputs — nothing to explain)")
		return nil
	}

	groups := map[string][]ExplainEntry{}
	for _, e := range entries {
		groups[e.Kind] = append(groups[e.Kind], e)
	}
	groupKeys := make([]string, 0, len(groups))
	for k := range groups {
		groupKeys = append(groupKeys, k)
	}
	sort.Strings(groupKeys)

	for _, k := range groupKeys {
		fmt.Printf("── %s ──\n", k)
		for _, e := range groups[k] {
			icon := "✅"
			if e.Skipped {
				icon = "⏭️ "
			}
			fmt.Printf("  %s %s\n", icon, e.OutputPath)
			for _, src := range e.Sources {
				fmt.Printf("     ← from %s\n", src)
			}
			for _, n := range e.Notes {
				fmt.Printf("     ← reason: %s\n", n)
			}
		}
		fmt.Println()
	}
	fmt.Printf("(%d output(s) tracked, generated at %s)\n", len(entries), time.Now().UTC().Format(time.RFC3339))
	return nil
}

// loadForgeDescriptor reads gen/forge_descriptor.json into a typed value.
// Returns (nil, nil) when the descriptor doesn't exist — caller treats
// that as "no proto data, skip proto-driven explain".
func loadForgeDescriptor(projectDir string) (*ForgeDescriptor, error) {
	path := filepath.Join(projectDir, "gen", "forge_descriptor.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var desc ForgeDescriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return nil, err
	}
	return &desc, nil
}

// classifyGeneratedFile returns (kind, sources, notes) for one tracked
// output file. The pattern matching is intentionally string-shaped — we
// follow forge's directory conventions (handlers/<svc>/_gen.go,
// internal/<pkg>/mock_gen.go, gen/services/<svc>/...) rather than
// re-deriving the call graph through the generators.
func classifyGeneratedFile(rel, projectDir string, svcByName map[string]codegen.ServiceDef, desc *ForgeDescriptor) (kind string, sources []string, notes []string) {
	relSlash := filepath.ToSlash(rel)

	switch {
	case strings.HasPrefix(relSlash, "handlers/") && strings.HasSuffix(relSlash, "_gen.go"):
		kind = "service-handler"
		// handlers/<pkg>/<file>_gen.go → look up the service
		parts := strings.Split(relSlash, "/")
		if len(parts) >= 2 {
			svcPkg := parts[1]
			if svc, ok := svcByName[svcPkg]; ok {
				sources = append(sources, svc.ProtoFile)
				if len(svc.Methods) > 0 {
					rpcs := make([]string, 0, len(svc.Methods))
					for _, m := range svc.Methods {
						rpcs = append(rpcs, m.Name)
					}
					notes = append(notes, fmt.Sprintf("%d RPC(s) declared (%s)", len(rpcs), strings.Join(rpcs, ", ")))
				}
			}
		}

	case strings.HasPrefix(relSlash, "handlers/mocks/") && strings.HasSuffix(relSlash, "_mock.go"):
		kind = "service-mock"
		base := strings.TrimSuffix(filepath.Base(relSlash), "_mock.go")
		if svc, ok := svcByName[base]; ok {
			sources = append(sources, svc.ProtoFile)
		}

	case strings.HasPrefix(relSlash, "gen/services/"):
		kind = "service-stub"
		// gen/services/<svc>/v1/foo.pb.go → buf-generated from proto
		parts := strings.Split(relSlash, "/")
		if len(parts) >= 3 {
			svcPkg := parts[2]
			if svc, ok := svcByName[svcPkg]; ok {
				sources = append(sources, svc.ProtoFile)
			}
		}

	case strings.HasPrefix(relSlash, "internal/db/") && strings.HasSuffix(relSlash, "_gen.go"):
		kind = "entity-orm"
		// internal/db/<entity>_orm_gen.go from proto entity
		base := filepath.Base(relSlash)
		base = strings.TrimSuffix(base, "_orm_gen.go")
		base = strings.TrimSuffix(base, "_gen.go")
		if desc != nil {
			for _, e := range desc.Entities {
				if strings.EqualFold(e.Name, base) || strings.EqualFold(e.TableName, base) {
					sources = append(sources, e.ProtoFile)
					notes = append(notes, fmt.Sprintf("entity %s (table=%s, %d field(s))", e.Name, e.TableName, len(e.Fields)))
					break
				}
			}
		}

	case strings.HasPrefix(relSlash, "internal/") && (strings.HasSuffix(relSlash, "/mock_gen.go") || strings.HasSuffix(relSlash, "/middleware_gen.go") ||
		strings.HasSuffix(relSlash, "/tracing_gen.go") || strings.HasSuffix(relSlash, "/metrics_gen.go")):
		kind = "contract"
		// driven by sibling contract.go
		dir := filepath.Dir(relSlash)
		contractPath := filepath.Join(projectDir, dir, "contract.go")
		if _, err := os.Stat(contractPath); err == nil {
			sources = append(sources, filepath.ToSlash(filepath.Join(dir, "contract.go")))
		}

	case strings.HasPrefix(relSlash, "pkg/config/") && strings.HasSuffix(relSlash, "_gen.go"):
		kind = "config"
		if desc != nil {
			for _, c := range desc.Configs {
				notes = append(notes, fmt.Sprintf("config message %s (%d field(s))", c.Name, len(c.Fields)))
			}
		}

	case strings.HasPrefix(relSlash, "frontends/"):
		kind = "frontend"
		// e.g. frontends/web/src/hooks/<svc>-hooks.ts
		base := filepath.Base(relSlash)
		base = strings.TrimSuffix(base, "-hooks.ts")
		base = strings.TrimSuffix(base, ".ts")
		base = strings.TrimSuffix(base, ".tsx")
		for k, svc := range svcByName {
			if strings.Contains(strings.ToLower(base), strings.ToLower(k)) {
				sources = append(sources, svc.ProtoFile)
				break
			}
		}

	case strings.HasPrefix(relSlash, "pkg/app/") && strings.HasSuffix(relSlash, "bootstrap.go"):
		kind = "bootstrap"
		notes = append(notes, "wires every service + every package contract")

	default:
		kind = "other"
	}
	return kind, sources, notes
}
