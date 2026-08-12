package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/database"
	"github.com/reliant-labs/forge/internal/devpg"
	"github.com/reliant-labs/forge/internal/hostinfra"
	"github.com/reliant-labs/forge/internal/projectstore"
	"github.com/reliant-labs/forge/pkg/pgtest"
	"github.com/reliant-labs/forge/pkg/seedplan"
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
	// Reconcile BEFORE the first write. This is the last point at which
	// forge can tell the DSN apart from the database it is supposed to
	// name: the very next call issues CREATE DATABASE against whatever is
	// listening at the DSN's coordinates, and after that the project's
	// tables and seed rows are in there regardless of whose postgres it is.
	//
	// A scaffolded project's DSN is derived from POSTGRES_PORT (see
	// codegen.devDatabaseDSN), but it is derived ONCE, at scaffold time.
	// Run the same project later under a different POSTGRES_PORT and
	// compose moves postgres while the committed DSN stays put — the exact
	// divergence that had projects creating their schema inside another
	// stack's database while their own postgres sat empty, with `forge run`
	// reporting success throughout. Refuse loudly instead.
	if err := reconcileDevDatabasePort(dsn, entities); err != nil {
		return err
	}
	if err := pgtest.EnsureDatabase(dsn); err != nil {
		return fmt.Errorf("ensure dev database: %w", err)
	}
	return nil
}

// reconcileDevDatabasePort checks the dev DSN against the port this
// project's dev database ACTUALLY listens on, and returns a runbook error
// when they disagree. A project whose dev database forge cannot locate, or
// whose DSN is aimed off-box, has nothing to reconcile and passes.
//
// Where that port comes from depends on how the env declares its database,
// and asking the wrong source is worse than not asking: a guard that
// refuses a CORRECT configuration teaches people to route around it, and
// the workaround is permanent while the false alarm was not.
//
//   - HOST-RUN (`forge.HostInfra`, the scaffolded default) — the port is in
//     the declaration, and the DSN is composed from that same variable, so
//     the two cannot drift. The check is a tautology and is skipped.
//   - CONTAINERIZED (`forge.Compose`) — the port lives in the compose file's
//     `${POSTGRES_PORT:-5432}` while the DSN is a separate string, so they
//     CAN drift. Resolve it with docker compose's own precedence (shell over
//     the project's `.env` over the compose default), and when forge cannot
//     faithfully reproduce that interpolation, say so and stand down rather
//     than refuse on a port it is not sure of.
func reconcileDevDatabasePort(dsn string, entities *KCLEntities) error {
	if declaresHostInfraPostgres(entities) {
		return nil
	}
	dir := projectDirForKCL()
	port, unknown := devpg.ResolveComposePort(dir, postgresComposeEnvFiles(entities))
	if unknown != "" {
		fmt.Printf("  Note: skipping the dev database port check — %s.\n"+
			"        DATABASE_URL (port %s) was NOT verified against the port compose publishes.\n",
			unknown, devpg.PortOf(dsn))
		return nil
	}
	return devpg.Reconcile(dsn, port)
}

// declaresHostInfraPostgres reports whether the env runs its database as a
// forge-supervised host process rather than a container.
func declaresHostInfraPostgres(entities *KCLEntities) bool {
	if entities == nil {
		return false
	}
	for _, s := range entities.Services {
		if s.Deploy.Type == "host-infra" && s.Deploy.HostInfra != nil &&
			s.Deploy.HostInfra.Engine == hostinfra.EnginePostgres {
			return true
		}
	}
	return false
}

