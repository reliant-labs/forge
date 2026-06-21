package cli

// This file blank-imports the dir-nested command-group subpackages so their
// init() runs and self-registers each group's command factory with
// internal/cli/factory. NewRootCmd ranges factory.Registered() to attach
// them. As commands migrate from the flat files in this package into group
// subpackages, add the group's import here.
//
// The import is one-directional: groups import internal/cli/factory (and may
// import internal/cli for shared helpers); internal/cli blank-imports the
// groups here. The registry indirection in the factory package is what keeps
// that from being an import cycle.
import (
	_ "github.com/reliant-labs/forge/internal/cli/component"
)
