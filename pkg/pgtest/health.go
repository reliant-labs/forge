package pgtest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A shared server can be ALIVE and still BROKEN: its postmaster answers, but
// the data directory it runs from has been deleted underneath it (a /tmp
// cleaner aging out template1's files, a reaper or a person removing the
// runtime dir). Every CREATE DATABASE then fails with SQLSTATE 58P01
// (`could not open file "base/1/pg_filenode.map"`), for every process that
// shares the server, until someone kills it by hand: probeShared saw a live
// postmaster that answered a ping and called it ready. On 2026-11-07 this
// turned a release cut red (control-plane refs/rc/sets/v1.9.0-rc.18).
//
// These are the checks that tell the two apart, and the stop that ends a
// broken server even when its pid file is gone with the rest of its data.

// dataIntact reports why the data directory of the server at runtimeDir(port)
// cannot serve a CREATE DATABASE, or nil when it can. It checks the files
// every new database needs: the cluster's version stamp, the shared catalog
// map, and template1's (OID 1, the default template) relation map.
func dataIntact(port uint32) error { return dataIntactAt(runtimeDir(port)) }

// dataIntactAt is dataIntact for the runtime dir dir.
func dataIntactAt(dir string) error {
	data := filepath.Join(dir, "data")
	for _, rel := range []string{"PG_VERSION", "global/pg_filenode.map", "base/1/pg_filenode.map"} {
		if _, err := os.Stat(filepath.Join(data, rel)); err != nil {
			return fmt.Errorf("data directory %s lost %s: %w", data, rel, err)
		}
	}
	return nil
}

// cwdDeleted reports whether pid runs in a directory that no longer exists:
// the postmaster chdirs into its data directory, so a deleted one shows as
// "<path> (deleted)" in /proc/<pid>/cwd. Linux only; elsewhere it reports
// false and dataIntact is the check.
func cwdDeleted(pid int) bool {
	if runtime.GOOS != "linux" || pid <= 0 {
		return false
	}
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	return err == nil && strings.HasSuffix(cwd, " (deleted)")
}

// isOurPostgres reports whether pid is a postgres running the binary the
// runtime dir at port extracted. Unlike verifyRecordedPid it needs no pid
// file (it may have gone with the data directory) and reads no start time
// (btime moves when the wall clock is stepped); the executable path alone
// names a binary only this runtime dir holds. Unreadable facts are false:
// never signal what cannot be identified.
func isOurPostgres(pid int, port uint32) bool { return isPostgresOf(pid, runtimeDir(port)) }

// isPostgresOf is isOurPostgres for the runtime dir dir.
func isPostgresOf(pid int, dir string) bool {
	if pid <= 0 || !processAlive(pid) {
		return false
	}
	f := readProcFacts(pid)
	return f.exeOK && samePath(f.exe, postgresBinary(dir))
}

// stopPid asks the postgres at pid for a fast shutdown, waits up to 5s, then
// kills it. It reports whether the process is gone.
func stopPid(pid int) bool {
	if gracefulStop(pid) {
		for i := 0; i < 50 && processAlive(pid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if processAlive(pid) {
		forceKill(pid)
		for i := 0; i < 20 && processAlive(pid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return !processAlive(pid)
}

// sharedBrokenReason reports why the live shared server recorded in st is
// unusable, or "" when nothing is known to be wrong with it. pid is the
// postmaster from its pid file (0 when that is gone too).
func sharedBrokenReason(st poolState, pid int) string {
	if pid == 0 && st.PID > 0 && isOurPostgres(st.PID, st.Port) {
		return fmt.Sprintf("postmaster %d is running but its pid file is gone", st.PID)
	}
	if pid == 0 {
		return ""
	}
	if err := dataIntact(st.Port); err != nil {
		return err.Error()
	}
	if cwdDeleted(pid) {
		return fmt.Sprintf("postmaster %d runs in a deleted directory", pid)
	}
	return ""
}

// stopBroken ends a broken shared server: the postmaster recorded at boot,
// by pid, once it is proven to be this runtime dir's postgres (its pid file
// may have gone with the data, so pg_ctl cannot reach it), then the orderly
// stop by data dir and the runtime dir's removal (stopSharedByPort).
func stopBroken(st poolState) {
	if isOurPostgres(st.PID, st.Port) {
		stopPid(st.PID)
	}
	stopSharedByPort(st.Port)
}

// brokenServerError reports whether err is the shared server failing for a
// reason a fresh server fixes: a file of its data directory missing (58P01
// undefined_file), or Postgres reporting it is shutting down (57P01) or not
// accepting connections (57P03).
func brokenServerError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "58P01", // undefined_file: the data directory lost a file
		"57P01", // admin_shutdown
		"57P03": // cannot_connect_now
		return true
	}
	return false
}

// healShared drops this process's binding to the shared server after a
// create failed with brokenServerError, so the next boot re-attaches: under
// the pool lock, attachEmbedded's probe then sees the broken server, stops
// it and boots a fresh one. It reports whether a retry is worth making (the
// server is the pooled embedded one, not an external FORGE_TEST_POSTGRES_URL).
func healShared() bool {
	localMu.Lock()
	external := poolMode == poolModeExternal && poolURL != ""
	localMu.Unlock()
	if external || strings.TrimSpace(os.Getenv(EnvBaseURL)) != "" {
		return false
	}
	Shutdown()
	return true
}
