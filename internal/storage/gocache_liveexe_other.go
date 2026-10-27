//go:build !linux

package storage

import (
	"context"
	"errors"
)

// procExecutables needs /proc/<pid>/exe, which only Linux has. The error makes
// the trim keep every executable entry (gocache_liveexe.go): a platform that
// cannot say which binaries are running must not delete them.
func procExecutables(context.Context, string) ([]LiveExecutable, error) {
	return nil, errors.New("no /proc on this platform")
}
