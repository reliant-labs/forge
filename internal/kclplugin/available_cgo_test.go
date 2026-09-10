//go:build cgo

package kclplugin

import "kcl-lang.io/kcl-go/pkg/plugin"

// namespaceRegistered asks KCL's own registry whether a plugin is
// installed under the "forge" name — the ground truth Available claims to
// report. kcl-go's plugin package is itself //go:build cgo, so this probe
// only exists in the CGO half; see available_nocgo_test.go for the other.
func namespaceRegistered() bool {
	_, ok := plugin.GetPlugin("forge")
	return ok
}
