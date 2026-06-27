package instance

import "sync"

// The active instance is a process-global, set ONCE at the start of a
// forge command (forge up / forge deploy) and read by every render path —
// both the entity render (renderKCLRaw) and the manifest render
// (cluster.renderDArgs). It mirrors kclplugin.UsePortStore: the render
// functions have a fixed signature called from ~20 sites, so a global is
// how forge already threads per-command render context (the port store)
// without churning every call site.
//
// The zero value is the default instance, so any command that never calls
// SetActive (forge ci, forge audit, tests) renders byte-identically to
// today — no instance options, default port store.
var (
	activeMu sync.RWMutex
	active   Instance
)

// SetActive records the instance for this process's subsequent renders.
// Call once, before the first render, on the up/deploy path only.
func SetActive(i Instance) {
	activeMu.Lock()
	active = i
	activeMu.Unlock()
}

// Active returns the instance set by SetActive (default when unset).
func Active() Instance {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return active
}

// ActiveDArgs is the convenience the render paths call: the `-D` bindings
// to push the active instance into KCL (nil for the default instance).
func ActiveDArgs() []string { return Active().DArgs() }
