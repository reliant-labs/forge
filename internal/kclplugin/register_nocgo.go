//go:build !cgo

package kclplugin

// Register is a no-op without CGO: KCL's plugin bridge requires cgo, so a
// CGO-free build cannot service kcl_plugin.forge.* calls. forge's
// distributed binaries are built with CGO; this stub only keeps CGO-free
// `go build` / `go vet` working for contributors and partial CI.
func Register() {}

// Available reports whether this binary can service kcl_plugin.forge.*
// calls — always false here, because Register above is a no-op.
//
// Without this probe the no-op Register is SILENT: the binary installs,
// reports a correct --version, and passes generate/lint/build, then fails
// every render with a KCL-level "plugin package not found" that names
// neither CGO nor the fix. Callers check Available and refuse up front.
func Available() bool { return false }
