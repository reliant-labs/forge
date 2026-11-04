// Copyright (c) 2026 Reliant Labs

package blobstore_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/blobstore"
)

func s3Config(endpoint string) blobstore.S3Config {
	return blobstore.S3Config{
		Endpoint:        endpoint,
		Bucket:          "uploads",
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
		PathStyle:       true,
	}
}

func TestNewS3RefusesIncompleteConfig(t *testing.T) {
	t.Parallel()
	bad := map[string]func(*blobstore.S3Config){
		"no bucket":          func(c *blobstore.S3Config) { c.Bucket = "" },
		"bad bucket":         func(c *blobstore.S3Config) { c.Bucket = "Not_A_Bucket" },
		"no key":             func(c *blobstore.S3Config) { c.AccessKeyID = "" },
		"no secret":          func(c *blobstore.S3Config) { c.SecretAccessKey = "" },
		"no scheme":          func(c *blobstore.S3Config) { c.Endpoint = "localhost:9000" },
		"path":               func(c *blobstore.S3Config) { c.Endpoint = "http://localhost:9000/uploads" },
		"bad public":         func(c *blobstore.S3Config) { c.PublicEndpoint = "ftp://localhost" },
		"unparsable":         func(c *blobstore.S3Config) { c.Endpoint = "http://[::1" },
		"public with a path": func(c *blobstore.S3Config) { c.PublicEndpoint = "http://localhost:9000/x" },
	}
	for name, mutate := range bad {
		cfg := s3Config("http://localhost:9000")
		mutate(&cfg)
		if _, err := blobstore.NewS3(cfg); err == nil {
			t.Errorf("%s: NewS3 accepted %+v", name, cfg)
		}
	}
}

// Presigning is local computation: no request reaches the endpoint, which
// here does not exist.
func TestS3PresignPutSignsTypeAndLengthForThePublicHost(t *testing.T) {
	t.Parallel()
	cfg := s3Config("http://minio.internal:9000")
	cfg.PublicEndpoint = "http://localhost:9000"
	s, err := blobstore.NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.PresignPut(context.Background(), "t1/a b.png", blobstore.PutOptions{ContentType: "image/png", ContentLength: 1234, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPut || u.Host != "localhost:9000" || u.Path != "/uploads/t1/a b.png" {
		t.Fatalf("PresignPut = %s %s, want PUT to the public host, path-style", req.Method, req.URL)
	}
	q := u.Query()
	if got := q.Get("X-Amz-SignedHeaders"); got != "content-length;content-type;host" {
		t.Fatalf("X-Amz-SignedHeaders = %q: the length and type must be signed", got)
	}
	if q.Get("X-Amz-Expires") != "60" || !strings.HasPrefix(q.Get("X-Amz-Credential"), "AKIDEXAMPLE/") {
		t.Fatalf("query = %v", q)
	}
	if req.Headers["Content-Type"] != "image/png" || len(req.Headers) != 1 {
		t.Fatalf("Headers = %v, want only Content-Type (a browser sets Content-Length itself)", req.Headers)
	}
	if _, err := s.PresignPut(context.Background(), "x", blobstore.PutOptions{ContentType: "image/png", ContentLength: 2, MaxBytes: 1}); !errors.Is(err, blobstore.ErrTooLarge) {
		t.Fatalf("over MaxBytes = %v, want ErrTooLarge", err)
	}
	if _, err := s.PresignPut(context.Background(), "../x", blobstore.PutOptions{ContentType: "image/png", ContentLength: 1}); !errors.Is(err, blobstore.ErrInvalidKey) {
		t.Fatalf("bad key = %v, want ErrInvalidKey", err)
	}
}

func TestS3PresignGetDownloadName(t *testing.T) {
	t.Parallel()
	s, err := blobstore.NewS3(s3Config("https://s3.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.PresignGet(context.Background(), "k", blobstore.GetOptions{DownloadName: `résumé "final".pdf`})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(req.URL)
	if u.Scheme != "https" || req.Method != http.MethodGet {
		t.Fatalf("PresignGet = %s %s", req.Method, req.URL)
	}
	want := `attachment; filename="r_sum_ _final_.pdf"; filename*=UTF-8''r%C3%A9sum%C3%A9%20%22final%22.pdf`
	if got := u.Query().Get("response-content-disposition"); got != want {
		t.Fatalf("response-content-disposition = %q, want %q", got, want)
	}
}

// Stat maps the service's 404 to ErrNotFound and keeps every other failure
// a failure.
func TestS3StatMapsNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uploads/present":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", "42")
			w.Header().Set("ETag", `"abc"`)
			w.Header().Set("Last-Modified", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Format(http.TimeFormat))
		case "/uploads/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	s, err := blobstore.NewS3(s3Config(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	o, err := s.Stat(ctx, "present")
	if err != nil || o.Size != 42 || o.ContentType != "image/png" || o.Key != "present" {
		t.Fatalf("Stat(present) = %+v, %v", o, err)
	}
	if _, err := s.Stat(ctx, "missing"); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("Stat(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(ctx, "forbidden"); err == nil || errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("Stat(forbidden) = %v, want a failure that is not ErrNotFound", err)
	}
}
