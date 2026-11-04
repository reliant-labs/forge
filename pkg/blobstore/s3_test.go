// Copyright (c) 2026 Reliant Labs

package blobstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
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

// fakeListServer answers ListObjectsV2 over keys the way S3 does: sorted,
// after start-after or the continuation token, at most max-keys a response.
// It records the largest max-keys it was asked for.
func fakeListServer(t *testing.T, keys []string, askedMaxKeys *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/uploads/" && r.URL.Path != "/uploads" || q.Get("list-type") != "2" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		after := q.Get("start-after")
		if tok := q.Get("continuation-token"); tok != "" {
			after = tok
		}
		maxKeys, err := strconv.Atoi(q.Get("max-keys"))
		if err != nil || maxKeys <= 0 || maxKeys > 1000 {
			maxKeys = 1000
		}
		for {
			old := askedMaxKeys.Load()
			if int32(maxKeys) <= old || askedMaxKeys.CompareAndSwap(old, int32(maxKeys)) {
				break
			}
		}
		var page []string
		truncated := false
		for _, k := range keys {
			if !strings.HasPrefix(k, q.Get("prefix")) || k <= after {
				continue
			}
			if len(page) == maxKeys {
				truncated = true
				break
			}
			page = append(page, k)
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>uploads</Name>`)
		fmt.Fprintf(&b, "<KeyCount>%d</KeyCount><MaxKeys>%d</MaxKeys><IsTruncated>%t</IsTruncated>", len(page), maxKeys, truncated)
		if truncated {
			fmt.Fprintf(&b, "<NextContinuationToken>%s</NextContinuationToken>", page[len(page)-1])
		}
		for _, k := range page {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>1</Size><ETag>"e"</ETag><LastModified>2026-01-02T03:04:05.000Z</LastModified></Contents>`, k)
		}
		b.WriteString("</ListBucketResult>")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, b.String())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// List pages through every object, including a page that ends exactly on
// the last object. It asks for one key past the page, so a single response
// shows whether another page exists. (How many requests minio's iterator
// makes is not asserted: it prefetches the next response concurrently.)
func TestS3ListPagesThroughEveryObject(t *testing.T) {
	t.Parallel()
	all := []string{"t/a", "t/b", "t/c", "t/d", "t/e", "u/x"}
	for _, limit := range []int{1, 2, 3, 5, 6} {
		var askedMaxKeys atomic.Int32
		s, err := blobstore.NewS3(s3Config(fakeListServer(t, all, &askedMaxKeys).URL))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		opts := blobstore.ListOptions{Limit: limit}
		pages := 0
		for ; ; pages++ {
			if pages > len(all) {
				t.Fatalf("limit %d: paging does not terminate", limit)
			}
			page, err := s.List(context.Background(), "t/", opts)
			if err != nil {
				t.Fatalf("limit %d: %v", limit, err)
			}
			if len(page.Objects) > limit {
				t.Fatalf("limit %d: page of %d objects", limit, len(page.Objects))
			}
			got = append(got, keys(page)...)
			if page.Next == "" {
				break
			}
			opts.After = page.Next
		}
		if strings.Join(got, ",") != "t/a,t/b,t/c,t/d,t/e" {
			t.Fatalf("limit %d: paged listing = %v", limit, got)
		}
		if n := int(askedMaxKeys.Load()); n != limit+1 {
			t.Fatalf("limit %d: asked for max-keys %d, want %d", limit, n, limit+1)
		}
	}
}
