// Package pgtest provides a REAL ephemeral postgres for generate-time
// schema introspection and for hermetic tests that need a database.
//
// # Why real postgres
//
// forge is postgres-pinned. The schema a project's migrations declare —
// schema-qualified DDL (CREATE TABLE controlplane.foo), postgres-only
// types (TIMESTAMPTZ, JSONB, TEXT[], BIGSERIAL), '::type' casts,
// multi-ADD ALTERs, generated/identity columns — only round-trips
// faithfully on real postgres. The previous in-memory SQLite "shadow"
// approximated this and froze the ORM the moment a project used a
// construct SQLite couldn't parse (the controlplane.-schema bug). Real
// postgres needs no normalization: migrations apply verbatim.
//
// # The shared instance
//
// Booting postgres is expensive, so this package boots ONE server per
// process (sync.Once) and hands every caller its own freshly-created,
// uniquely-named database on that server. Databases are cheap; the
// server boot is the cost, paid once. This mirrors how the e2e corpus
// builds the forge binary once via sync.Once and runs fixtures in
// parallel against it.
//
// By default the server is an embedded-postgres binary
// (github.com/fergusstrange/embedded-postgres) — a real postgres
// downloaded and cached under the user cache dir on first use, no Docker
// required. Set FORGE_TEST_POSTGRES_URL to a base postgres DSN
// (postgres://user:pass@host:port/postgres?sslmode=disable) to point at
// an already-running server instead (a dev docker-compose, CI service
// container, or a detected local postgres) and skip the embedded boot.
package pgtest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	_ "github.com/lib/pq" // postgres driver, registered as "postgres"
)

// EnvBaseURL names the env var that, when set, points pgtest at an
// already-running postgres instead of booting embedded-postgres. The
// value is a base DSN whose database is "postgres" (the maintenance DB
// pgtest connects to in order to CREATE DATABASE).
const EnvBaseURL = "FORGE_TEST_POSTGRES_URL"

// server is the lazily-booted shared instance. Exactly one of embedded /
// baseURL is populated; baseDB is the maintenance connection used to
// create per-caller databases.
type server struct {
	baseURL  string
	embedded *embeddedpostgres.EmbeddedPostgres
	baseDB   *sql.DB
}

var (
	sharedOnce sync.Once
	shared     *server
	sharedErr  error
	dbCounter  atomic.Uint64
)

// freePort asks the OS for an unused TCP port. embedded-postgres wants a
// concrete port; binding :0 and reading it back avoids collisions when
// several processes boot instances concurrently.
func freePort() (uint32, error) {
	return reserveLoopbackPort()
}

// boot starts (or connects to) the shared postgres server. Idempotent
// via sharedOnce.
func boot() (*server, error) {
	sharedOnce.Do(func() {
		shared, sharedErr = startServer()
	})
	return shared, sharedErr
}

func startServer() (*server, error) {
	if base := os.Getenv(EnvBaseURL); base != "" {
		db, err := openBase(base)
		if err != nil {
			return nil, fmt.Errorf("pgtest: connect to %s=%q: %w", EnvBaseURL, base, err)
		}
		return &server{baseURL: base, baseDB: db}, nil
	}

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pgtest: reserve port: %w", err)
	}

	const (
		user = "forge"
		pass = "forge"
	)
	cfg := embeddedpostgres.DefaultConfig().
		Username(user).
		Password(pass).
		Database("postgres").
		Port(port).
		RuntimePath(runtimeDir(port)).
		// CachePath defaults under the user cache dir; the downloaded
		// binary is reused across runs. StartTimeout is generous for the
		// first run that has to extract the binary.
		StartTimeout(90 * time.Second)
	if cache := cacheDir(); cache != "" {
		cfg = cfg.CachePath(cache)
	}

	ep := embeddedpostgres.NewDatabase(cfg)
	if err := ep.Start(); err != nil {
		return nil, fmt.Errorf("pgtest: start embedded postgres: %w", err)
	}

	base := fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres?sslmode=disable", user, pass, port)
	db, err := openBase(base)
	if err != nil {
		_ = ep.Stop()
		return nil, fmt.Errorf("pgtest: connect to embedded postgres: %w", err)
	}
	return &server{baseURL: base, embedded: ep, baseDB: db}, nil
}

func openBase(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	// Pool against the maintenance DB stays tiny; it only runs
	// CREATE/DROP DATABASE.
	db.SetMaxOpenConns(4)
	if err := pingWithRetry(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func pingWithRetry(db *sql.DB) error {
	var err error
	for i := 0; i < 60; i++ {
		if err = db.Ping(); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

// runtimeDir keeps each embedded server's data directory unique per port
// so two processes never share one. It lives under the OS temp dir and
// is cleaned by Stop().
func runtimeDir(port uint32) string {
	return filepath.Join(os.TempDir(), "forge-pgtest", strconv.FormatUint(uint64(port), 10))
}

// cacheDir returns a stable directory for the downloaded postgres binary
// so it is fetched once and reused across runs. Falls back to "" (the
// library default) when no cache dir is resolvable.
func cacheDir() string {
	if c, err := os.UserCacheDir(); err == nil {
		return filepath.Join(c, "forge", "embedded-postgres")
	}
	return ""
}

// New creates a fresh, uniquely-named, empty database on the shared
// server and returns an open *sql.DB connected to it plus a cleanup
// function that closes the connection and drops the database. The first
// call boots the shared server (embedded download on the very first run
// of a new machine).
//
// Callers own the returned cleanup; tests typically defer it or register
// it with t.Cleanup. The connection is configured for postgres
// (database/sql driver "postgres").
func New() (*sql.DB, func(), error) {
	s, err := boot()
	if err != nil {
		return nil, nil, err
	}
	name := fmt.Sprintf("forge_test_%d_%d", os.Getpid(), dbCounter.Add(1))
	if _, err := s.baseDB.Exec("CREATE DATABASE " + name); err != nil {
		return nil, nil, fmt.Errorf("pgtest: create database %s: %w", name, err)
	}

	dsn := replaceDBName(s.baseURL, name)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		_, _ = s.baseDB.Exec("DROP DATABASE IF EXISTS " + name)
		return nil, nil, err
	}
	if err := pingWithRetry(db); err != nil {
		_ = db.Close()
		_, _ = s.baseDB.Exec("DROP DATABASE IF EXISTS " + name)
		return nil, nil, fmt.Errorf("pgtest: ping %s: %w", name, err)
	}

	cleanup := func() {
		_ = db.Close()
		// Terminate lingering backends so DROP DATABASE doesn't block.
		_, _ = s.baseDB.Exec(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = s.baseDB.Exec("DROP DATABASE IF EXISTS " + name)
	}
	return db, cleanup, nil
}

// replaceDBName swaps the database segment of a base DSN
// (".../postgres?...") for name.
func replaceDBName(baseURL, name string) string {
	// baseURL is "postgres://.../<db>?<query>"; swap the path segment.
	q := ""
	main := baseURL
	if i := lastIndexByte(baseURL, '?'); i >= 0 {
		main, q = baseURL[:i], baseURL[i:]
	}
	if i := lastIndexByte(main, '/'); i >= 0 {
		main = main[:i+1] + name
	}
	return main + q
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
