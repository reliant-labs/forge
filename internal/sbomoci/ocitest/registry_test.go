package ocitest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

func repoOf(t *testing.T, r *Registry, name string) *remote.Repository {
	t.Helper()
	repo, err := remote.NewRepository(r.Host() + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true
	return repo
}

func do(t *testing.T, r *Registry, method, path string, body io.Reader, headers ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+r.Host()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestHostIsPlainHTTPLoopback(t *testing.T) {
	r := New(t, nil)
	if !strings.HasPrefix(r.Host(), "127.0.0.1:") {
		t.Errorf("Host = %q, want a 127.0.0.1 address", r.Host())
	}
	if resp, _ := do(t, r, http.MethodGet, "/v2/", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("/v2/ = %d, want 200: the ping every registry client sends first", resp.StatusCode)
	}
	if resp, _ := do(t, r, http.MethodGet, "/elsewhere", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a path outside /v2/ = %d, want 404", resp.StatusCode)
	}
	if resp, _ := do(t, r, http.MethodGet, "/v2/repo/unknown-route/x", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown route under /v2/ = %d, want 404", resp.StatusCode)
	}
}

func TestPutSubject_StoresAResolvableArtifact(t *testing.T) {
	r := New(t, nil)
	ref := r.PutSubject("team/app", "v1")
	if !strings.HasPrefix(ref, r.Host()+"/team/app@sha256:") {
		t.Fatalf("ref = %q, want <host>/<repo>@<digest>", ref)
	}
	if !slices.Equal(r.Tags("team/app"), []string{"v1"}) {
		t.Errorf("Tags = %v", r.Tags("team/app"))
	}
	if got := r.Tags("other/repo"); len(got) != 0 {
		t.Errorf("a repository nobody pushed to has no tags, got %v", got)
	}

	repo := repoOf(t, r, "team/app")
	desc, err := repo.Resolve(context.Background(), "v1")
	if err != nil {
		t.Fatalf("Resolve by tag: %v", err)
	}
	if !strings.HasSuffix(ref, "@"+desc.Digest.String()) {
		t.Errorf("the returned reference names %s, the tag resolves to %s", ref, desc.Digest)
	}
	byDigest, err := repo.Resolve(context.Background(), desc.Digest.String())
	if err != nil || byDigest.Digest != desc.Digest {
		t.Errorf("Resolve by digest = (%v, %v)", byDigest, err)
	}
	if _, err := repo.Resolve(context.Background(), "missing"); err == nil {
		t.Error("an unknown tag must not resolve")
	}

	rc, err := repo.Fetch(context.Background(), desc)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var m ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.ArtifactType != "application/vnd.forge.staticsite.v1" || len(m.Layers) != 1 {
		t.Errorf("manifest = %+v", m)
	}
	layer, err := content.FetchAll(context.Background(), repo, m.Layers[0])
	if err != nil || string(layer) != "layer of team/app:v1" {
		t.Errorf("layer = (%q, %v): PutSubject must also store the blob its manifest names", layer, err)
	}
}

func TestPush_ThroughTheUploadProtocol(t *testing.T) {
	r := New(t, nil)
	repo := repoOf(t, r, "app")
	ctx := context.Background()

	blob := []byte("a blob")
	desc := content.NewDescriptorFromBytes("application/octet-stream", blob)
	if err := repo.Push(ctx, desc, bytes.NewReader(blob)); err != nil {
		t.Fatalf("pushing a blob: %v", err)
	}
	got, err := content.FetchAll(ctx, repo, desc)
	if err != nil || !bytes.Equal(got, blob) {
		t.Errorf("fetched (%q, %v)", got, err)
	}
	if ok, err := repo.Exists(ctx, desc); err != nil || !ok {
		t.Errorf("Exists = (%v, %v)", ok, err)
	}
	missing := content.NewDescriptorFromBytes("application/octet-stream", []byte("never pushed"))
	if ok, err := repo.Exists(ctx, missing); err != nil || ok {
		t.Errorf("Exists(missing) = (%v, %v)", ok, err)
	}
	if _, err := content.FetchAll(ctx, repo, missing); err == nil {
		t.Error("fetching an unknown blob must fail")
	}

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` +
		ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	mdesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifest)
	if err := repo.Push(ctx, mdesc, bytes.NewReader(manifest)); err != nil {
		t.Fatalf("pushing a manifest: %v", err)
	}
	if err := repo.Tag(ctx, mdesc, "latest"); err != nil {
		t.Fatalf("tagging: %v", err)
	}
	if !slices.Equal(r.Tags("app"), []string{"latest"}) {
		t.Errorf("Tags = %v", r.Tags("app"))
	}
	reqs := strings.Join(r.Requests(), "\n")
	for _, want := range []string{"POST /v2/app/blobs/uploads/", "PUT /v2/app/manifests/"} {
		if !strings.Contains(reqs, want) {
			t.Errorf("Requests lacks %q:\n%s", want, reqs)
		}
	}
}

func TestUpload_RefusesBytesThatDoNotMatchTheirDigest(t *testing.T) {
	r := New(t, nil)
	resp, _ := do(t, r, http.MethodPut, "/v2/app/blobs/uploads/1?digest="+digest.FromString("expected").String(), strings.NewReader("different"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a mismatched upload = %d, want 400 DIGEST_INVALID", resp.StatusCode)
	}
	resp, _ = do(t, r, http.MethodPut, "/v2/app/blobs/uploads/1?digest=not-a-digest", strings.NewReader("x"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unparseable digest = %d, want 400", resp.StatusCode)
	}
	if resp, _ := do(t, r, http.MethodDelete, "/v2/app/blobs/uploads/1", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE on an upload = %d, want 405", resp.StatusCode)
	}
}

func TestBlobEndpoint(t *testing.T) {
	r := New(t, nil)
	blob := []byte("payload")
	d := digest.FromBytes(blob)
	if resp, _ := do(t, r, http.MethodPut, "/v2/app/blobs/uploads/1?digest="+d.String(), bytes.NewReader(blob)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	resp, body := do(t, r, http.MethodGet, "/v2/app/blobs/"+d.String(), nil)
	if resp.StatusCode != http.StatusOK || string(body) != "payload" || resp.Header.Get("Docker-Content-Digest") != d.String() {
		t.Errorf("GET = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	resp, body = do(t, r, http.MethodHead, "/v2/app/blobs/"+d.String(), nil)
	if resp.StatusCode != http.StatusOK || len(body) != 0 || resp.Header.Get("Content-Length") != "7" {
		t.Errorf("HEAD = %d %q len=%s: headers only", resp.StatusCode, body, resp.Header.Get("Content-Length"))
	}
	if resp, _ := do(t, r, http.MethodGet, "/v2/app/blobs/sha256:"+strings.Repeat("0", 64), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown blob = %d, want 404", resp.StatusCode)
	}
	if resp, _ := do(t, r, http.MethodGet, "/v2/app/blobs/not-a-digest", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed digest = %d, want 400", resp.StatusCode)
	}
}

func TestCorruptBlob_ServesDifferentBytesForOneDigest(t *testing.T) {
	var corrupt digest.Digest
	r := New(t, func(r *Registry) { r.CorruptBlob = digest.FromBytes([]byte("layer of app:v1")) })
	corrupt = r.CorruptBlob
	r.PutSubject("app", "v1")
	_, body := do(t, r, http.MethodGet, "/v2/app/blobs/"+corrupt.String(), nil)
	if string(body) != "LAYER OF APP:V1" {
		t.Errorf("served %q, want the bytes altered so the digest no longer matches", body)
	}
	if digest.FromBytes(body) == corrupt {
		t.Error("a corrupted blob must not hash to its own digest")
	}
}

func TestManifestEndpoint_DeleteAndRefusal(t *testing.T) {
	t.Run("delete removes the manifest and its tags, and is counted", func(t *testing.T) {
		r := New(t, nil)
		ref := r.PutSubject("app", "v1")
		d := ref[strings.LastIndex(ref, "@")+1:]
		resp, _ := do(t, r, http.MethodDelete, "/v2/app/manifests/"+d, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("DELETE = %d", resp.StatusCode)
		}
		if len(r.Tags("app")) != 0 {
			t.Errorf("a deleted manifest keeps its tag: %v", r.Tags("app"))
		}
		if resp, _ := do(t, r, http.MethodGet, "/v2/app/manifests/v1", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET after delete = %d, want 404", resp.StatusCode)
		}
		if resp, _ := do(t, r, http.MethodDelete, "/v2/app/manifests/"+d, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("deleting twice = %d, want 404", resp.StatusCode)
		}
		if r.DeleteCount() != 2 {
			t.Errorf("DeleteCount = %d, want both attempts counted", r.DeleteCount())
		}
	})

	t.Run("RefuseDelete is the hosted tenant token", func(t *testing.T) {
		r := New(t, func(r *Registry) { r.RefuseDelete = true })
		ref := r.PutSubject("app", "v1")
		d := ref[strings.LastIndex(ref, "@")+1:]
		if resp, _ := do(t, r, http.MethodDelete, "/v2/app/manifests/"+d, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("DELETE = %d, want 401", resp.StatusCode)
		}
		if r.DeleteCount() != 1 || len(r.Tags("app")) != 1 {
			t.Errorf("a refused delete is counted (%d) and removes nothing (%v)", r.DeleteCount(), r.Tags("app"))
		}
	})

	t.Run("an unsupported verb", func(t *testing.T) {
		r := New(t, nil)
		if resp, _ := do(t, r, http.MethodPatch, "/v2/app/manifests/v1", nil); resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("PATCH = %d, want 405", resp.StatusCode)
		}
	})
}

func attach(t *testing.T, r *Registry, repo *remote.Repository, subject ocispec.Descriptor, artifactType string) ocispec.Descriptor {
	t.Helper()
	m := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest, ArtifactType: artifactType, Config: ocispec.DescriptorEmptyJSON,
		Layers: []ocispec.Descriptor{}, Subject: &subject,
	}
	m.SchemaVersion = 2
	raw, _ := json.Marshal(m)
	desc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, raw)
	if err := repo.Push(context.Background(), ocispec.DescriptorEmptyJSON, bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)); err != nil && !strings.Contains(err.Error(), "exists") {
		t.Fatal(err)
	}
	if err := repo.Push(context.Background(), desc, bytes.NewReader(raw)); err != nil {
		t.Fatalf("attaching: %v", err)
	}
	return desc
}

func TestReferrers(t *testing.T) {
	ctx := context.Background()

	t.Run("without the referrers API the route is 404 and no header advertises a subject", func(t *testing.T) {
		r := New(t, nil)
		ref := r.PutSubject("app", "v1")
		subject := ref[strings.LastIndex(ref, "@")+1:]
		resp, _ := do(t, r, http.MethodGet, "/v2/app/referrers/"+subject, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("referrers = %d, want 404 (distribution v3 / GHCR)", resp.StatusCode)
		}
		repo := repoOf(t, r, "app")
		sd, _ := repo.Resolve(ctx, "v1")
		put, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.DescriptorEmptyJSON, Subject: &sd})
		resp, _ = do(t, r, http.MethodPut, "/v2/app/manifests/sha256-fallback", bytes.NewReader(put), "Content-Type", ocispec.MediaTypeImageManifest)
		if resp.Header.Get("OCI-Subject") != "" {
			t.Errorf("a registry without the API must not send OCI-Subject, got %q", resp.Header.Get("OCI-Subject"))
		}
	})

	t.Run("with the API it indexes subjects, filters by artifact type and reports the subject", func(t *testing.T) {
		r := New(t, func(r *Registry) { r.ReferrersAPI = true })
		r.PutSubject("app", "v1")
		repo := repoOf(t, r, "app")
		sd, err := repo.Resolve(ctx, "v1")
		if err != nil {
			t.Fatal(err)
		}
		sbom := attach(t, r, repo, sd, "application/vnd.cyclonedx+json")
		attach(t, r, repo, sd, "application/vnd.example.other")

		all, err := registryReferrers(ctx, repo, sd, "")
		if err != nil || len(all) != 2 {
			t.Fatalf("referrers = (%v, %v), want both attachments", all, err)
		}
		only, err := registryReferrers(ctx, repo, sd, "application/vnd.cyclonedx+json")
		if err != nil || len(only) != 1 || only[0].Digest != sbom.Digest || only[0].ArtifactType != "application/vnd.cyclonedx+json" {
			t.Errorf("filtered referrers = (%v, %v), want only the SBOM", only, err)
		}
		none, err := registryReferrers(ctx, repo, ocispec.Descriptor{Digest: digest.FromString("nobody refers to me")}, "")
		if err != nil || len(none) != 0 {
			t.Errorf("referrers of an unreferenced subject = (%v, %v)", none, err)
		}

		raw, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.DescriptorEmptyJSON, Subject: &sd})
		resp, _ := do(t, r, http.MethodPut, "/v2/app/manifests/extra", bytes.NewReader(raw), "Content-Type", ocispec.MediaTypeImageManifest)
		if resp.Header.Get("OCI-Subject") != sd.Digest.String() {
			t.Errorf("OCI-Subject = %q, want %q", resp.Header.Get("OCI-Subject"), sd.Digest)
		}
	})

	t.Run("NullManifests reproduces GAR's null list for an empty answer", func(t *testing.T) {
		r := New(t, func(r *Registry) { r.ReferrersAPI, r.NullManifests = true, true })
		d := digest.FromString("subject")
		_, body := do(t, r, http.MethodGet, "/v2/app/referrers/"+d.String(), nil)
		if !strings.Contains(string(body), `"manifests":null`) {
			t.Errorf("body = %s, want a null manifests list", body)
		}
		r2 := New(t, func(r *Registry) { r.ReferrersAPI = true })
		_, body = do(t, r2, http.MethodGet, "/v2/app/referrers/"+d.String(), nil)
		if !strings.Contains(string(body), `"manifests":[]`) {
			t.Errorf("body = %s, want an empty (non-null) manifests list", body)
		}
	})

	t.Run("a malformed subject digest", func(t *testing.T) {
		r := New(t, func(r *Registry) { r.ReferrersAPI = true })
		if resp, _ := do(t, r, http.MethodGet, "/v2/app/referrers/not-a-digest", nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("referrers of a malformed digest = %d, want 400", resp.StatusCode)
		}
	})
}

func registryReferrers(ctx context.Context, repo *remote.Repository, subject ocispec.Descriptor, artifactType string) ([]ocispec.Descriptor, error) {
	var out []ocispec.Descriptor
	err := repo.Referrers(ctx, subject, artifactType, func(refs []ocispec.Descriptor) error {
		out = append(out, refs...)
		return nil
	})
	return out, err
}

func TestLoseIndexWrites_TheFallbackTagKeepsItsOldIndex(t *testing.T) {
	r := New(t, nil)
	r.LoseIndexWrites(1)
	if r.IndexWritesToLose() != 1 {
		t.Fatalf("IndexWritesToLose = %d", r.IndexWritesToLose())
	}
	put := func(body string) {
		raw := []byte(body)
		resp, _ := do(t, r, http.MethodPut, "/v2/app/manifests/sha256-abc", bytes.NewReader(raw), "Content-Type", ocispec.MediaTypeImageIndex)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT = %d", resp.StatusCode)
		}
	}
	first := `{"schemaVersion":2,"manifests":[]}`
	second := `{"schemaVersion":2,"manifests":[],"annotations":{"x":"2"}}`
	put(first) // the tag did not exist yet: this write lands
	put(second)
	// The second write is the one that vanishes, the way a racing writer's older index wins.
	_, body := do(t, r, http.MethodGet, "/v2/app/manifests/sha256-abc", nil)
	if string(body) != first {
		t.Errorf("the tag holds %q, want the OLD index %q after a lost write", body, first)
	}
	if r.IndexWritesToLose() != 0 {
		t.Errorf("IndexWritesToLose = %d, want the owed loss consumed", r.IndexWritesToLose())
	}
	put(second) // the next write lands
	if _, body := do(t, r, http.MethodGet, "/v2/app/manifests/sha256-abc", nil); string(body) != second {
		t.Errorf("after the loss is spent the tag holds %q, want %q", body, second)
	}

	// Only fallback tags (sha256-<hex>) lose writes; an ordinary tag never does.
	r.LoseIndexWrites(5)
	do(t, r, http.MethodPut, "/v2/app/manifests/v1", strings.NewReader(first), "Content-Type", ocispec.MediaTypeImageIndex)
	do(t, r, http.MethodPut, "/v2/app/manifests/v1", strings.NewReader(second), "Content-Type", ocispec.MediaTypeImageIndex)
	if _, body := do(t, r, http.MethodGet, "/v2/app/manifests/v1", nil); string(body) != second {
		t.Errorf("an ordinary tag lost a write: %q", body)
	}
}

func TestWriteErr(t *testing.T) {
	r := New(t, nil)
	resp, body := do(t, r, http.MethodGet, "/v2/app/manifests/missing", nil)
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var parsed struct {
		Errors []struct{ Code, Message string }
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Errors) != 1 || parsed.Errors[0].Code != "MANIFEST_UNKNOWN" || parsed.Errors[0].Message != "manifest_unknown" {
		t.Errorf("error body = %s (%v)", body, err)
	}
}
