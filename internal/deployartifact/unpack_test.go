package deployartifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry is one thing to put in a test archive. Kept as a struct rather
// than a builder-with-methods so a hostile archive is a literal a reader
// can see the shape of at a glance.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	// repeatBody, when > 0, writes body that many times — for the bomb
	// test, which needs far more bytes than a literal should carry.
	repeatBody int
}

// buildTarGz assembles a gzipped tar from entries, with NO sanitisation.
// That is the point: this helper must be able to produce the archives a
// real attacker would, or the defences are being tested against inputs
// that were already made safe.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		body := e.body
		if e.repeatBody > 0 {
			body = strings.Repeat(e.body, e.repeatBody)
		}
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: flag,
			Linkname: e.linkname,
		}
		if flag == tar.TypeDir {
			hdr.Size = 0
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if flag == tar.TypeReg {
			if _, err := io.WriteString(tw, body); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return raw.Bytes()
}

// validDoc is a well-formed artifact document, exercising BOTH addressing
// shapes — an oci item pinned by digest and an npm item addressed by
// version + integrity — so a parse that handled only one would fail.
func validDoc(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(Artifact{
		Release: "v1.4.0",
		Env:     "prod",
		Items: map[string]Item{
			"api": {
				Name:   "api",
				Kind:   KindOCI,
				Digest: "sha256:" + strings.Repeat("ab", 32),
			},
			"web-runtime": {
				Name:      "web-runtime",
				Kind:      KindNPM,
				Version:   "0.3.1",
				Integrity: "sha512-abc==",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	return string(raw)
}

// ─────────────────────────────────────────────────────────────────────────────
// The happy path — which exists so the hostile cases below prove the
// defence and not merely that unpacking fails in general.
//
// This is the control on the controls. Without it, every "refuses X" test
// would pass against an Unpack whose body was `return ErrUnsafeArchive`,
// which proves nothing about whether the specific defence works.
// ─────────────────────────────────────────────────────────────────────────────

func TestUnpack_ExtractsAWellFormedArtifact(t *testing.T) {
	dir := t.TempDir()
	archive := buildTarGz(t, []tarEntry{
		{name: "manifests/", typeflag: tar.TypeDir},
		{name: "manifests/deployment.yaml", body: "kind: Deployment\n"},
		{name: ArtifactDocName, body: validDoc(t)},
	})

	written, err := Unpack(bytes.NewReader(archive), dir)
	if err != nil {
		t.Fatalf("Unpack of a well-formed archive: %v", err)
	}
	if len(written) != 2 {
		t.Errorf("extracted %v, want the two regular files", written)
	}

	got, rerr := os.ReadFile(filepath.Join(dir, "manifests", "deployment.yaml"))
	if rerr != nil {
		t.Fatalf("read extracted file: %v", rerr)
	}
	if string(got) != "kind: Deployment\n" {
		t.Errorf("extracted content = %q", got)
	}

	// Modes are FIXED, not inherited: nothing in an artifact needs to be
	// executable, and honouring the archive's bits is how a setuid file
	// arrives.
	info, _ := os.Stat(filepath.Join(dir, "manifests", "deployment.yaml"))
	if info.Mode().Perm() != unpackFileMode {
		t.Errorf("extracted mode = %v, want %v", info.Mode().Perm(), unpackFileMode)
	}

	art, derr := ReadArtifactDoc(dir)
	if derr != nil {
		t.Fatalf("ReadArtifactDoc: %v", derr)
	}
	if art.Release != "v1.4.0" || len(art.Items) != 2 {
		t.Errorf("parsed artifact = %+v", art)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Defensive unpack — one test per attack.
// ─────────────────────────────────────────────────────────────────────────────

// TestUnpack_RefusesPathTraversal: the classic "../../" escape.
//
// The assertion is not merely that Unpack errors — it is that NOTHING
// LANDED OUTSIDE the destination. An implementation that wrote the file
// and then returned an error would satisfy an error-only assertion while
// having already compromised the machine.
func TestUnpack_RefusesPathTraversal(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	outside := filepath.Join(root, "pwned.txt")

	archive := buildTarGz(t, []tarEntry{
		{name: "../pwned.txt", body: "owned"},
	})
	_, err := Unpack(bytes.NewReader(archive), dest)
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
	if _, serr := os.Stat(outside); serr == nil {
		t.Fatal("the traversal entry LANDED at " + outside + " — erroring after the write is not a defence")
	}
}

// TestUnpack_RefusesAbsolutePaths. filepath.Join would silently re-root
// "/etc/passwd" under the destination, which happens to be safe but hides
// that the archive asked for something it must never ask for.
func TestUnpack_RefusesAbsolutePaths(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{{name: "/etc/cron.d/evil", body: "* * * * * root sh"}})
	_, err := Unpack(bytes.NewReader(archive), t.TempDir())
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %q does not say the entry was absolute", err)
	}
}

// TestUnpack_RefusesSymlinks is the subtle one, and the reason a path
// check alone is not enough.
//
// Neither entry's NAME escapes the destination, so a containment check on
// names passes BOTH. The escape happens because the first entry makes
// `link` point at /tmp and the second writes THROUGH it. This test builds
// exactly that two-entry archive, so an implementation that checked paths
// but permitted symlinks fails here and would pass every other test in
// this file.
func TestUnpack_RefusesSymlinks(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{
		{name: "link", typeflag: tar.TypeSymlink, linkname: "/tmp"},
		{name: "link/escaped.txt", body: "through the link"},
	})
	_, err := Unpack(bytes.NewReader(archive), t.TempDir())
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive — a symlink escapes the destination even "+
			"though neither entry name does", err)
	}
	if !strings.Contains(err.Error(), "disallowed type") {
		t.Errorf("error %q does not name the entry type as the reason", err)
	}
}

// TestUnpack_RefusesHardlinks — the same escape, different typeflag. A
// defence written as "reject symlinks" rather than "allow only regular
// files and dirs" would miss this.
func TestUnpack_RefusesHardlinks(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{
		{name: "hard", typeflag: tar.TypeLink, linkname: "/etc/passwd"},
	})
	if _, err := Unpack(bytes.NewReader(archive), t.TempDir()); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
}

// TestUnpack_RefusesDecompressionBomb.
//
// The archive declares an HONEST Size here, which is what makes the test
// meaningful in the other direction too: the defence must count bytes
// actually read rather than trusting the header, and a bomb that lied
// about its size would be caught by the same limit. A few hundred KB of
// gzip expands past the ceiling.
func TestUnpack_RefusesDecompressionBomb(t *testing.T) {
	// 1 KiB repeated past MaxTotalBytes. Highly compressible, so the
	// archive itself stays small.
	reps := int(MaxTotalBytes/1024) + 16
	archive := buildTarGz(t, []tarEntry{
		{name: "bomb.bin", body: strings.Repeat("A", 1024), repeatBody: reps},
	})
	if int64(len(archive)) > MaxTotalBytes {
		t.Fatalf("the fixture archive is %d bytes — it must be SMALL, or it is not a bomb "+
			"and the test would pass for the wrong reason", len(archive))
	}

	_, err := Unpack(bytes.NewReader(archive), t.TempDir())
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q does not name a size limit", err)
	}
}

