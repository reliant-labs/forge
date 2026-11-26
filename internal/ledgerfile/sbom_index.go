package ledgerfile

// The per-release SBOM index: which SBOM each artifact of a release names,
// and what each control plane was told about them.
//
//	<ledger>/<project-id>/
//	  sbom-index/<release>.json        one document per release
//
// WHY IT EXISTS. A release recorded on a control plane is stored there
// without its SBOM refs (they travel through their own rpc, never inside the
// release document), so once a cut's upload was refused or skipped, nothing
// but this machine knew which SBOM file describes which artifact. The index
// keeps that mapping beside the SBOM bytes, whichever ledger the env records
// releases in, so `forge vuln sbom push` can re-send a release's SBOMs and a
// later cut can drain the uploads an older control plane refused.
//
// VERSIONED, AND INVISIBLE TO AN OLDER FORGE. An older forge never reads
// sbom-index/, so the directory costs it nothing. Each document names its
// schema; a reader that meets a schema it does not know reports
// ErrSBOMIndexUnknownSchema rather than misreading it.
//
// Every document is written whole (temp file, then rename) under the ledger
// lock, so a read-modify-write by two cuts at once loses neither.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

const (
	sbomIndexDir    = "sbom-index"
	sbomIndexSuffix = ".json"
	// SBOMIndexSchema is the index document's schema.
	SBOMIndexSchema = "forge.ledger.sbom-index/v1"
)

// The SBOMUpload.State values.
const (
	// SBOMUploadPending is a release whose SBOMs a control plane has not
	// taken yet: it refused them (too old, or Unimplemented), could not be
	// asked, or failed some of them. The next cut to it re-sends them.
	SBOMUploadPending = "pending"
	// SBOMUploadDone is a release every SBOM of which the control plane
	// took, already held, or holds a different copy of: nothing is left to
	// send.
	SBOMUploadDone = "done"
	// SBOMUploadAbandoned is a pending release the control plane no longer
	// knows (it answered NotFound): nothing will be re-sent.
	SBOMUploadAbandoned = "abandoned"
)

// ErrSBOMIndexUnknownSchema is an index document written by a newer forge,
// in a schema this one does not read.
var ErrSBOMIndexUnknownSchema = errors.New("sbom index written in a schema this forge does not read")

// ReleaseSBOMIndex is one release's SBOM index.
type ReleaseSBOMIndex struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	// Project is the forge project the release belongs to, when known.
	Project string `json:"project,omitempty"`
	// CutAt is the release's cut time; it orders a drain of pending
	// releases, oldest first.
	CutAt time.Time `json:"cut_at,omitempty"`
	// Artifacts is each artifact's SBOM ref as the cut recorded it, an
	// unavailable one (no SBOM, with the reason) included. Artifacts that
	// describe no shipped code (a source pin, source maps) are absent.
	Artifacts map[string]release.SBOMRef `json:"artifacts"`
	// Uploads is what each control plane was told, by its endpoint URL.
	Uploads map[string]SBOMUpload `json:"uploads,omitempty"`
}

