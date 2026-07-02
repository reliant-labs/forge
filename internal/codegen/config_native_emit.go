// Package codegen — config_native_emit.go wires the three KCL-native config
// emitters (config_schema_gen.go, config_projection_gen.go, config_k_gen.go)
// into the generate pipeline. It is the Phase-4 counterpart to
// deploy_config_gen.go's GenerateDeployConfig (the Go projector that renders
// the per-env config_gen.k).
//
// Two ownership tiers, mirroring the design:
//
//   - config_schema.k + config_projection.k are PROJECT-LEVEL and Tier-1
//     forge-owned (writeForgeOwned): one pair per project, regenerated from
//     proto on every `forge generate`. They carry the config TYPE and the
//     projection BEHAVIOR — the role config_gen.k's schema/env-list logic
//     played, now factored out of the per-env files.
//   - deploy/kcl/<env>/config.k is PER-ENV and USER-OWNED (write-if-absent):
//     the one-time migration of config.<env>.yaml into a typed AppConfig
//     instance. forge scaffolds it once and never clobbers later edits.
package codegen

import (
	"fmt"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/checksums"
)

// GenerateConfigNativeShared emits the two project-level, forge-owned KCL
// files that back the KCL-native config path — <kclDirAbs>/config_schema.k and
// <kclDirAbs>/config_projection.k — from the proto-derived config fields.
//
// projectDir is the project root (for the checksum-relative path); kclDirAbs is
// the absolute deploy/kcl directory; cs is the checksum ledger. When cs is nil
// or the path can't be made relative, the files are still written (untracked).
func GenerateConfigNativeShared(fields []ConfigField, projectName, projectDir, kclDirAbs string, cs *checksums.FileChecksums) error {
	schema, err := GenerateConfigSchemaKCL(fields, projectName)
	if err != nil {
		return fmt.Errorf("generate config schema: %w", err)
	}
	proj, err := GenerateConfigProjectionKCL(fields)
	if err != nil {
		return fmt.Errorf("generate config projection: %w", err)
	}

	files := []struct{ name, body string }{
		{ConfigSchemaModule + ".k", schema},
		{"config_projection.k", proj},
	}
	for _, f := range files {
		outPath := filepath.Join(kclDirAbs, f.name)
		if cs != nil && projectDir != "" {
			if rel, rerr := filepath.Rel(projectDir, outPath); rerr == nil {
				if werr := writeForgeOwned(projectDir, rel, []byte(f.body), cs); werr != nil {
					return fmt.Errorf("write %s: %w", outPath, werr)
				}
				continue
			}
		}
		if werr := writeUserScaffold(outPath, []byte(f.body)); werr != nil {
			return fmt.Errorf("write %s: %w", outPath, werr)
		}
	}
	return nil
}

// GenerateConfigKScaffold emits deploy/kcl/<envName>/config.k — the per-env,
// user-owned typed AppConfig VALUES instance migrated from config.<env>.yaml —
// ONLY when it does not already exist. Returns true when a fresh file was
// written, false when an existing user-owned file was left untouched.
func GenerateConfigKScaffold(fields []ConfigField, envConfig map[string]any, projectName, kclDirAbs, envName string) (bool, error) {
	body, err := GenerateConfigKFromYAML(fields, envConfig, projectName)
	if err != nil {
		return false, fmt.Errorf("generate config.k for %s: %w", envName, err)
	}
	outPath := filepath.Join(kclDirAbs, envName, "config.k")
	return writeUserScaffoldIfAbsent(outPath, []byte(body))
}
