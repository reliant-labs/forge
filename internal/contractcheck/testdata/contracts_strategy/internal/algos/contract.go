// Strategy-registry package. The `//forge:strategy` directive opts
// the package out of the canonical Service/Deps/New enforcement.
//
//forge:strategy
package algos

import "context"

// Strategy is the package's polymorphic interface. Each registered
// algorithm implements it.
type Strategy interface {
	Name() string
	Run(ctx context.Context, input []float64) (float64, error)
}

// Registry holds the available strategies, keyed by name.
var Registry = map[string]Strategy{}

// Register adds a strategy to the registry. Called from each impl's init().
func Register(s Strategy) {
	Registry[s.Name()] = s
}
