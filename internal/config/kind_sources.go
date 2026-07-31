package config

import (
	"os"
	"path/filepath"
)

// deriveProjectKindFromSources determines the project kind by reading the
// project's REAL sources on disk — never a manifest, never an authored
// forge.yaml bit. Every signal is a directory or file the scaffold and the
// add verbs actually write, so the kind cannot drift from the tree:
//
//   - the KCL deploy tree (deploy/kcl/), the service composition root /
//     registry (pkg/app/), the service implementations
//     (internal/handlers/), or the service protos (proto/services/) →
//     service. Any one of them is enough: forge emits them only for service
//     projects, so even a zero-service scaffold reads as a service, which is
//     exactly what it is — a shell waiting for `forge scaffold service`.
//   - otherwise a cmd/<name>/main.go binary → a CLI;
//   - nothing of the above → a pure library (a Go module with no entrypoint).
//
// projectDir is the directory holding forge.yaml.
func deriveProjectKindFromSources(projectDir string) string {
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

	// Not service-shaped. A cmd/<name>/main.go binary is a CLI; anything
	// else is a library.
	if hasCmdBinary(projectDir) {
		return ProjectKindCLI
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
