// Fires forgeconv-interactor-deps-are-interfaces. The package opts
// into the interactor convention but Deps carries a concrete pointer
// instead of an interface — exactly the foot-gun the rule catches.

// forge:interactor
//
// billingflow is an interactor whose Deps holds a concrete pointer to
// an adapter struct, defeating the all-mock-test surface.
package billingflow

import "context"

type Service interface {
	Run(ctx context.Context) error
}
