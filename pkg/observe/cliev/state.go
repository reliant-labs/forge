package cliev

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// stateDoc is telemetry.json.
type stateDoc struct {
	InstallID string `json:"install_id,omitempty"`
	// Enabled is the user's choice (`<app> telemetry on|off`); nil when they
	// never made one.
	Enabled      *bool  `json:"enabled,omitempty"`
	NoticeShown  bool   `json:"notice_shown,omitempty"`
	FirstRunSent bool   `json:"first_run_sent,omitempty"`
	Day          string `json:"day,omitempty"`
	SentToday    int    `json:"sent_today,omitempty"`
	// Attribution is where the install came from, as the install page passed
	// it (SetInstallAttribution): a query string of rid and utm_* only.
	Attribution string `json:"attribution,omitempty"`
}

// state is the file. A file that cannot be read is a fresh install; one that
// cannot be written is simply not remembered.
type state struct {
	path string
	mu   sync.Mutex
}

func newState(path string) *state { return &state{path: path} }

func (s *state) load() stateDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	var doc stateDoc
	data, err := os.ReadFile(s.path)
	if err != nil || json.Unmarshal(data, &doc) != nil {
		return stateDoc{}
	}
	return doc
}

func (s *state) save(doc stateDoc) { _ = s.saveErr(doc) }

func (s *state) saveErr(doc stateDoc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".telemetry-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// newUUID is a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
