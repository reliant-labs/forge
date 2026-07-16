//go:build !darwin && !linux

package cli

// readProcEnviron has no portable implementation outside darwin/linux
// (Windows, etc.). It always reports "unreadable", so ownership resolution
// degrades to "unidentifiable → treated as foreign" — the forge dev loop
// (`forge up`) is Unix-first, and this keeps the build green elsewhere
// without ever misclassifying a process as reclaimable.
func readProcEnviron(_ int) ([]string, bool) {
	return nil, false
}
