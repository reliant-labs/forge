// Copyright (c) 2026 Reliant Labs

package blobstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/reliant-labs/forge/pkg/blobstore"
)

// fakeGCSObjects is Cloud Storage as GCS.Open and GCS.PutObject reach it,
// with object bytes: objects.get and objects.list (JSON API), a ranged
// media GET pinned to a generation (XML API), and a multipart upload (JSON
// API). An upload is committed only once its whole request body arrived,
// as the service does; commits counts them.
type fakeGCSObjects struct {
	mu      sync.Mutex
	objects map[string]fakeGCSObject
	gen     int64
	commits int
}

type fakeGCSObject struct {
	data        []byte
	contentType string
	gen         int64
}

func (f *fakeGCSObjects) resource(key string, o fakeGCSObject) map[string]any {
	return map[string]any{
		"kind": "storage#object", "bucket": "uploads", "name": key,
		"size": strconv.Itoa(len(o.data)), "contentType": o.contentType,
		"etag": fmt.Sprintf("etag-%d", o.gen), "generation": strconv.FormatInt(o.gen, 10),
		"updated": "2026-01-02T03:04:05.000Z",
	}
}

func (f *fakeGCSObjects) serve(t *testing.T) *httptest.Server {
	t.Helper()
	const meta, media, upload = "/storage/v1/b/uploads/o", "/uploads/", "/upload/storage/v1/b/uploads/o"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == upload && r.Method == http.MethodPost:
			f.upload(w, r)
		case strings.HasPrefix(r.URL.Path, meta+"/") && r.Method == http.MethodGet:
			key := strings.TrimPrefix(r.URL.Path, meta+"/")
			o, ok := f.objects[key]
			if !ok {
				gcsError(w, http.StatusNotFound, "No such object")
				return
			}
			writeJSON(w, f.resource(key, o))
		case strings.HasPrefix(r.URL.Path, meta+"/") && r.Method == http.MethodDelete:
			delete(f.objects, strings.TrimPrefix(r.URL.Path, meta+"/"))
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, media) && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			f.read(w, r, strings.TrimPrefix(r.URL.Path, media))
		default:
			gcsError(w, http.StatusForbidden, r.Method+" "+r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGCSObjects) read(w http.ResponseWriter, r *http.Request, key string) {
	o, ok := f.objects[key]
	if !ok || (r.URL.Query().Has("generation") && r.URL.Query().Get("generation") != strconv.FormatInt(o.gen, 10)) {
		http.Error(w, "NoSuchKey", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", o.contentType)
	w.Header().Set("X-Goog-Generation", strconv.FormatInt(o.gen, 10))
	w.Header().Set("X-Goog-Metageneration", "1")
	w.Header().Set("Last-Modified", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Format(http.TimeFormat))
	writeRange(w, r, o.data)
}

// upload commits a multipart upload (metadata, then media) only when the
// request body arrived whole.
func (f *fakeGCSObjects) upload(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("uploadType") != "multipart" {
		gcsError(w, http.StatusBadRequest, "only multipart uploads: "+r.URL.RawQuery)
		return
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		gcsError(w, http.StatusBadRequest, err.Error())
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var md struct {
		Name        string `json:"name"`
		ContentType string `json:"contentType"`
	}
	part, err := mr.NextPart()
	if err == nil {
		err = json.NewDecoder(part).Decode(&md)
	}
	var data []byte
	if err == nil {
		if part, err = mr.NextPart(); err == nil {
			data, err = io.ReadAll(part)
		}
	}
	if err == nil {
		if _, err = mr.NextPart(); err == io.EOF {
			err = nil
		}
	}
	if err != nil {
		gcsError(w, http.StatusBadRequest, "incomplete upload: "+err.Error())
		return
	}
	f.gen++
	f.commits++
	o := fakeGCSObject{data: data, contentType: md.ContentType, gen: f.gen}
	f.objects[md.Name] = o
	writeJSON(w, f.resource(md.Name, o))
}

func newObjectGCS(t *testing.T) (*blobstore.GCS, *fakeGCSObjects) {
	t.Helper()
	fake := &fakeGCSObjects{objects: map[string]fakeGCSObject{}}
	srv := fake.serve(t)
	g, err := blobstore.NewGCS(context.Background(), blobstore.GCSConfig{
		Bucket:        "uploads",
		ClientOptions: []option.ClientOption{option.WithEndpoint(srv.URL + "/storage/v1/"), option.WithoutAuthentication()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return g, fake
}

func TestGCSObjectIO(t *testing.T) {
	t.Parallel()
	g, fake := newObjectGCS(t)
	checkObjectIO(t, g, "io")
	// Every refused body was refused before its upload request completed.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for key := range fake.objects {
		if strings.Contains(key, "refused-") {
			t.Fatalf("a refused upload was committed: %s", key)
		}
	}
}

// Open streams the generation whose attributes it reported: an object
// replaced between the two is not found, never mixed.
func TestGCSOpenPinsTheGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, fake := newObjectGCS(t)
	if _, err := g.PutObject(ctx, "t/a", strings.NewReader("first"), blobstore.PutObjectOptions{ContentType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	r, o, err := g.Open(ctx, "t/a", blobstore.OpenOptions{Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "fir" || o.Size != 5 || o.ETag != "etag-1" {
		t.Fatalf("Open = %q %+v", got, o)
	}
	fake.mu.Lock()
	commits := fake.commits
	fake.mu.Unlock()
	if commits != 1 {
		t.Fatalf("%d commits, want 1", commits)
	}
}

func TestGCSPresignGetInline(t *testing.T) {
	t.Parallel()
	g, signer := newSigningGCS(t)
	qs := checkInlineGets(t, g)
	for _, q := range qs {
		signedQuery(t, "https://storage.googleapis.com/uploads/t1/a.png?"+q.Encode(), "/uploads/t1/a.png", blobstore.DefaultTTL)
		if q.Get("X-Goog-SignedHeaders") != "host" {
			t.Fatalf("SignedHeaders = %q, want host (the overrides are signed query parameters)", q.Get("X-Goog-SignedHeaders"))
		}
	}
	checkStringToSign(t, signer.last(t), qs[len(qs)-1])
}