// TestUnpack_RefusesAggregateBomb exercises the TOTAL-bytes ceiling
// specifically, with every individual file comfortably under the per-file
// cap.
//
// THIS TEST EXISTS BECAUSE ITS ABSENCE WAS CAUGHT BY SABOTAGE. Deleting
// the MaxTotalBytes limit left TestUnpack_RefusesDecompressionBomb still
// passing — that fixture is one large file, so MaxFileBytes was catching
// it and the total ceiling was never the thing under test. A defence no
// test exercises is a defence that can be deleted silently, which is the
// entire failure mode the sabotage step is for.
//
// The distinction is not academic. Many-modest-files is the EASIER bomb
// to build: it needs no single oversized member, so it slips past any
// per-file check while still filling a disk.
func TestUnpack_RefusesAggregateBomb(t *testing.T) {
	// Each file is an eighth of the per-file cap — never individually
	// suspicious — but together they exceed the total ceiling.
	const per = MaxFileBytes / 8
	count := int(MaxTotalBytes/per) + 4

	entries := make([]tarEntry, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, tarEntry{
			name:       "chunk/" + itoa(i) + ".bin",
			body:       strings.Repeat("A", 1024),
			repeatBody: int(per / 1024),
		})
	}
	if count > MaxEntries {
		t.Fatalf("the fixture needs %d entries, over the %d entry cap — the entry cap would "+
			"catch it first and this test would not exercise the byte ceiling", count, MaxEntries)
	}

	archive := buildTarGz(t, entries)
	_, err := Unpack(bytes.NewReader(archive), t.TempDir())
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive — %d files of %d bytes each exceed the %d "+
			"total ceiling while every one is under the %d per-file cap",
			err, count, per, MaxTotalBytes, MaxFileBytes)
	}
	if !strings.Contains(err.Error(), "decompressed output exceeds") {
		t.Errorf("error %q was raised by some other limit; this test must exercise the TOTAL "+
			"ceiling, not the per-file or entry cap", err)
	}
}

