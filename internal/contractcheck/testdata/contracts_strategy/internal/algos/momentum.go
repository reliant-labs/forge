package algos

import "context"

type Momentum struct{ Lookback int }

func NewMomentum(lookback int) *Momentum { return &Momentum{Lookback: lookback} }

func (m *Momentum) Name() string { return "momentum" }

func (m *Momentum) Run(ctx context.Context, input []float64) (float64, error) {
	if len(input) < m.Lookback {
		return 0, nil
	}
	return input[len(input)-1] - input[len(input)-m.Lookback], nil
}

func init() { Register(NewMomentum(20)) }
