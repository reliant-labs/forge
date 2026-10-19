package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
)

var (
	templateMu    sync.Mutex
	templateNames = map[string]string{} // key -> published template database
	templateIdent = regexp.MustCompile(`[^a-z0-9_]+`)
)

// NewFromTemplate is New for a database that already has a schema: the schema is
// built ONCE per (server, key) by build and every caller gets its own clone
// (CREATE DATABASE ... TEMPLATE), which copies files instead of replaying the
// migrations. For a project with a few hundred migrations and a few hundred
// tests that is the difference between seconds and minutes of test time.
//
// key names what build produces: change it whenever the schema build changes (a
// hash of the migration files is the usual choice) and a stale template is never
// served. The template lives on the shared server, so every process of a
// fan-out — one `go test` binary per package — builds it once between them: a
// postgres advisory lock serializes the builders, the finished template is
// published under its final name only when complete (a reader never sees a
// half-built one), and a builder that dies mid-way leaves a *_build database the
// next builder drops. The template dies with the server.
//
// build receives an open handle on the empty database to fill. Connections it
// leaves open are terminated before the template is published, but it must not
// need them afterwards.
func NewFromTemplate(key string, build func(*sql.DB) error) (*sql.DB, func(), error) {
	s, err := boot()
	if err != nil {
		return nil, nil, err
	}
	tmpl, err := ensureTemplate(s, key, build)
	if err != nil {
		return nil, nil, err
	}
	name := fmt.Sprintf("forge_test_%d_%d", os.Getpid(), dbCounter.Add(1))
	if _, err := s.baseDB.Exec("CREATE DATABASE " + name + " TEMPLATE " + tmpl); err != nil {
		return nil, nil, fmt.Errorf("pgtest: clone template %s into %s: %w", tmpl, name, err)
	}
	baseDB := s.baseDB
	drop := func() {
		_, _ = baseDB.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = baseDB.Exec("DROP DATABASE IF EXISTS " + name)
	}
	db, err := open(replaceDBName(s.baseURL, name))
	if err != nil {
		drop()
		return nil, nil, err
	}
	if err := pingWithRetry(db); err != nil {
		_ = db.Close()
		drop()
		return nil, nil, fmt.Errorf("pgtest: ping %s: %w", name, err)
	}
	return db, func() { _ = db.Close(); drop() }, nil
}

func ensureTemplate(s *server, key string, build func(*sql.DB) error) (string, error) {
	templateMu.Lock()
	defer templateMu.Unlock()
	// The cache is per (server, key): a Shutdown followed by a boot may be a
	// different server, and names are not unique across servers.
	cacheKey := s.baseURL + "\x00" + key
	if name, ok := templateNames[cacheKey]; ok {
		return name, nil
	}
	ctx := context.Background()
	final := "forge_tmpl_" + templateIdent.ReplaceAllString(strings.ToLower(key), "_")
	building := final + "_build"

	// Advisory locks are session-scoped: hold one dedicated connection, and the
	// lock releases itself if this process dies mid-build.
	conn, err := s.baseDB.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", final); err != nil {
		return "", err
	}
	defer func() { _, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock(hashtext($1))", final) }()

	var exists bool
	if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", final).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		// Holding the lock, nobody else is building: a *_build database is the
		// remains of a builder that died.
		if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+building+" WITH (FORCE)"); err != nil {
			return "", err
		}
		if _, err := conn.ExecContext(ctx, "CREATE DATABASE "+building); err != nil {
			return "", fmt.Errorf("pgtest: create template database: %w", err)
		}
		fail := func(err error) (string, error) {
			_, _ = conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+building+" WITH (FORCE)")
			return "", err
		}
		bdb, err := open(replaceDBName(s.baseURL, building))
		if err != nil {
			return fail(err)
		}
		if err := pingWithRetry(bdb); err != nil {
			_ = bdb.Close()
			return fail(err)
		}
		berr := build(bdb)
		_ = bdb.Close()
		if berr != nil {
			return fail(berr)
		}
		// CREATE DATABASE ... TEMPLATE refuses a source anyone is connected to.
		if _, err := conn.ExecContext(ctx,
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", building); err != nil {
			return fail(err)
		}
		if _, err := conn.ExecContext(ctx, "ALTER DATABASE "+building+" WITH ALLOW_CONNECTIONS false"); err != nil {
			return fail(err)
		}
		// The rename is the publish.
		if _, err := conn.ExecContext(ctx, "ALTER DATABASE "+building+" RENAME TO "+final); err != nil {
			return fail(fmt.Errorf("pgtest: publish template database: %w", err))
		}
	}
	templateNames[cacheKey] = final
	return final, nil
}