// TestUnpack_RefusesTooManyEntries. The byte ceiling does not bound this
// shape at all — a million zero-length files cost no bytes — so it needs
// its own cap.
func TestUnpack_RefusesTooManyEntries(t *testing.T) {
	entries := make([]tarEntry, 0, MaxEntries+8)
	for i := 0; i < MaxEntries+8; i++ {
		entries = append(entries, tarEntry{name: "f" + itoa(i), body: ""})
	}
	archive := buildTarGz(t, entries)
	_, err := Unpack(bytes.NewReader(archive), t.TempDir())
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
	if !strings.Contains(err.Error(), "entries") {
		t.Errorf("error %q does not name the entry cap", err)
	}
}

// TestUnpack_RefusesDuplicateEntries. An archive listing a path twice
// would otherwise have its second copy win, so a reviewer auditing the
// first occurrence would be auditing bytes that never landed. O_EXCL
// turns that into a refusal.
func TestUnpack_RefusesDuplicateEntries(t *testing.T) {
	archive := buildTarGz(t, []tarEntry{
		{name: "config.yaml", body: "reviewed: true\n"},
		{name: "config.yaml", body: "reviewed: false\n"},
	})
	dir := t.TempDir()
	if _, err := Unpack(bytes.NewReader(archive), dir); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive for a duplicate entry", err)
	}
}

// TestUnpack_RefusesNonGzip: a stream that is not gzip at all must be
// refused as unsafe rather than producing a confusing tar parse error
// halfway through.
func TestUnpack_RefusesNonGzip(t *testing.T) {
	if _, err := Unpack(strings.NewReader("not a gzip stream"), t.TempDir()); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
}

// itoa avoids importing strconv for one call in a test helper.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// ─────────────────────────────────────────────────────────────────────────────
// Artifact.Validate — the client-side twin of migration 00075's CHECKs
// ─────────────────────────────────────────────────────────────────────────────

func TestValidate_AcceptsBothAddressingShapes(t *testing.T) {
	a := Artifact{
		Release: "v1",
		Items: map[string]Item{
			"api": {Name: "api", Digest: "sha256:" + strings.Repeat("ab", 32)},
			"pkg": {Name: "pkg", Kind: KindNPM, Version: "1.0.0"},
			"mod": {Name: "mod", Kind: KindGoModule, Version: "v0.1.15", Integrity: "h1:abc="},
			"blob": {Name: "blob", Kind: KindFile,
				URI: "https://cdn.example/x.tgz", Integrity: "sha256:abc"},
		},
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("a valid artifact was rejected: %v", err)
	}
}

// TestValidate_OCIWithoutDigestIsRefused is the client-side analogue of
// the migration's most carefully-argued line. In SQL, a CHECK that
// evaluates to NULL is SATISFIED, so a nullable digest column needed
// `IS NOT NULL` written explicitly or the constraint would enforce
// nothing. The Go analogue is that "" matches nothing and would silently
// slip past a regex-only check, so emptiness is tested first.
func TestValidate_OCIWithoutDigestIsRefused(t *testing.T) {
	a := Artifact{Release: "v1", Items: map[string]Item{
		"api": {Name: "api", Kind: KindOCI},
	}}
	err := a.Validate()
	if !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("error = %v, want ErrInvalidArtifact", err)
	}
	if !strings.Contains(err.Error(), "no digest") {
		t.Errorf("error %q does not name the missing digest", err)
	}
}

// TestValidate_EmptyKindIsOCIAndStillNeedsADigest pins that the default
// is spelled the same way on both sides of the wire. An empty kind means
// OCI (matching ReleaseArtifact.EffectiveKind and the SQL
// COALESCE(NULLIF(kind,”),'oci')), so an item with no kind and no digest
// must be refused — not accepted as some unknown non-OCI ecosystem.
func TestValidate_EmptyKindIsOCIAndStillNeedsADigest(t *testing.T) {
	a := Artifact{Release: "v1", Items: map[string]Item{
		"api": {Name: "api"}, // no Kind, no Digest
	}}
	if err := a.Validate(); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("an item with no kind must be treated as oci and require a digest, got %v", err)
	}
}

