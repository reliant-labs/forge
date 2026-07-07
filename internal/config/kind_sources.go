package config

import (
	"os"
	"path/filepath"
)

// deriveProjectKindFromSources determines the project kind by reading the
// project's REAL sources — never a manifest, never an authored forge.yaml
// bit. The order of signals mirrors what the scaffold actually writes:
//
//   - a server-shaped component (from the proto descriptor, threaded in as
//     `components`) → the project serves Connect RPC → service;
//   - the KCL deploy tree (deploy/kcl/) or the service composition root /
//     registry (pkg/app/, pkg/app/services.go) → a service project even
//     before its first service exists — these are generated only for
//     services;
//   - otherwise a cmd/<name>/main.go binary → a CLI;
//   - nothing of the above → a pure library.
//
// This is the deliberate replacement for the old components.json presence
// bit: the shape is read from the KCL deployment tree and the service
// registry (the sources forge already owns) instead of a cached manifest.
// projectDir is the directory holding forge.yaml.
func deriveProjectKindFromSources(projectDir string, components []ComponentConfig) string {
	// A server-shaped component is the strongest, source-of-truth signal.
	for _, c := range components {
		if c.EffectiveKind() != ComponentKindBinary {
			return ProjectKindService
		}
	}

	// These directories are emitted only for service projects — the KCL
	// deploy tree, the pkg/app composition root / service registry, the
	// service implementations (internal/handlers/<svc>/contract.go), and the
	// service protos. Any one present makes even a zero-service scaffold read
	// as a service. A CLI or library carries none of them.
	serviceSources := []string{
		filepath.Join(projectDir, "deploy", "kcl"),        // KCL deploy tree
		filepath.Join(projectDir, "pkg", "app"),           // composition root / registry home
		filepath.Join(projectDir, "internal", "handlers"), // service impls + contract.go
		filepath.Join(projectDir, "proto", "services"),    // service protos
	}
	for _, d := range serviceSources {
		if dirExists(d) {
			return ProjectKindService
		}
	}

	// Not service-shaped. A cmd/<name>/main.go binary (or a binary-kind
	// component) is a CLI; anything else is a library.
	if hasCmdBinary(projectDir) {
		return ProjectKindCLI
	}
	for _, c := range components {
		if c.EffectiveKind() == ComponentKindBinary {
			return ProjectKindCLI
		}
	}
	return ProjectKindLibrary
}

// hasCmdBinary reports whether projectDir carries a cmd/<name>/main.go — the
// real entrypoint of a CLI (or service) binary. Best-effort: a missing cmd/
// tree yields false.
func hasCmdBinary(projectDir string) bool {
	entries, err := os.ReadDir(filepath.Join(projectDir, "cmd"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(projectDir, "cmd", e.Name(), "main.go")) {
			return true
		}
	}
	return false
}

// sourceProjectDir returns the directory to read real sources from, or "" when
// there is no on-disk project to read (byte-only loads in tests pass a
// synthetic path). It requires the forge.yaml itself to exist so we never
// probe an unrelated cwd for a hand-constructed config.
func sourceProjectDir(forgeYAMLPath string) string {
	if forgeYAMLPath == "" {
		return ""
	}
	if _, err := os.Stat(forgeYAMLPath); err != nil {
		return ""
	}
	return filepath.Dir(forgeYAMLPath)
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
