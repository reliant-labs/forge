package ledgerfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

func TestReleaseSBOMIndexRoundTripsAndMerges(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	if x, err := s.ReleaseSBOMIndex("v1.0.0"); err != nil || x != nil {
		t.Fatalf("an index never written: %+v, %v", x, err)
	}
	d, err := s.PutSBOM([]byte(`{"bomFormat":"CycloneDX"}`))
	if err != nil {
		t.Fatal(err)
	}
	cut := time.Date(2026, 11, 25, 18, 8, 0, 0, time.UTC)
	if err := s.UpdateReleaseSBOMIndex("v1.0.0", func(x *ReleaseSBOMIndex) error {
		x.CutAt = cut
		x.Artifacts = map[string]release.SBOMRef{"api": {Digest: d, Status: release.SBOMStatusOK}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateReleaseSBOMIndex("v1.0.0", func(x *ReleaseSBOMIndex) error {
		x.Uploads = map[string]SBOMUpload{"https://cp.test": {State: SBOMUploadPending, Attempts: 1}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	x, err := s.ReleaseSBOMIndex("v1.0.0")
	if err != nil || x == nil {
		t.Fatal(x, err)
	}
	if x.Schema != SBOMIndexSchema || x.Artifacts["api"].Digest != d || !x.CutAt.Equal(cut) || x.Uploads["https://cp.test"].State != SBOMUploadPending {
		t.Fatalf("index: %+v", x)
	}
	// A failing update writes nothing.
	boom := errors.New("boom")
	if err := s.UpdateReleaseSBOMIndex("v1.0.0", func(x *ReleaseSBOMIndex) error { x.Artifacts = nil; return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if x, _ := s.ReleaseSBOMIndex("v1.0.0"); x.Artifacts["api"].Digest != d {
		t.Fatalf("a failed update changed the index: %+v", x)
	}
	// A bad digest is refused.
	if err := s.UpdateReleaseSBOMIndex("v1.0.0", func(x *ReleaseSBOMIndex) error {
		x.Artifacts["web"] = release.SBOMRef{Digest: "sha256:nope"}
		return nil
	}); err == nil {
		t.Fatal("a non-canonical digest was written")
	}
}

func TestReleaseSBOMIndexRefusesAnUnknownSchema(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	path := s.sbomIndexPath("v2")
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":"forge.ledger.sbom-index/v9","version":"v2","artifacts":{}}`), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseSBOMIndex("v2"); !errors.Is(err, ErrSBOMIndexUnknownSchema) {
		t.Fatalf("err = %v, want ErrSBOMIndexUnknownSchema", err)
	}
	// One unreadable index never hides the others' pending uploads.
	if err := s.UpdateReleaseSBOMIndex("v1", func(x *ReleaseSBOMIndex) error {
		x.Uploads = map[string]SBOMUpload{"ep": {State: SBOMUploadPending}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pending, skipped, err := s.PendingSBOMUploads("ep")
	if err != nil || len(pending) != 1 || pending[0].Version != "v1" || len(skipped) != 1 || !strings.Contains(skipped[0], "v9") {
		t.Fatalf("pending %+v, skipped %v, err %v", pending, skipped, err)
	}
}

func TestPendingSBOMUploadsOldestFirstPerEndpoint(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	base := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	for _, v := range []string{"v3", "v1", "v2", "v4"} {
		state := SBOMUploadPending
		if v == "v4" {
			state = SBOMUploadDone
		}
		at := base.Add(time.Duration(map[string]int{"v1": 1, "v2": 2, "v3": 3, "v4": 4}[v]) * time.Hour)
		if err := s.UpdateReleaseSBOMIndex(v, func(x *ReleaseSBOMIndex) error {
			x.CutAt = at
			x.Uploads = map[string]SBOMUpload{"a": {State: state}, "b": {State: SBOMUploadDone}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	pending, _, err := s.PendingSBOMUploads("a")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range pending {
		got = append(got, p.Version)
	}
	if strings.Join(got, ",") != "v1,v2,v3" {
		t.Fatalf("pending on a = %v, want v1,v2,v3", got)
	}
	if p, _, _ := s.PendingSBOMUploads("b"); len(p) != 0 {
		t.Fatalf("pending on b = %+v", p)
	}
}

func TestSBOMsListsAndHeadReadsAPrefix(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	if files, err := s.SBOMs(); err != nil || len(files) != 0 {
		t.Fatalf("empty ledger: %v %v", files, err)
	}
	doc := []byte(`{"bomFormat":"CycloneDX","metadata":{"component":{"name":"img"}},"components":[]}`)
	d, err := s.PutSBOM(doc)
	if err != nil {
		t.Fatal(err)
	}
	files, err := s.SBOMs()
	if err != nil || len(files) != 1 || files[0].Digest != d || files[0].Size != int64(len(doc)) {
		t.Fatalf("SBOMs = %+v, %v", files, err)
	}
	head, err := s.SBOMHead(d, 10)
	if err != nil || string(head) != string(doc[:10]) {
		t.Fatalf("head = %q, %v", head, err)
	}
	if all, err := s.SBOMHead(d, 1<<20); err != nil || string(all) != string(doc) {
		t.Fatalf("head past the end = %q, %v", all, err)
	}
	if _, err := s.SBOMHead("sha256:"+strings.Repeat("0", 64), 10); !errors.Is(err, ErrSBOMNotFound) {
		t.Fatalf("absent: %v", err)
	}
}
