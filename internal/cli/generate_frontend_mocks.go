package cli

import (
	"fmt"

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
	// the same seedplan.Config `forge db seed apply` uses — so the mocks and
	// the database cannot be looking at different plans. nil (no migrations,
	// no reachable shadow server) degrades every value to the synthetic
	// placeholder, which is what the seeder would write too.
	seedCfg := seedConfigFromStore(projectstore.New(cfg))
	seed := codegen.BuildSeedProjection(projectDir, seedCfg)

	// WHICH schema those fixtures describe. Read from the migration files
	// directly, not from the projection: the projection needs a reachable
	// postgres and the fingerprint does not, so the record of what the
	// fixtures stand for survives the case where the fixtures themselves
	// degraded to placeholders. See generator.EmitFixtureFreshnessSurface.
	seedFPFiles, seedFPConfig := codegen.SeedFingerprint(projectDir, seedCfg)

	for _, fe := range cfg.Frontends {
		if !generator.FrontendTypeHasMockSurface(fe.Type) {
			continue
		}

		feDir, ok := fe.Dir(projectDir)
		if !ok {
			// No directory in this repository — a cross-repo
			// source pin, or a path outside the project root.
			continue
		}

		count, err := generator.EmitFrontendMockSurface(projectDir, feDir, services, entities, seed, cs)
		if err != nil {
			return err
		}

		if err := generator.EmitFixtureFreshnessSurface(projectDir, feDir, seedFPFiles, seedFPConfig, cs); err != nil {
			return err
		}

		fmt.Printf("  ✅ Generated %d mock data file(s) + transport for frontend %s\n", count, fe.Name)
	}

	return nil
}
