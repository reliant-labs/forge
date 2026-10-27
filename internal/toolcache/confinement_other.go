//go:build !linux

package toolcache

import (
	"fmt"
	"os"
)

func openLockNoFollow(_ *os.File, name string) (*os.File, error) {
	return nil, fmt.Errorf("retain %s: no-follow lock opens unavailable", name)
}

// No destructive fallback: until a platform supplies no-follow, no-mount
// descriptor-relative opens, uncertain trees are retained unchanged.
func openDirectoryNoFollow(_ *os.File, name string, _ bool) (*os.File, error) {
	return nil, fmt.Errorf("retain %s: confined no-follow directory opens unavailable on this platform", name)
}
