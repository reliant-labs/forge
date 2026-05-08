// Package docs renders project documentation (markdown / hugo) from the
// project config, proto descriptors, and contract.go interfaces.
//
// The behavioural surface is a single Service. Sub-generators
// (APIGenerator, ArchitectureGenerator, ConfigGenerator, ContractGenerator)
// implement the package-internal Generator interface and are registered
// via Registry; that interface is internal scaffolding for the multi-doc
// pipeline, not the user-facing seam.
package docs

import (
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator/contract"
)

// Service is the docs package's public seam: one entry point that runs
// the configured generators against a project directory.
type Service interface {
	Run(projectDir string, cfg *config.ProjectConfig, overrides *Overrides) error
}

// Deps wires the cross-package collaborators docs needs. Each defaults
// to the canonical implementation when nil.
type Deps struct {
	CodegenParser codegen.Parser    // descriptor + go.mod parsing
	Contract      contract.Service  // contract.go AST parsing
}

// New constructs a docs.Service. nil Deps fields are filled with the
// canonical implementation from each collaborator package.
func New(d Deps) Service {
	if d.CodegenParser == nil {
		d.CodegenParser = codegen.NewParser(codegen.Deps{})
	}
	if d.Contract == nil {
		d.Contract = contract.New(contract.Deps{})
	}
	return &svc{deps: d}
}

type svc struct {
	deps Deps
}

// Run delegates to the package-level Run helper. The helper still
// references the package-level codegen / contract free funcs because
// those are also wired into the deps types — refactoring buildContext
// to walk via s.deps is a future cleanup once the CLI port lands.
func (s *svc) Run(projectDir string, cfg *config.ProjectConfig, overrides *Overrides) error {
	return Run(projectDir, cfg, overrides)
}

var _ Service = (*svc)(nil)
