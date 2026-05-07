package api

import (
	"log/slog"
)

type Authorizer interface{ Check() }
type Config struct{}
type EventPublisher interface{}

// Deps holds dependencies for the api service.
//
// forge:optional-dep
type Deps struct {
	Logger     *slog.Logger
	Config     *Config
	Authorizer Authorizer

	NATSPublisher EventPublisher
}

// helper does nothing.
//
// forge:optional-dep
func helper() {}
