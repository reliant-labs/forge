package cli

import (
	"fmt"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/projectstore"
)

// generateFrontendMocks re-emits the mock/scenario surface of every Next.js /
// Vite frontend with the project's parsed services + entities, so the
// transport's per-entity fixture dispatch table tracks the current schema.
//
// The surface itself (what files, and why the transport is always the rich
// scenario-capable render rather than a stub) is described on
// generator.EmitFrontendMockSurface, which the frontend SCAFFOLD also calls —
// with no entities — so a freshly added frontend compiles before this step
// has ever run. React Native frontends have no mock surface.
//
// cs carries the project's ownership state: every file this step emits is
// Tier-1 (regenerated every run, per its own header), so each goes through
// the certification chokepoint that stamps the forge:hash marker `forge
// project disown` and the drift lint both read.
func generateFrontendMocks(cfg *config.ProjectConfig, services []codegen.ServiceDef, entities []codegen.EntityDef, projectDir string, cs *checksums.FileChecksums) error {
	// The dev dataset every fixture value is read from, resolved once for
	// all frontends. It is built from the project's OWN seed configuration —
	// the same seeddata.Config `forge db seed apply` uses — so the mocks and
	// the database cannot be looking at different plans. nil (no migrations,
	// no reachable shadow server) degrades every value to the synthetic
	// placeholder, which is what the seeder would write too.
	seed := codegen.BuildSeedProjection(projectDir, seedConfigFromStore(projectstore.New(cfg)))

	for _, fe := range cfg.Frontends {
		if !generator.FrontendTypeHasMockSurface(fe.Type) {
			continue
		}

		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}

		count, err := generator.EmitFrontendMockSurface(projectDir, feDir, services, entities, seed, cs)
		if err != nil {
			return err
		}

		fmt.Printf("  ✅ Generated %d mock data file(s) + transport for frontend %s\n", count, fe.Name)
	}

	return nil
}
