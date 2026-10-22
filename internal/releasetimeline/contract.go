package releasetimeline

import "context"

// Source supplies the ledger data for one environment. Local (the machine
// ledger) and Cloud (a control plane) implement it, so the verbs assemble a
// timeline the same way whichever backs the environment.
//
//forge:contract
type Source interface {
	Load(ctx context.Context, env string, q Query) (Data, error)
}
