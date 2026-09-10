//go:build !cgo

package kclplugin

// namespaceRegistered is false without CGO by construction: kcl-go's
// plugin package is //go:build cgo, so there is no registry to consult and
// nothing could have been installed into one. Register is the no-op stub
// in register_nocgo.go, and Available must report that honestly.
func namespaceRegistered() bool { return false }
