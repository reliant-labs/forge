// Clean fixture: interactor's Deps are all interfaces. Lint must not fire.

// forge:interactor
//
// billingflow declares its Charger dep as an interface in contract.go;
// adapters that satisfy this shape get wired in pkg/app/setup.go.
package billingflow

import "context"

type Service interface {
	Run(ctx context.Context) error
}

type Charger interface {
	Charge(ctx context.Context, userID string, amount int64) (string, error)
}
