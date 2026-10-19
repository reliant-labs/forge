package cli

import (
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/devstack"
)

// composeSpecFor is shared by launch and discovery. An env's project name is
// an exact checkout identity, not a directory basename (nested worktrees all
// have the same basename). Explicit project names remain the author's choice.
func composeSpecFor(projectDir, env string, cm *ComposeRuntime) *deploytarget.ComposeSpec {
	spec := &deploytarget.ComposeSpec{ComposeFile: cm.File, Service: cm.Service, EnvFile: cm.EnvFile, Wait: cm.Wait, WaitTimeoutSeconds: cm.WaitTimeout, Shared: cm.Shared, ProjectDirectory: projectDir}
	spec.Env = make(map[string]string, len(cm.Env)+1)
	for k, v := range cm.Env {
		spec.Env[k] = v
	}
	if cm.Shared {
		spec.ProjectDirectory = devstack.SharedProjectDir(projectDir)
	} else if spec.Env["COMPOSE_PROJECT_NAME"] == "" {
		spec.Env["COMPOSE_PROJECT_NAME"] = "forge-" + projectIDForDir(projectDir) + "-" + env
	}
	return spec
}
