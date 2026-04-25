package debug

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const sessionFile = ".forge/debug-session.json"

// LoadSession reads the persisted debug session from dir.
// Returns (nil, nil) if no session file exists.
func LoadSession(dir string) (*SessionInfo, error) {
	path := filepath.Join(dir, sessionFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading session file: %w", err)
	}
	var s SessionInfo
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing session file: %w", err)
	}
	return &s, nil
}

// SaveSession writes a debug session to dir/.forge/debug-session.json.
func SaveSession(dir string, session *SessionInfo) error {
	path := filepath.Join(dir, sessionFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating session directory: %w", err)
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// ClearSession removes the session file from dir.
func ClearSession(dir string) error {
	path := filepath.Join(dir, sessionFile)
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing session file: %w", err)
	}
	return nil
}
