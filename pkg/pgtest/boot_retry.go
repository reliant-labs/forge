package pgtest

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

const embeddedBootAttempts = 4

// portBootError preserves the last collision while reporting the bounded set
// of candidates. Internal: callers need no new public API to recover from it.
type portBootError struct {
	ports []uint32
	err   error
}

func (e *portBootError) Error() string {
	return fmt.Sprintf("port collisions exhausted %d embedded postgres boot attempts (ports %v); check concurrent listeners or set %s to an existing test server: %v", len(e.ports), e.ports, EnvBaseURL, e.err)
}
func (e *portBootError) Unwrap() error { return e.err }

// startEmbeddedOnFreshPort retries only a confirmed bind collision. Dependencies
// are per-call so deterministic tests never replace process-global functions.
// Config is copied unchanged except for the port and its runtime directory.
func startEmbeddedOnFreshPort(cfg embeddedpostgres.Config, reserve func() (uint32, error), dirFor func(uint32) string, start func(embeddedpostgres.Config) (*Embedded, error)) (*Embedded, uint32, error) {
	var ports []uint32
	var last error
	for range embeddedBootAttempts {
		port, err := reserve()
		if err != nil {
			return nil, 0, fmt.Errorf("pgtest: reserve port: %w", err)
		}
		ports = append(ports, port)
		dir := dirFor(port)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, 0, fmt.Errorf("pgtest: create runtime parent: %w", err)
		}
		// The dependency unconditionally RemoveAlls RuntimePath before
		// extraction. Never give it a directory somebody else already owns,
		// even if the port currently looks free (e.g. a sibling mid-boot).
		if err := os.Mkdir(dir, 0o755); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return nil, 0, fmt.Errorf("pgtest: claim runtime directory: %w", err)
			}
			last = fmt.Errorf("runtime directory %s already claimed: %w", dir, err)
			continue
		}
		ep, err := start(cfg.Port(port).RuntimePath(dir))
		if err == nil {
			return ep, port, nil
		}
		// No Stop/stopSharedByPort here: the listener that won the race is
		// NOT ours. StartEmbedded owns its failed-start logs. Remove only an
		// empty claim; partial extraction/data is left intact, never swept.
		_ = os.Remove(dir)
		if !embeddedBindCollision(err, port, dir) {
			return nil, 0, err
		}
		last = err
	}
	return nil, 0, &portBootError{ports: ports, err: last}
}

func embeddedBindCollision(err error, port uint32, dir string) bool {
	// Do not restart after an uncertain partial boot. Even a pid file whose
	// process is invisible here could belong to a still-running postmaster.
	if _, statErr := os.Stat(filepath.Join(dir, "data", "postmaster.pid")); !errors.Is(statErr, os.ErrNotExist) {
		return false
	}
	if err.Error() == fmt.Sprintf("process already listening on port %d", port) {
		// Upstream discards Listen's cause, so its wording alone also covers
		// permission/socket exhaustion errors. Confirm EADDRINUSE, not merely
		// the presence of an arbitrary startup error or an answering port.
		ln, listenErr := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
		if listenErr == nil {
			_ = ln.Close()
		}
		return errors.Is(listenErr, syscall.EADDRINUSE)
	}
	// The second race is after upstream's precheck, during pg_ctl start.
	// Require both PostgreSQL's bind diagnostic and its terminal TCP failure,
	// not a generic "address" substring (download/initdb errors aren't binds).
	text := err.Error()
	if !strings.HasPrefix(text, "could not start postgres using ") || !strings.Contains(text, "FATAL:  could not create any TCP/IP sockets") {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "could not bind IPv") && (strings.Contains(line, "Address already in use") || strings.Contains(line, "Only one usage of each socket address")) {
			return true
		}
	}
	return false
}
