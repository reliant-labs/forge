//go:build !cgo

package kclplugin

// Register is a no-op without CGO: KCL's plugin bridge requires cgo, so a
// CGO-free build cannot service kcl_plugin.forge.* calls. forge's
// distributed binaries are built with CGO; this stub only keeps CGO-free
// `go build` / `go vet` working for contributors and partial CI.
func Register() {}
