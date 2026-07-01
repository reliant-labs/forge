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
				// allocate_port(base, key) -> int. Deterministic, memoized
				// keyed port for parallel dev stacks: base + block(key)*100,
				// where forge assigns + persists a stable block per key (the
				// index is internal, never surfaced here). key "" -> base.
				"allocate_port": {
					Body: func(args *plugin.MethodArgs) (*plugin.MethodResult, error) {
						base := int(args.IntArg(0))
						key := args.StrArg(1)
						p, err := allocatePort(base, key)
						if err != nil {
							return nil, err
						}
						return &plugin.MethodResult{V: p}, nil
					},
				},
				// derive_jwk(private_key_pem, kid, alg) -> JWK dict. Derives
				// the PUBLIC JWK from an ES256 private-key PEM at render time
				// so a forge.TestJWKS publishes the public half of the EXACT
				// key its signer uses — signer + JWKS can't drift.
				"derive_jwk": {
					Body: func(args *plugin.MethodArgs) (*plugin.MethodResult, error) {
						pemStr := args.StrArg(0)
						kid := args.StrArg(1)
						alg := args.StrArg(2)
						jwk, err := DeriveES256JWK(pemStr, kid, alg)
						if err != nil {
							return nil, err
						}
						return &plugin.MethodResult{V: jwk}, nil
					},
				},
			},
		})
	})
}
