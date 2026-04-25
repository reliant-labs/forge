package packs

import (
	"embed"
	"fmt"
	"sort"
)

//go:embed all:api-key all:audit-log all:jwt-auth all:stripe all:clerk all:twilio
var packsFS embed.FS

// ListPacks returns all available packs by scanning the embedded FS
// for directories containing a pack.yaml manifest.
func ListPacks() ([]Pack, error) {
	entries, err := packsFS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read packs directory: %w", err)
	}

	var packs []Pack
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Try to load pack.yaml from this directory
		p, err := LoadPack(entry.Name())
		if err != nil {
			// Skip directories without a valid manifest
			continue
		}
		packs = append(packs, *p)
	}

	sort.Slice(packs, func(i, j int) bool {
		return packs[i].Name < packs[j].Name
	})

	return packs, nil
}

// GetPack returns a specific pack by name, or an error if not found.
func GetPack(name string) (*Pack, error) {
	if !ValidPackName(name) {
		return nil, fmt.Errorf("invalid pack name %q", name)
	}
	return LoadPack(name)
}