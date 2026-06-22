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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

	// Reap instances orphaned by SIGKILLed test binaries before booting a
	// fresh one — otherwise they accumulate across runs and exhaust the
	// kernel's shared-memory/semaphore tables (see reapStaleInstances).
	reapStaleInstances()

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
		// Shrink the per-instance footprint and — critically — use
		// mmap-backed shared memory instead of System V (shmget). The e2e
		// corpus boots many instances concurrently (every `forge generate`
		// subprocess spins one up for schema introspection); the default
		// sysv shared memory exhausts the kernel's SHMMNI limit on macOS
		// ("could not create shared memory segment: No space left on
		// device"). mmap avoids the sysv segment table entirely. fsync=off
		// is safe — these databases are ephemeral and dropped after use.
		StartParameters(map[string]string{
			"shared_buffers": "32MB",
			// One shared server fans out to many per-call databases AND the
			// generated bootstrap pools ~25 connections; keep the ceiling
			// generous so the parallel corpus never starves.
			"max_connections":            "200",
			"dynamic_shared_memory_type": "mmap",
			"shared_memory_type":         "mmap",
			"fsync":                      "off",
			"synchronous_commit":         "off",
			"full_page_writes":           "off",
		}).
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

// staleInstanceAge is how old an embedded-postgres instance must be
// before reapStaleInstances treats it as abandoned. Real gate runs
// finish in a couple of minutes (the test -timeout is a far-off
// ceiling, not the runtime), so anything this old belongs to a process
// that died before it could clean up. Generous enough that a live
// concurrent run is never reaped.
const staleInstanceAge = 30 * time.Minute

// reapStaleInstances removes forge-pgtest runtime dirs — and SIGKILLs any
// postgres still bound to them — left behind by test binaries that were
// killed before t.Cleanup/Stop could run. When a test process dies hard,
// its embedded postgres child is reparented to init and keeps running;
// across many concurrent gate runs these orphans pile up and exhaust the
// kernel's SysV shared-memory/semaphore tables, after which EVERY new
// boot fails with "initdb: exit status 1". Reaping is age-based off each
// instance's postmaster.pid start time, so a live concurrent instance
// (recently started) is left untouched; a dead postmaster (pid gone) is
// reaped regardless of age. Best-effort: every error is ignored.
//
// Each reap also reclaims the instance's leaked SysV shared-memory segment
// (reclaimShmSegment) BEFORE removing its dir — the segment is the resource
// that actually exhausts; the dir is just where its id is recorded. Removing
// the dir first (as this used to) deleted the pid file and orphaned the
// segment permanently, so segments piled up even though dirs were cleaned.
func reapStaleInstances() {
	root := filepath.Join(os.TempDir(), "forge-pgtest")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		pidFile := filepath.Join(dir, "data", "postmaster.pid")
		pid, alive := postmaster(pidFile)
		if alive {
			// Only reap a still-running postmaster once it's clearly orphaned
			// (older than staleInstanceAge). Recently-started ones belong to
			// live concurrent runs.
			if info, statErr := os.Stat(pidFile); statErr != nil || time.Since(info.ModTime()) < staleInstanceAge {
				continue
			}
			if proc, ferr := os.FindProcess(pid); ferr == nil {
				_ = proc.Signal(syscall.SIGKILL)
			}
		}
		// Reclaim the leaked shm segment while the pid file (which names it)
		// still exists — must precede RemoveAll.
		reclaimShmSegment(pidFile)
		_ = os.RemoveAll(dir)
	}
}

// shmIDFromPidfile parses the System V shared-memory segment id postgres
// records in its postmaster.pid lock file. Postgres writes "<shmkey> <shmid>"
// on the 7th line (1-based; LOCK_FILE_LINE_SHMEM_KEY) precisely so a
// replacement postmaster can detect and remove a stale segment — we reuse
// that contract on reap. Returns ok=false when the line is absent (a pid file
// written before shmem attached), the fields don't parse, or the id is
// non-positive (no SysV segment recorded).
func shmIDFromPidfile(content string) (int, bool) {
	lines := strings.Split(content, "\n")
	const shmemLine = 6 // 0-based index of the 7th line
	if len(lines) <= shmemLine {
		return 0, false
	}
	fields := strings.Fields(lines[shmemLine])
	if len(fields) < 2 {
		return 0, false
	}
	id, err := strconv.Atoi(fields[1])
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// reclaimShmSegment best-effort removes the SysV shared-memory segment named
// in pidFile. Even with shared_memory_type=mmap, postgres always creates one
// tiny SysV interlock segment per instance; a postmaster that was SIGKILLed
// or died with its test-binary parent never releases it, and these orphans
// exhaust the kernel SHMMNI table (macOS default 32) until every initdb fails
// with "could not create shared memory segment: No space left on device".
// `ipcrm` exists on macOS and Linux; anywhere else this is a harmless no-op.
func reclaimShmSegment(pidFile string) {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	id, ok := shmIDFromPidfile(string(b))
	if !ok {
		return
	}
	// ipcrm -m marks the segment for removal (freed once the last attached
	// process detaches). Best-effort: a missing ipcrm / already-gone id is
	// ignored.
	_ = exec.Command("ipcrm", "-m", strconv.Itoa(id)).Run()
}

// postmaster reads a postmaster.pid file and reports the server PID and
// whether that process is currently alive (signal 0 probe). Returns
// (0, false) when the file is absent/unreadable — a crashed instance
// whose dir lingers, which the caller reaps unconditionally.
func postmaster(pidFile string) (pid int, alive bool) {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	first := string(b)
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	pid, err = strconv.Atoi(strings.TrimSpace(first))
	if err != nil || pid <= 0 {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return pid, false
	}
	return pid, proc.Signal(syscall.Signal(0)) == nil
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

// NewURL creates a fresh, uniquely-named, empty database on the shared
// server like New, but returns its connection DSN (a postgres:// URL)
// instead of an open *sql.DB. Use this when the consumer is a separate
// process that connects itself via DATABASE_URL — e.g. an e2e test that
// boots a generated server. The returned cleanup drops the database; the
// caller must not hold connections past it.
func NewURL() (dsn string, cleanup func(), err error) {
	s, err := boot()
	if err != nil {
		return "", nil, err
	}
	name := fmt.Sprintf("forge_test_%d_%d", os.Getpid(), dbCounter.Add(1))
	if _, err := s.baseDB.Exec("CREATE DATABASE " + name); err != nil {
		return "", nil, fmt.Errorf("pgtest: create database %s: %w", name, err)
	}
	cleanup = func() {
		_, _ = s.baseDB.Exec(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = s.baseDB.Exec("DROP DATABASE IF EXISTS " + name)
	}
	return replaceDBName(s.baseURL, name), cleanup, nil
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
