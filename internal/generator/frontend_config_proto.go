package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/naming"
)

// FrontendConfigProtoRelPath is where a frontend's config message is
// declared, relative to the project root.
//
// ONE FILE PER CONSUMER, ONE SHARED PACKAGE. The file is per-frontend so
// that adding a frontend WRITES a new file rather than editing a file that
// already has content in it — which is what makes `forge scaffold frontend`
// safe on an existing project without parsing or rewriting anyone's proto.
//
// The DIRECTORY is shared (proto/config/v1/), so every config message stays
// in package config.v1 and therefore in one generated Go package. That is
// not tidiness: buf's STANDARD lint enforces PACKAGE_DIRECTORY_MATCH, so a
// per-consumer directory forces a per-consumer proto package, which forces
// a per-consumer Go package — and pkg/config/config_gen.go imports exactly
// one generated config package and names every config message through it.
// A directory-per-consumer layout does not merely reorganize files; it
// stops the generated Go config shim from compiling.
//
// Sharing the package also keeps shared DEFINITIONS free: a block declared
// in config.proto composes onto a message here by bare name, with no
// cross-package import, so one fact declared once projects into both the
// backend's config and the browser's.
//
// The base name goes through naming.GoPackage, not naming.ToSnakeCase.
// ToSnakeCase splits camelCase/PascalCase boundaries but passes a HYPHEN
// straight through, and `forge scaffold frontend` accepts hyphenated names
// (ValidateFrontendName rejects only a leading "-"). A frontend named
// "internal-console" therefore produced "internal-console_config.proto",
// which buf's STANDARD lint rejects with `Filename ... should be
// lower_snake_case.proto` — a red `buf lint` in the generated CI workflow
// on the project's first push, fixable only by renaming the frontend or
// hand-editing a forge-owned path. GoPackage is the canonical CLI-name →
// snake_case identifier rule (hyphens fold, repeated separators collapse),
// already used for the Go package, the KCL binding and the handler dir, so
// routing through it keeps this filename agreeing with them by construction.
func FrontendConfigProtoRelPath(frontend string) string {
	return filepath.Join("proto", "config", "v1", naming.GoPackage(frontend)+"_config.proto")
}

// FrontendConfigMessageName is the proto message declaring a frontend's
// config. It names the generated KCL schema and the TypeScript type, so it
// is derived once here rather than spelled independently at each site.
func FrontendConfigMessageName(frontend string) string {
	return naming.ToPascalCase(frontend) + "Config"
}

// frontendConfigEnvVars is the env_var set the scaffolded config template
// declares, in the file's own declaration order.
//
// It exists so the SCAFFOLD paths can build a FrontendTypedConfig without
// reading the descriptor. Both scaffold paths write this proto and render
// the frontend's templates in the same pass — before `forge generate` has
// ever run, so there is no descriptor to parse and no generated module to
// inspect. Deriving the presence set from the template's own field list is
// what lets a freshly scaffolded oidc-provider.ts import the typed module
// it is about to be given.
//
// It must stay in agreement with frontend_config.proto.tmpl;
// TestFrontendConfigTemplateMatchesDeclaredEnvVars is the gate.
var frontendConfigEnvVars = []string{
	"API_URL",
	"MOCK_API",
	"APP_VERSION",
	"OTEL_ENDPOINT",
	"OIDC_ISSUER",
	"OIDC_CLIENT_ID",
	"OIDC_REDIRECT_URI",
	"OIDC_SCOPES",
	"OIDC_RESOURCE",
}

// ScaffoldedFrontendTypedConfig is the presence set a frontend gets from
// the config proto this package scaffolds for it.
func ScaffoldedFrontendTypedConfig() FrontendTypedConfig {
	return FrontendTypedConfigFrom(frontendConfigEnvVars)
}

// frontendConfigProtoData is the render input for frontend_config.proto.tmpl.
type frontendConfigProtoData struct {
	Module      string
	Frontend    string
	MessageName string
	APIPort     int
}

// WriteFrontendConfigProto declares the named frontend's public runtime
// config, with the message-level (forge.v1.frontend_config) annotation that
// activates the three projections: the typed zod module the bundle imports,
// the KCL schema per-env values are authored against, and the config.js
// document the browser reads at boot.
//
// Without this file the config system is complete and unreachable — every
// generator keys on the annotation, so a project that declares none
// correctly emits nothing and its frontend keeps reading build-time
// process.env.NEXT_PUBLIC_*.
//
// APPEND-ONLY BY CONSTRUCTION. Adding a frontend to an existing project
// writes a NEW file, so no existing config content is read, rewritten or
// reordered — the failure mode the workloads.k append helper has to guard
// against does not arise here, because there is nothing to append INTO.
//
// It refuses to overwrite an existing file. A project that already declares
// this frontend's config has values in it (an issuer, a client id) that a
// re-scaffold must not silently discard.
func WriteFrontendConfigProto(root, modulePath, frontend string, apiPort int) error {
	if frontend == "" {
		return nil
	}
	rel := FrontendConfigProtoRelPath(frontend)
	dest := filepath.Join(root, rel)

	if _, err := os.Stat(dest); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", rel, err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(rel), err)
	}

	return assets.WriteTemplateWithData("frontend_config.proto.tmpl", dest, frontendConfigProtoData{
		Module:      modulePath,
		Frontend:    frontend,
		MessageName: FrontendConfigMessageName(frontend),
		APIPort:     apiPort,
	})
}
