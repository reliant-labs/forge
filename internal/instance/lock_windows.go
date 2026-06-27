//go:build windows

package instance

import "os"

// lockFD / unlockFD are no-ops on Windows: forge's dev/up loop targets
// Unix hosts, and the registry write is already atomic via temp-file +
// rename. The exclusive lock matters only for the concurrent first-`up`
// race, which is a Unix-host concern in practice. If Windows multi-worktree
// support is ever needed, replace these with LockFileEx.
func lockFD(*os.File) error   { return nil }
func unlockFD(*os.File) error { return nil }
