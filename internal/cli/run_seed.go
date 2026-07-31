package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/database"
	"github.com/reliant-labs/forge/internal/projectstore"
	"github.com/reliant-labs/forge/internal/seeddata"
	"github.com/reliant-labs/forge/pkg/pgtest"
)

// ensureDevDatabase creates the dev database the host services are about to
// dial when it does not already exist — so a `forge run` against a freshly
// scaffolded project boots alive. It is the runtime counterpart to forge's
// generate-time shadow DB, which pgtest already ensure-creates on the fly:
// the scaffolded dev DSN (postgres://…:5434/<project>) names a database
// nothing has issued CREATE DATABASE for, so the app's first boot would die
// with `FATAL: database "<project>" does not exist` before AUTO_MIGRATE could
// apply the schema.
//
// Dev-only (seedTargetIsDev — the same fail-closed classifier the auto-seed
// gate reads) and only when a DSN is actually resolved. The maintenance
// connection failing is a HARD error: the app cannot boot without the DB
// server, so `forge run` says so loudly here rather than let the app fail
// later with an opaque connect error.
func ensureDevDatabase(cfg *config.ProjectConfig, entities *KCLEntities, env string) error {
	dev, err := seedTargetIsDev(env)
	if err != nil || !dev {
		return nil
	}
	dsn := resolveSeedDSN(entities, cfg, env)
	if dsn == "" {
		return nil
	}
	if err := pgtest.EnsureDatabase(dsn); err != nil {
		return fmt.Errorf("ensure dev database: %w", err)
	}
	return nil
}

// maybeAutoSeed is the `forge run` / `forge env up --host-only` first-boot
// auto-seed hook. It runs after the host-services readiness gate (so the
// app's AUTO_MIGRATE has already applied migrations) and materializes the
// deterministic dev dataset exactly once — when the target is dev, the DB is
// reachable, and every seedable table is empty. Every failure mode (no
// DATABASE_URL, unreachable DB, non-empty tables, apply error) is a warning,
// never fatal to the dev loop.
func maybeAutoSeed(ctx context.Context, store *projectstore.Store, cfg *config.ProjectConfig, entities *KCLEntities, opts upOptions) {
	if opts.noSeed {
		// quiet: the user passed --no-seed; echoing their own flag back is noise.
		return
	}
	if store != nil && !store.Database().Seed.AutoEnabled() {
		// quiet: database.seed.auto is false in forge.yaml — a standing
		// project-level decision, not an outcome of this run.
		return
	}
	dev, err := seedTargetIsDev(opts.env)
	if err != nil || !dev {
		// quiet: auto-seed is a dev-only affordance. On staging/prod the
		// absence of demo rows is the correct and expected state.
		return
	}
	dsn := resolveSeedDSN(entities, cfg, opts.env)
	if dsn == "" {
		// Nothing to seed against. This used to return in silence on the
		// theory that a host-only run legitimately starts no database — but
		// the app itself resolves a DSN through its own config layering, so a
		// run where forge cannot find one still boots a live app against a
		// real database and simply never seeds it. Silence there reads as
		// "seeded, and the domain has no rows", which is the state the
		// charter tells the next phase to treat as a blocker.
		fmt.Printf("[up] auto-seed skipped: no DATABASE_URL resolved for env %q (checked the environment, forge.yaml config, the env's secret provider, and the host-service KCL env)\n", opts.env)
		return
	}
	db, err := database.ConnectDB(ctx, dsn)
	if err != nil {
		fmt.Printf("[up] auto-seed skipped: database not reachable (%v)\n", err)
		return
	}
	defer func() { _ = db.Close() }()

	plan, err := seeddata.BuildLivePlan(ctx, db, migrationsDefault(), seedConfigFromProject())
	if err != nil {
		fmt.Printf("[up] auto-seed skipped: %v\n", err)
		return
	}
	// Vocabulary validation + constraint-satisfaction warnings: worth a line
	// even on the quiet first-boot path — this is where most users first meet
	// seeds, and a row target capped by a UNIQUE column is surprising in silence.
	for _, w := range plan.Warnings() {
		fmt.Printf("[up] %s\n", w)
	}
	// First-boot only: never touch a dev DB that already has data. The two
	// non-seeding outcomes are different events and used to collapse into
	// one silent return: "already has rows" is the expected steady state,
	// while a count ERROR means forge could not tell — typically the schema
	// is not applied yet, i.e. exactly the fresh-database case auto-seed
	// exists to serve. Reporting only the second keeps the happy path quiet
	// without letting a failed probe masquerade as "nothing to do".
	empty, err := seeddata.AllSeedableTablesEmpty(ctx, db, plan)
	if err != nil {
		fmt.Printf("[up] auto-seed skipped: could not tell whether the seedable tables are empty (%v)\n", err)
		return
	}
	if !empty {
		// quiet: the steady state. The database already holds rows, so
		// first-boot seeding has nothing to do and never had.
		return
	}
	res, err := seeddata.Apply(ctx, db, plan)
	if err != nil {
		fmt.Printf("[up] auto-seed skipped: %v\n", err)
		return
	}
	if res.Total() > 0 {
		fmt.Printf("[up] auto-seeded %d rows across %d tables (first boot; disable with --no-seed)\n",
			res.Total(), len(res.Tables))
	}
}

// resolveSeedDSN finds a DATABASE_URL for the auto-seed hook: an exported env
// var first, then the per-env project config, then the env's SECRET PROVIDER,
// then the rendered host-service KCL env (the compose/devstack DSN the
// services themselves dial).
//
// The secret-provider probe is the load-bearing one for a scaffolded project:
// DATABASE_URL is a `sensitive` config field, so the KCL projection emits a
// Secret REFERENCE rather than a value and the per-env config carries no DSN
// at all. The value lives in the env's dotenv provider (`.env.<env>`) — the
// same source `forge run` layers onto the host processes — so reading it here
// keeps ensureDevDatabase / auto-seed / the discovery facts pointed at the
// database the app itself dials. An `external` provider resolves nothing (by
// design), so cloud envs fall through unchanged.
func resolveSeedDSN(entities *KCLEntities, cfg *config.ProjectConfig, env string) string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	if m := loadProjectConfigEnv(cfg, env); m["DATABASE_URL"] != "" {
		return m["DATABASE_URL"]
	}
	if prov, err := secretProviderFromEntities(entities, projectDirForKCL()); err == nil {
		if v, ok := prov.Resolve("DATABASE_URL"); ok && v != "" {
			return v
		}
	}
	if entities != nil {
		for _, s := range entities.Services {
			if v := envVarValue(s.EnvVars, "DATABASE_URL"); v != "" {
				return v
			}
			if s.Deploy.Host != nil {
				if v := envVarValue(s.Deploy.Host.EnvVars, "DATABASE_URL"); v != "" {
					return v
				}
			}
		}
	}
	return ""
}
