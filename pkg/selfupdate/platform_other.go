//go:build !unix && !windows

package selfupdate

// Every target that is neither unix nor windows can stage a release but not
// swap it.

func swapSupported() bool { return false }

// runningImageLocked: nothing is ever swapped here.
const runningImageLocked = false

func execBinary(string, []string, []string) error { return ErrUnsupported }

func isTextBusy(error) bool { return false }

// processAlive is conservative where it cannot ask: a boot it cannot see
// die is never counted as a crash.
func processAlive(int) bool { return true }

func syncDir(string) error { return nil }
