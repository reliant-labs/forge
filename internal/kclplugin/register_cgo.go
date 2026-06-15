//go:build cgo

package kclplugin

import (
	"sync"

	"kcl-lang.io/kcl-go/pkg/plugin"
)

var registerOnce sync.Once

// Register installs the kcl_plugin.forge namespace into the process-global
// KCL plugin registry. Idempotent and safe to call before every render.
//
// Requires CGO — KCL's plugin bridge is //go:build cgo. forge ships
// prebuilt CGO binaries (homebrew + release pipeline), so the namespace is
// always available in distribution. The CGO-free build gets the no-op
// Register in register_nocgo.go.
func Register() {
	registerOnce.Do(func() {
		plugin.RegisterPlugin(plugin.Plugin{
			Name: "forge",
			MethodMap: map[string]plugin.MethodSpec{
				// resolve_port(name, preferred) -> int. One stable host port
				// per name, preferring `preferred` when free.
				"resolve_port": {
					Body: func(args *plugin.MethodArgs) (*plugin.MethodResult, error) {
						name := args.StrArg(0)
						preferred := int(args.IntArg(1))
						p, err := defaultResolver.Resolve(name, preferred)
						if err != nil {
							return nil, err
						}
						return &plugin.MethodResult{V: p}, nil
					},
				},
			},
		})
	})
}
