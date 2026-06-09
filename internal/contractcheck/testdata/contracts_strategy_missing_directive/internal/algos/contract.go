// Same shape as contracts_strategy/internal/algos but WITHOUT the
// `//forge:strategy` directive. The lint should fire Service/Deps/New
// findings — we don't auto-detect the shape.
package algos

import "context"

type Strategy interface {
	Name() string
	Run(ctx context.Context, input []float64) (float64, error)
}

var Registry = map[string]Strategy{}

func Register(s Strategy) {
	Registry[s.Name()] = s
}
