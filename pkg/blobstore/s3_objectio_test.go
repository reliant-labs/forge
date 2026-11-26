// Copyright (c) 2026 Reliant Labs

package blobstore_test

import (
	"crypto/md5" //nolint:gosec // an ETag, as S3 computes it; not a security boundary
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/blobstore"
)

// writeRange answers a GET or HEAD of data, honouring a "bytes=a-b" or
// "bytes=a-" Range header as S3 and GCS's XML API do: 206 with a
// Content-Range naming the complete length, and 416 for a range that starts
// at or past the end, even of an empty object.
func writeRange(w http.ResponseWriter, r *http.Request, data []byte) {
	start, end := int64(0), int64(len(data))-1
	rng := r.Header.Get("Range")
	if rng != "" {
		spec, ok := strings.CutPrefix(rng, "bytes=")
		from, to, _ := strings.Cut(spec, "-")
		var err error
		if start, err = strconv.ParseInt(from, 10, 64); !ok || err != nil || start >= int64(len(data)) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidRange</Code></Error>`)
			return
		}
		if to != "" {
			if e, err := strconv.ParseInt(to, 10, 64); err == nil && e < end {
				end = e
			}
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if rng != "" {
		w.WriteHeader(http.StatusPartialContent)
	}
	if r.Method == http.MethodGet {
		_, _ = w.Write(data[start : end+1])
	}
}

// fakeS3Objects is an S3 bucket as S3.Open and S3.PutObject reach it over
// TLS (so minio-go sends a plain body, not a streaming signature): single
// PUTs, ranged GETs, HEAD and DELETE. A PUT is committed only when its
// whole body arrived, as the service does.
type fakeS3Objects struct {
	mu      sync.Mutex
	objects map[string]fakeS3Object
}

type fakeS3Object struct {
	data        []byte
	contentType string
}

func (f *fakeS3Objects) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.URL.Path, "/uploads/")
		if !ok || r.URL.RawQuery != "" && r.Method != http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		o, found := f.objects[key]
		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil || int64(len(data)) != r.ContentLength {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.objects[key] = fakeS3Object{data: data, contentType: r.Header.Get("Content-Type")}
			w.Header().Set("ETag", etag(data))
		case http.MethodGet, http.MethodHead:
			if !found {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code></Error>`)
				}
				return
			}
			w.Header().Set("ETag", etag(o.data))
			w.Header().Set("Last-Modified", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Format(http.TimeFormat))
			w.Header().Set("Content-Type", o.contentType)
			writeRange(w, r, o.data)
		case http.MethodDelete:
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func etag(data []byte) string {
	sum := md5.Sum(data) //nolint:gosec // see the import
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// The S3 backend's Open and PutObject over a fake bucket; the same contract
// holds against a real MinIO in the integration lane, which also covers
// multipart uploads.
func TestS3ObjectIO(t *testing.T) {
	t.Parallel()
	fake := &fakeS3Objects{objects: map[string]fakeS3Object{}}
	srv := fake.serve(t)
	cfg := s3Config(srv.URL)
	cfg.Transport = srv.Client().Transport
	s, err := blobstore.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkObjectIO(t, s, "io")
}