// SBOMUpload is the upload state of one release on one control plane.
type SBOMUpload struct {
	State string `json:"state"`
	// Reason says why a pending or abandoned release is so.
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// Attempts counts the uploads tried while pending; a drain gives up on
	// a release after a bound (the caller's).
	Attempts int `json:"attempts,omitempty"`
	// Artifacts is the outcome per artifact of the last upload
	// ("sent", "already_recorded", "differs", "missing", "failed").
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

func (x ReleaseSBOMIndex) validate() error {
	switch {
	case x.Schema != SBOMIndexSchema:
		return fmt.Errorf("%w: %q (this forge reads %s)", ErrSBOMIndexUnknownSchema, x.Schema, SBOMIndexSchema)
	case strings.TrimSpace(x.Version) == "":
		return fmt.Errorf("%w: sbom index names no release", release.ErrInvalid)
	}
	for name, ref := range x.Artifacts {
		if ref.Digest != "" {
			if _, err := sbomFileStem(ref.Digest); err != nil {
				return fmt.Errorf("sbom index of %s, artifact %s: %w", x.Version, name, err)
			}
		}
	}
	return nil
}

func (s *Store) sbomIndexPath(version string) string {
	return s.path(sbomIndexDir, safeSegment(version)+sbomIndexSuffix)
}

// ReleaseSBOMIndex returns release version's index, or (nil, nil) when this
// ledger holds none. A file under the release's name that records another
// release (two versions that flatten to one file name) is none too.
func (s *Store) ReleaseSBOMIndex(version string) (*ReleaseSBOMIndex, error) {
	var out *ReleaseSBOMIndex
	err := s.withLock(func() error {
		var err error
		out, err = s.readSBOMIndex(s.sbomIndexPath(version), version)
		return err
	})
	return out, err
}

func (s *Store) readSBOMIndex(path, version string) (*ReleaseSBOMIndex, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from Store.path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var x ReleaseSBOMIndex
	if err := json.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := x.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if version != "" && x.Version != version {
		return nil, nil
	}
	return &x, nil
}

// UpdateReleaseSBOMIndex reads release version's index (a fresh one when
// there is none), hands it to fn, and writes back what fn leaves, under the
// ledger lock. fn returning an error writes nothing.
func (s *Store) UpdateReleaseSBOMIndex(version string, fn func(*ReleaseSBOMIndex) error) error {
	if strings.TrimSpace(version) == "" {
		return fmt.Errorf("%w: sbom index names no release", release.ErrInvalid)
	}
	return s.withLock(func() error {
		path := s.sbomIndexPath(version)
		x, err := s.readSBOMIndex(path, "")
		if err != nil {
			return err
		}
		if x != nil && x.Version != version {
			return fmt.Errorf("%s records release %s, not %s: two versions share one file name", path, x.Version, version)
		}
		if x == nil {
			x = &ReleaseSBOMIndex{Schema: SBOMIndexSchema, Version: version}
		}
		if err := fn(x); err != nil {
			return err
		}
		if x.Artifacts == nil {
			x.Artifacts = map[string]release.SBOMRef{}
		}
		if err := x.validate(); err != nil {
			return err
		}
		data, err := canonicalJSON(x)
		if err != nil {
			return err
		}
		return writeFileAtomic(path, append(data, '\n'))
	})
}

// PendingSBOMUploads returns every release whose upload to endpoint is
// pending, oldest cut first (then by version). An index this forge cannot
// read is skipped and named in skipped, never fatal: one bad file must not
// stop every other release's re-send.
func (s *Store) PendingSBOMUploads(endpoint string) (pending []ReleaseSBOMIndex, skipped []string, err error) {
	err = s.withLock(func() error {
		dir := s.path(sbomIndexDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), sbomIndexSuffix) || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			x, err := s.readSBOMIndex(filepath.Join(dir, e.Name()), "")
			if err != nil {
				skipped = append(skipped, err.Error())
				continue
			}
			if x != nil && x.Uploads[endpoint].State == SBOMUploadPending {
				pending = append(pending, *x)
			}
		}
		return nil
	})
	sort.SliceStable(pending, func(i, j int) bool {
		if !pending[i].CutAt.Equal(pending[j].CutAt) {
			return pending[i].CutAt.Before(pending[j].CutAt)
		}
		return pending[i].Version < pending[j].Version
	})
	return pending, skipped, err
}

// SBOMFile is one SBOM this ledger stores.
type SBOMFile struct {
	Digest  string
	ModTime time.Time
	Size    int64
}

// SBOMs lists the SBOMs this ledger stores, newest file first. The bytes are
// not read: GetSBOM verifies them when they are.
func (s *Store) SBOMs() ([]SBOMFile, error) {
	dir := s.path(sbomsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []SBOMFile
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, sbomSuffix) {
			continue
		}
		digest := strings.Replace(strings.TrimSuffix(name, sbomSuffix), "-", ":", 1)
		if !release.ValidDigest(digest) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed between the listing and the stat
		}
		out = append(out, SBOMFile{Digest: digest, ModTime: info.ModTime(), Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.After(out[j].ModTime)
		}
		return out[i].Digest < out[j].Digest
	})
	return out, nil
}

// SBOMHead returns up to n leading bytes of the SBOM stored under digest,
// unverified: enough to read a CycloneDX document's metadata without reading
// (and hashing) a multi-megabyte file. Anything decided from it is confirmed
// with GetSBOM before it is used.
func (s *Store) SBOMHead(digest string, n int) ([]byte, error) {
	stem, err := sbomFileStem(digest)
	if err != nil {
		return nil, err
	}
	path := s.path(sbomsDir, stem+sbomSuffix)
	f, err := os.Open(path) //nolint:gosec // path comes from Store.path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", digest, ErrSBOMNotFound)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	m, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return buf[:m], nil
}
