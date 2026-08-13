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
	"context"
	"database/sql"
	"fmt"
	"net/url"
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
	"github.com/lib/pq" // postgres driver (registered as "postgres") + typed errors
)

// EnvBaseURL names the env var that, when set, points pgtest at an
// already-running postgres instead of booting embedded-postgres. The
// value is a base DSN whose database is "postgres" (the maintenance DB
// pgtest connects to in order to CREATE DATABASE).
const EnvBaseURL = "FORGE_TEST_POSTGRES_URL"

// server is the lazily-booted shared instance for THIS process. baseURL is
// the maintenance DSN (an external FORGE_TEST_POSTGRES_URL server, or the
// cross-process shared embedded instance resolved via AcquireShared); baseDB
// is this process's maintenance connection used to create per-caller
// databases. release drops this process's pool reference on Shutdown.
type server struct {
	baseURL string
	baseDB  *sql.DB
	release func()
}

var (
	// sharedMu guards the lazy init below. It replaces a sync.Once, which
	// could not express "Shutdown returns us to the uninitialized state":
	// a Once stays fired forever, so a boot AFTER a Shutdown handed back the
	// gutted struct — non-nil server, nil baseDB — together with a NIL error,
	// and the first Exec in New segfaulted on it.
	//
	// The forge binary never hit this because main calls cli.Execute exactly
	// once and Execute DEFERS Shutdown, making "after Shutdown" identical to
	// "after the process exits". internal/tierguard breaks that equivalence
	// deliberately: it drives the whole pipeline in-process through repeated
	// cli.Execute() calls, so it pays the deferred Shutdown after every
	// simulated forge invocation and keeps going.
	sharedMu  sync.Mutex
	shared    *server
	sharedErr error
	dbCounter atomic.Uint64
)

// freePort asks the OS for an unused TCP port, in the uint32 shape
// embedded-postgres's Port option takes. Binding :0 and reading it back
// avoids collisions when several processes boot instances concurrently.
func freePort() (uint32, error) {
	p, err := ReserveLoopbackPort()
	if err != nil {
		return 0, err
	}
	return uint32(p), nil
}

// boot resolves this process's shared postgres server, acquiring exactly ONE
// cross-process pool reference (or one external-server connection) at a time,
// released by Shutdown.
//
// A live handle is reused; anything else re-acquires. That makes the sequence
// boot → Shutdown → boot correct rather than fatal, which is what an
// in-process embedder (internal/tierguard) needs and what a plain sync.Once
// could not give: the Once stayed fired after Shutdown and handed the next
// caller a closed handle with a nil error.
func boot() (*server, error) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared != nil && shared.baseDB != nil {
		return shared, sharedErr
	}
	shared, sharedErr = startServer()
	return shared, sharedErr
}

// startServer binds this process to a base postgres server via AcquireShared
// — an explicit FORGE_TEST_POSTGRES_URL, else the cross-process SHARED
// embedded instance — and opens the maintenance connection used to create
// per-caller scratch databases. It no longer boots a per-process embedded
// postgres directly: that is what made a parallel fan-out spin up N instances
// and exhaust the kernel IPC tables. See pool.go.
func startServer() (*server, error) {
	baseURL, release, err := AcquireShared()
	if err != nil {
		return nil, err
	}
	db, err := openBase(baseURL)
	if err != nil {
		release()
		return nil, fmt.Errorf("pgtest: connect to shared postgres: %w", err)
	}
	return &server{baseURL: baseURL, baseDB: db, release: release}, nil
}

// Shutdown releases this process's shared-server reference: it closes the
// maintenance connection and detaches from the cross-process pool. When this
// is the LAST live user of the shared embedded instance, the detach tears the
// server down cleanly (stop + remove data dir + release IPC). It is a cheap
// no-op when this process never opened a database, and is idempotent.
//
// The forge CLI defers Shutdown so every `forge generate` process cleans up
// its share on exit; ordinary `go test` binaries that never call it are
// backstopped by reapStaleInstances. (`forge test` used to be the other
// deferring caller. It is gone — projects run their suite through
// `task test` — so the per-package `go test` binaries a suite spawns now reach
// the pool directly, which is the path reapStaleInstances covers.)
func Shutdown() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared == nil {
		return
	}
	if shared.baseDB != nil {
		_ = shared.baseDB.Close()
		shared.baseDB = nil
	}
	if shared.release != nil {
		shared.release()
		shared.release = nil
	}
	// Drop the handle entirely, so the next boot re-acquires rather than
	// resurrecting a closed one. Leaving the pointer behind is what made
	// Shutdown a one-way door.
	shared = nil
	sharedErr = nil
}

