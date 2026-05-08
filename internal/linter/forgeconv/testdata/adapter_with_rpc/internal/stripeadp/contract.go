// Fires the forgeconv-adapter-no-rpc rule. The package opts into the
// adapter convention via `// forge:adapter`, then registers a Connect
// RPC handler — exactly the foot-gun the rule catches.

// forge:adapter
//
// stripeadp is an adapter that has, regretfully, also become a Connect
// RPC service. The lint rule should flag adapter.go's call to
// connect.NewBillingHandler.
package stripeadp

import "context"

type Service interface {
	HealthCheck(ctx context.Context) error
}
