package generator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

const checksumFile = ".forge/checksums.json"

// FileChecksums tracks sha256 digests of forge-generated files.
type FileChecksums struct {
	ForgeVersion string            `json:"forge_version"`
	Files        map[string]string `json:"files"` // relative path -> sha256
}

// LoadChecksums loads the checksum file from the project root.
func LoadChecksums(root string) (*FileChecksums, error) {
	path := filepath.Join(root, checksumFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileChecksums{Files: make(map[string]string)}, nil
		}
		return nil, err
	}
	var cs FileChecksums
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, err
	}
	if cs.Files == nil {
		cs.Files = make(map[string]string)
	}
	return &cs, nil
}

// SaveChecksums writes the checksum file.
func SaveChecksums(root string, cs *FileChecksums) error {
	path := filepath.Join(root, checksumFile)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// HashContent returns the sha256 hex digest of content.
func HashContent(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}

// IsFileModified checks if a generated file has been modified by the user.
// Returns true if the file exists but its hash doesn't match the stored checksum.
// Returns false if the file doesn't exist or matches the checksum.
func (cs *FileChecksums) IsFileModified(root, relPath string) bool {
	stored, ok := cs.Files[relPath]
	if !ok {
		return false // never tracked, not "modified"
	}
	content, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		return false // file doesn't exist
	}
	return HashContent(content) != stored
}

// RecordFile stores the checksum for a generated file.
func (cs *FileChecksums) RecordFile(relPath string, content []byte) {
	cs.Files[relPath] = HashContent(content)
}

// WriteGeneratedFile writes content to a file and records its checksum.
// If the file has been user-modified (checksum mismatch), it skips the write.
// Returns true if the file was written, false if skipped.
func WriteGeneratedFile(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error) {
	if !force && cs.IsFileModified(root, relPath) {
		// User has modified this file — skip
		return false, nil
	}

	fullPath := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return false, err
	}
	if err := os.WriteFile(fullPath, content, 0644); err != nil {
		return false, err
	}
	cs.RecordFile(relPath, content)
	return true, nil
}