// bootEmbedded starts a fresh embedded postgres and returns its maintenance
// DSN, port, and handle. Only the pool (attachEmbedded) calls it, under the
// pool lock; every other caller shares the instance it boots. It reaps
// orphaned instances first so a past crash's leftovers do not accumulate.
func bootEmbedded() (baseURL string, port uint32, ep *embeddedpostgres.EmbeddedPostgres, err error) {
	// Reap instances orphaned by SIGKILLed processes before booting a fresh
	// one — otherwise they accumulate across runs and exhaust the kernel's
	// shared-memory/semaphore tables (see reapStaleInstances).
	reapStaleInstances()

	port, err = freePort()
	if err != nil {
		return "", 0, nil, fmt.Errorf("pgtest: reserve port: %w", err)
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
		// mmap-backed shared memory instead of System V (shmget). The default
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

	ep = embeddedpostgres.NewDatabase(cfg)
	if err := ep.Start(); err != nil {
		return "", 0, nil, fmt.Errorf("pgtest: start embedded postgres: %w", err)
	}

	base := fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres?sslmode=disable", user, pass, port)
	// Verify reachability once here so a failed boot is reported now, not on
	// the first CREATE DATABASE. The caller opens its own maintenance pool.
	db, err := openBase(base)
	if err != nil {
		_ = ep.Stop()
		return "", 0, nil, fmt.Errorf("pgtest: connect to embedded postgres: %w", err)
	}
	_ = db.Close()
	return base, port, ep, nil
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
// boot fails with "initdb: exit status 1". Reaping is age-based in BOTH
// states: a live concurrent instance (recent postmaster.pid) is left
// untouched, and a dir with no postmaster is reaped only once it is
// older than staleInstanceAge — a young pid-less dir belongs to a
// sibling process mid-boot (archive extraction, initdb) and reaping it
// breaks that boot. Best-effort: every error is ignored.
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
		} else {
			// No live postmaster is NOT enough to reap: a sibling process
			// booting concurrently owns dirs with no postmaster.pid yet —
			// the embedded-postgres temp_* archive-extraction dir and the
			// freshly-created runtime dir mid-initdb. Reaping those
			// mid-boot broke the sibling's extraction ("rename …/temp_…:
			// no such file or directory") whenever two test binaries
			// booted pgtest at the same time. Only a dir that has SAT
			// there past the stale age is genuinely abandoned.
			if info, statErr := e.Info(); statErr != nil || time.Since(info.ModTime()) < staleInstanceAge {
				continue
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

	// Capture the maintenance handle rather than the server struct: a caller
	// that defers BOTH cleanup and Shutdown runs them in the natural order
	// Shutdown-then-cleanup, and reading s.baseDB then would dereference the
	// field Shutdown just nilled. Holding the *sql.DB directly means the worst
	// case is a scratch database left behind — which the pool teardown drops
	// anyway — instead of a panic in a deferred call.
	baseDB := s.baseDB
	cleanup := func() {
		_ = db.Close()
		if baseDB == nil {
			return
		}
		// Terminate lingering backends so DROP DATABASE doesn't block.
		_, _ = baseDB.Exec(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = baseDB.Exec("DROP DATABASE IF EXISTS " + name)
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

// NewAtURL creates a fresh, uniquely-named, empty database on the
// ALREADY-RUNNING postgres server addressed by baseURL and returns an
// open *sql.DB connected to it plus a cleanup that drops it. It is the
// "give me a scratch database on THIS server" primitive: unlike New it
// never boots embedded-postgres and never consults FORGE_TEST_POSTGRES_URL
// — the CALLER has already resolved which server to use (that policy lives
// above this package; pgtest stays a dumb ephemeral-postgres utility).
//
// baseURL is a DSN whose database is the maintenance DB (conventionally
// "postgres"): NewAtURL connects there to CREATE the scratch database and
// never touches whatever application database also lives on the server.
// The scratch database is dropped-if-exists BEFORE creating it, so a
// leftover from a crashed prior run (same pid+counter) can never wedge a
// fresh run; cleanup drops it again.
//
// Callers own the returned cleanup; the generate pipeline defers it.
func NewAtURL(baseURL string) (*sql.DB, func(), error) {
	base, err := openBase(baseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("pgtest: connect to %s: %w", redactDSN(baseURL), err)
	}
	name := fmt.Sprintf("forge_shadow_%d_%d", os.Getpid(), dbCounter.Add(1))
	// Drop-if-exists first: robust to a leftover database an earlier run
	// left behind after a crash before its cleanup could drop it.
	_, _ = base.Exec("DROP DATABASE IF EXISTS " + name)
	if _, err := base.Exec("CREATE DATABASE " + name); err != nil {
		_ = base.Close()
		return nil, nil, fmt.Errorf("pgtest: create database %s: %w", name, err)
	}

	dropAndClose := func() {
		_, _ = base.Exec(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = base.Exec("DROP DATABASE IF EXISTS " + name)
		_ = base.Close()
	}

	dsn := replaceDBName(baseURL, name)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		dropAndClose()
		return nil, nil, err
	}
	if err := pingWithRetry(db); err != nil {
		_ = db.Close()
		dropAndClose()
		return nil, nil, fmt.Errorf("pgtest: ping %s: %w", name, err)
	}

	cleanup := func() {
		_ = db.Close()
		dropAndClose()
	}
	return db, cleanup, nil
}

// EnsureDatabase makes the application database named in appDSN exist,
// idempotently — the RUNTIME counterpart to NewAtURL's throwaway scratch
// database. Where NewAtURL creates a uniquely-named database and DROPS it on
// cleanup (generate-time schema introspection), EnsureDatabase creates the ONE
// named database the app will actually run against and NEVER drops it: a fresh
// `forge run` against a scaffolded dev DSN would otherwise die with
// `FATAL: database "<project>" does not exist (SQLSTATE 3D000)` because nothing
// had issued CREATE DATABASE.
//
// It reuses the SAME maintenance-DB mechanism as NewAtURL: appDSN is reduced to
// its SERVER coordinates against the maintenance database "postgres" (the app's
// own database is never opened), a short maintenance connection is opened there
// (openBase), and CREATE DATABASE is issued only when the target is absent. A
// concurrent creator winning the race between the existence check and the
// CREATE (duplicate_database, SQLSTATE 42P04) is a no-op, not an error — the
// post-condition (the database exists) holds either way.
//
// The maintenance connection FAILING (server down, wrong credentials) is a HARD
// error: the app cannot boot without it, so the caller should surface it loudly
// rather than let the app fail later with an opaque connect error. appDSN that
// will not parse, names no host, or names no database is likewise an error.
func EnsureDatabase(appDSN string) error {
	name, server, err := splitAppDSN(appDSN)
	if err != nil {
		return err
	}
	base, err := openBase(server)
	if err != nil {
		return fmt.Errorf("pgtest: connect to %s: %w", redactDSN(server), err)
	}
	defer func() { _ = base.Close() }()

	var exists bool
	if err := base.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name,
	).Scan(&exists); err != nil {
		return fmt.Errorf("pgtest: check database %q: %w", name, err)
	}
	if exists {
		return nil
	}
	// CREATE DATABASE takes no parameters, so the name is interpolated; quote it
	// as an identifier so a name that is not a bare identifier (a project named
	// "control-plane", say) is created verbatim rather than parsed as SQL.
	if _, err := base.Exec("CREATE DATABASE " + quoteIdent(name)); err != nil {
		if isDuplicateDatabaseErr(err) {
			return nil // concurrent creator won the race — post-condition holds
		}
		return fmt.Errorf("pgtest: create database %q: %w", name, err)
	}
	return nil
}

// splitAppDSN splits an application DSN into its database NAME and the
// maintenance SERVER DSN (same scheme/credentials/host/port, database forced to
// "postgres" and sslmode defaulted to disable when absent). It mirrors the
// contract NewAtURL states for its baseURL — connect to the maintenance DB to
// create ANOTHER database — but derives BOTH the target name and the
// maintenance URL from one app DSN. A DSN that will not parse, names no host,
// or names no database yields an error.
func splitAppDSN(appDSN string) (name, server string, err error) {
	u, err := url.Parse(appDSN)
	if err != nil {
		return "", "", fmt.Errorf("pgtest: parse dsn %s: %w", redactDSN(appDSN), err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("pgtest: dsn %s names no host", redactDSN(appDSN))
	}
	name = strings.TrimPrefix(u.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("pgtest: dsn %s names no database", redactDSN(appDSN))
	}
	maint := *u
	maint.Path = "/postgres"
	q := maint.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "disable")
		maint.RawQuery = q.Encode()
	}
	return name, maint.String(), nil
}

// quoteIdent double-quotes a postgres identifier, doubling any embedded double
// quote per postgres' quoting rules, so a database name that is not a bare
// lowercase identifier is created verbatim.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// isDuplicateDatabaseErr reports whether err is postgres' duplicate_database
// (SQLSTATE 42P04) — a concurrent creator won the race between EnsureDatabase's
// existence check and its CREATE DATABASE. lib/pq returns a raw *pq.Error from
// Exec (unwrapped), so a direct type assertion suffices.
func isDuplicateDatabaseErr(err error) bool {
	if pqErr, ok := err.(*pq.Error); ok {
		return pqErr.Code.Name() == "duplicate_database"
	}
	return false
}

// CanReach reports whether a postgres server is reachable at baseURL with
// working credentials — a fast open/ping/close with a short timeout. It
// creates and mutates NOTHING; a malformed URL or any connect/ping failure
// (server down, wrong password, missing role) returns false. Callers use
// it to pick among candidate servers before committing one to NewAtURL, so
// an unreachable or wrong-credential candidate is skipped rather than
// turned into a hard CREATE DATABASE error.
func CanReach(baseURL string) bool {
	db, err := sql.Open("postgres", withConnectTimeout(baseURL, 2))
	if err != nil {
		return false
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return db.PingContext(ctx) == nil
}

// withConnectTimeout appends a lib/pq connect_timeout (seconds) to a DSN so
// a probe against a dead host fails fast instead of hanging on the OS TCP
// timeout. Appended unconditionally — a duplicate param is harmless (pq
// takes the last).
func withConnectTimeout(dsn string, seconds int) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%sconnect_timeout=%d", dsn, sep, seconds)
}

// redactDSN masks the password in a DSN for safe inclusion in error
// messages. A DSN that will not parse is reported opaquely.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "postgres://<unparseable-dsn>"
	}
	if u.User != nil {
		if _, hasPw := u.User.Password(); hasPw {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	return u.String()
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