// TestValidate_MiscasedKindIsRefused. "OCI" and "oci " would take the
// non-OCI branch and thereby SKIP the digest requirement — the exact
// bypass migration 00075's kind-shape CHECK exists to close. Closing it
// here too keeps a mis-cased kind from being accepted as a new ecosystem.
func TestValidate_MiscasedKindIsRefused(t *testing.T) {
	for _, bad := range []string{"OCI", "oci ", "Oci", "1oci"} {
		a := Artifact{Release: "v1", Items: map[string]Item{
			"api": {Name: "api", Kind: bad},
		}}
		if err := a.Validate(); !errors.Is(err, ErrInvalidArtifact) {
			t.Errorf("kind %q was accepted; a mis-cased kind takes the non-oci branch and "+
				"silently skips the digest requirement", bad)
		}
	}
}

// TestValidate_NonOCIWithDigestIsRefused. A package carrying a digest
// would make "is this digest referenced by any release" answerable "yes"
// by something that is not an image — and a caller pinning a container
// spec could write it into a pod.
func TestValidate_NonOCIWithDigestIsRefused(t *testing.T) {
	a := Artifact{Release: "v1", Items: map[string]Item{
		"pkg": {Name: "pkg", Kind: KindNPM, Version: "1.0.0", Digest: "sha256:" + strings.Repeat("ab", 32)},
	}}
	if err := a.Validate(); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("a non-oci item carrying a digest was accepted: %v", err)
	}
}

// TestValidate_UnaddressableItemIsRefused is the web-runtime failure
// 00075 was written for: a release that NAMES an artifact nobody can
// install. Version and uri are both empty, so nothing can fetch it.
func TestValidate_UnaddressableItemIsRefused(t *testing.T) {
	a := Artifact{Release: "v1", Items: map[string]Item{
		"web-runtime": {Name: "web-runtime", Kind: KindNPM, Integrity: "sha512-abc=="},
	}}
	err := a.Validate()
	if !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("an unaddressable item was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing can fetch it") {
		t.Errorf("error %q does not explain the item is unfetchable", err)
	}
}

// TestValidate_IntegrityIsNotRequired pins the deliberate asymmetry:
// addressability is required, verification is not. Some publish paths
// learn the integrity hash only after the registry accepts the upload,
// and an unaddressable artifact is unusable NOW whereas an unverified one
// is merely unverified.
func TestValidate_IntegrityIsNotRequired(t *testing.T) {
	a := Artifact{Release: "v1", Items: map[string]Item{
		"pkg": {Name: "pkg", Kind: KindNPM, Version: "1.0.0"},
	}}
	if err := a.Validate(); err != nil {
		t.Fatalf("integrity must not be required: %v", err)
	}
}

// TestPinnedDigest_NonOCIAlwaysSaysNo is the guard that keeps kinds from
// leaking into the image path. A caller pinning images must get "no"
// rather than a value it might write into a pod — so callers skip non-OCI
// items for free, with no call site having to remember to check Kind.
func TestPinnedDigest_NonOCIAlwaysSaysNo(t *testing.T) {
	// Deliberately constructed with a digest it should not have, to prove
	// the guard is on KIND and not merely on emptiness.
	it := Item{Name: "pkg", Kind: KindNPM, Digest: "sha256:" + strings.Repeat("ab", 32)}
	if d, ok := it.PinnedDigest(); ok {
		t.Errorf("PinnedDigest returned (%q, true) for an npm item; a package has no digest a "+
			"container spec could be pinned with", d)
	}
}

// TestValidate_UnknownKindRoundTripsAndFailsClosed. Kinds are OPEN by
// design so an older forge reading a newer artifact does not corrupt it —
// but an unrecognised kind must FAIL CLOSED: carried, never guessed at as
// oci.
func TestValidate_UnknownKindRoundTripsAndFailsClosed(t *testing.T) {
	it := Item{Name: "thing", Kind: "cargo", Version: "1.0.0"}
	a := Artifact{Release: "v1", Items: map[string]Item{"thing": it}}
	if err := a.Validate(); err != nil {
		t.Fatalf("an unknown but well-formed kind must round-trip: %v", err)
	}
	if it.EffectiveKind() != "cargo" {
		t.Errorf("EffectiveKind = %q, want the kind preserved verbatim", it.EffectiveKind())
	}
	if _, ok := it.PinnedDigest(); ok {
		t.Error("an unknown kind must not be pinnable as an image — fail closed, never guess oci")
	}
}
