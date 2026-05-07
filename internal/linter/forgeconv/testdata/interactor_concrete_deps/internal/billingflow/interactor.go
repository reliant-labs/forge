package billingflow

import (
	"context"
	"log/slog"
)

// stripeAdapter is a stand-in for a concrete adapter struct. The lint
// rule fires on the `Charger *stripeAdapter` field below — a concrete
// pointer in Deps defeats unit-testing the workflow with mocks.
type stripeAdapter struct{}

func (s *stripeAdapter) Charge(_ context.Context, _ string, _ int64) (string, error) {
	return "", nil
}

// Deps fires the rule: Charger is a concrete struct pointer.
type Deps struct {
	Logger  *slog.Logger
	Charger *stripeAdapter // SHOULD be an interface
}

type service struct{ deps Deps }

func New(deps Deps) Service { return &service{deps: deps} }

func (s *service) Run(ctx context.Context) error {
	_, err := s.deps.Charger.Charge(ctx, "u1", 100)
	return err
}