// postgresComposeEnvFiles returns the `--env-file` paths forge itself will
// pass when it brings the postgres compose service up (deploytarget/compose.go
// forwards a compose service's declared env_file as --env-file). That flag
// REPLACES compose's default `.env`, so the reconcile has to interpolate from
// the same file or it would compare against a port compose never uses.
//
// Nil — the common case — means no env_file is declared and compose falls
// back to the project's `.env`.
func postgresComposeEnvFiles(entities *KCLEntities) []string {
	if entities == nil {
		return nil
	}
	for _, s := range entities.Services {
		if s.Deploy.Type != "compose" || s.Deploy.Compose == nil {
			continue
		}
		if s.Deploy.Compose.Service != "postgres" && s.Name != "postgres" {
			continue
		}
		if f := s.Deploy.Compose.EnvFile; f != "" {
			return []string{f}
		}
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
	// Seeding WRITES. ensureDevDatabase already reconciles the DSN against
	// the compose port, but only on the host-only path (`forge run`), while
	// this hook also runs for a full `forge env up` — so the check is
	// repeated here at the write boundary rather than assumed. Unlike every
	// other skip in this function this one is not a soft warning about
	// missing rows: it means the rows would land in a database this project
	// does not own.
	if err := reconcileDevDatabasePort(dsn, entities); err != nil {
		fmt.Printf("[up] auto-seed REFUSED: %v\n", err)
		return
	}
	db, err := database.ConnectDB(ctx, dsn)
	if err != nil {
		fmt.Printf("[up] auto-seed skipped: database not reachable (%v)\n", err)
		return
	}
	defer func() { _ = db.Close() }()

	plan, err := seedplan.BuildLivePlan(ctx, db, migrationsDefault(), seedShadowFor(migrationsDefault()), seedConfigFromProject())
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
	empty, err := seedplan.AllSeedableTablesEmpty(ctx, db, plan)
	if err != nil {
		fmt.Printf("[up] auto-seed skipped: could not tell whether the seedable tables are empty (%v)\n", err)
		return
	}
	if !empty {
		// quiet: the steady state. The database already holds rows, so
		// first-boot seeding has nothing to do and never had.
		return
	}
	res, err := seedplan.Apply(ctx, db, plan)
	if err != nil {
		fmt.Printf("[up] auto-seed skipped: %v\n", err)
		return
	}
	if res.Total() > 0 {
		fmt.Printf("[up] auto-seeded %d rows across %d tables (first boot; disable with --no-seed)\n",
			res.Total(), len(res.Tables))
	}
}

// resolveSeedDSN finds the DATABASE_URL that ensureDevDatabase, the
// auto-seed hook and the discovery facts should use.
//
// THE ORDER IS THE CONTRACT, and it must match the precedence the host
// processes themselves see (hostlaunch.LayerHostEnv): the shell wins, then
// the KCL declaration, then the secret store, then per-env project config.
// Anything else and forge prepares one database while the app dials
// another — which is not a cosmetic disagreement: it creates the database,
// applies migrations and seeds rows somewhere the app will never look,
// reporting success the whole way.
//
// The KCL DECLARATION ahead of the secret store is the half that took an
// incident to get right. `secrets/dev.yaml` is seeded once, at scaffold
// time, with a DSN naming whatever port was free THEN; the env's KCL
// composes its DSN from the port it declares the database on TODAY. When
// those disagree the declaration is the one that is true — it is what the
// server actually bound and what the app's own env carries — and the stored
// copy is a stale artifact of the day the project was created.
func resolveSeedDSN(entities *KCLEntities, cfg *config.ProjectConfig, env string) string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	// The KCL-declared value, from the same env stream the host services
	// get. A service's own env_vars first, then its deploy block's.
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
			if s.Deploy.Cluster != nil {
				if v := envVarValue(s.Deploy.Cluster.EnvVars, "DATABASE_URL"); v != "" {
					return v
				}
			}
		}
	}
	// The env's SECRET PROVIDER. Load-bearing for a project whose DSN is
	// genuinely only a secret: DATABASE_URL is a `sensitive` config field,
	// so the KCL projection emits a Secret REFERENCE rather than a value,
	// and a project that has not declared a DSN in KCL keeps its real one
	// here. An `external` provider resolves nothing (by design), so cloud
	// envs fall through unchanged.
	if prov, err := secretProviderFromEntities(entities, projectDirForKCL()); err == nil {
		if v, ok := prov.Resolve("DATABASE_URL"); ok && v != "" {
			return v
		}
	}
	if m := loadProjectConfigEnv(cfg, env); m["DATABASE_URL"] != "" {
		return m["DATABASE_URL"]
	}
	return ""
}
