package packs

import (
	"context"

	"github.com/reliant-labs/forge/internal/config"
)

// Manager defines the contract for pack operations: loading, installing,
// removing, and listing packs within a Forge project.
type Manager interface {
	Load(ctx context.Context, name string) (*Pack, error)
	Install(ctx context.Context, pack *Pack, projectDir string, cfg *config.ProjectConfig) error
	Remove(ctx context.Context, pack *Pack, projectDir string, cfg *config.ProjectConfig) error
	List(ctx context.Context) ([]Pack, error)
	InstalledPacks(ctx context.Context, cfg *config.ProjectConfig) ([]*Pack, error)
	IsInstalled(ctx context.Context, name string, cfg *config.ProjectConfig) bool
	RenderGenerateFiles(ctx context.Context, pack *Pack, projectDir string, cfg *config.ProjectConfig) error
}
