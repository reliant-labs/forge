package api

import (
	"log/slog"
)

type Authorizer interface{ Check() }
type Config struct{}
type EventPublisher interface{}

// Deps holds dependencies for the api service.
type Deps struct {
	Logger     *slog.Logger
	Config     *Config
	Authorizer Authorizer

	// NATSPublisher is intentionally optional.
	// forge:optional-dep
	NATSPublisher EventPublisher
}
