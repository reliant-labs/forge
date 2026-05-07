package billingflow

import (
	"context"
	"log/slog"
)

// Deps holds only interfaces (plus the always-allowed Logger).
type Deps struct {
	Logger  *slog.Logger
	Charger Charger
}

type service struct{ deps Deps }

func New(deps Deps) Service { return &service{deps: deps} }

func (s *service) Run(ctx context.Context) error {
	_, err := s.deps.Charger.Charge(ctx, "u1", 100)
	return err
}
