package cli

import (
	"os"
	"sync"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/forgedelegate"
)

// A forge started by `go run …@<pin>` runs from the Go build cache
// (<GOCACHE>/xx/<hash>-d/forge). That entry is not the process's to keep: a
// trim or `go clean -cache` removes it while forge is alive — typically while
// it queues ~10 minutes for a heavy slot. Every place forge names its own path
// (the protoc-gen-forge plugin handed to buf, the generate fingerprint, a
// re-exec) then fails with ENOENT although the process is fine.
//
// selfExecutable is the one answer to "where is my own executable?". For a
// `go run` build it files a durable copy under the pinned-forge cache once, at
// startup, and names that; for every other forge it is os.Executable.

// Seams for tests.
var (
	selfExecutableSource = os.Executable
	selfVersion          = buildinfo.Version
)

var selfExe struct {
	mu   sync.Mutex
	done bool
	path string
	err  error
}

// resetSelfExecutable forgets the memoized answer (tests only).
func resetSelfExecutable() {
	selfExe.mu.Lock()
	selfExe.done, selfExe.path, selfExe.err = false, "", nil
	selfExe.mu.Unlock()
}

// selfExecutable returns a path to this forge's executable that stays valid
// for the life of the process. The first call decides and the answer is kept,
// so the copy is made at startup (Execute) before anything can wait. A copy
// that cannot be filed (read-only cache dir, a dev build with no version to
// key it by) falls back to the executable's own path: today's behaviour.
func selfExecutable() (string, error) {
	selfExe.mu.Lock()
	defer selfExe.mu.Unlock()
	if selfExe.done {
		return selfExe.path, selfExe.err
	}
	selfExe.done = true
	exe, err := selfExecutableSource()
	if err != nil {
		selfExe.err = err
		return "", err
	}
	selfExe.path = exe
	if !goRunExecutable(exe) {
		return exe, nil
	}
	version := selfVersion()
	if forgeIdentity(version) == "" {
		return exe, nil
	}
	if durable, err := (&forgedelegate.Resolver{}).Adopt(version, exe); err == nil {
		selfExe.path = durable
	}
	return selfExe.path, nil
}
