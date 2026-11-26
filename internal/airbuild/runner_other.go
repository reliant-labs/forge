//go:build !linux && !darwin

package airbuild

import "errors"

// getpgrp has no answer forge reads here; CurrentRunner reports no runner, so
// a failed build exits with its status at once (air's own behaviour).
func getpgrp() int { return 0 }

func lookupProc(int) (procInfo, bool) { return procInfo{}, false }

// execWaiter has no process table to wait on here; the caller holds
// in-process.
func execWaiter(string, int) error { return errors.New("not supported on this platform") }
