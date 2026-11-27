package agentua

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
)

func mustParseFile(t *testing.T, name string) []netip.Prefix {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	p, err := ParseRanges(f)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseRanges(t *testing.T) {
	p := mustParseFile(t, "testdata/ranges.json")
	want := []string{"192.0.2.0/24", "198.51.100.128/25", "2001:db8:4801:10::/64"}
	if len(p) != len(want) {
		t.Fatalf("got %v, want %v", p, want)
	}
	for i := range want {
		if p[i].String() != want[i] {
			t.Errorf("prefix %d = %s, want %s", i, p[i], want[i])
		}
	}
	// Bing's published file uses CRLF line endings.
	if got := mustParseFile(t, "testdata/bingbot-crlf.json"); len(got) != 2 {
		t.Fatalf("bingbot: %v", got)
	}
}

func TestParseRangesRejectsBadInput(t *testing.T) {
	for _, in := range []string{
		"",
		"<!DOCTYPE html><html></html>",
		`{"prefixes":[]}`,
		`{"prefixes":[{"ipv4Prefix":"not-a-prefix"}]}`,
		`{"prefixes":[{"ipv4Prefix":"192.0.2.1"}]}`,
		`{"prefixes":"nope"}`,
	} {
		if p, err := ParseRanges(strings.NewReader(in)); err == nil {
			t.Errorf("ParseRanges(%q) = %v, want an error", in, p)
		}
	}
}

func TestVerifier(t *testing.T) {
	v := NewVerifier(map[string][]netip.Prefix{"GPTBot": mustParseFile(t, "testdata/ranges.json")})
	cases := []struct {
		agent, ip string
		want      bool
	}{
		{"GPTBot", "192.0.2.77", true},
		{"GPTBot", "198.51.100.200", true},
		{"GPTBot", "198.51.100.10", false},
		{"GPTBot", "203.0.113.5", false},
		{"GPTBot", "2001:db8:4801:10::abcd", true},
		{"GPTBot", "2001:db8:4801:11::1", false},
		{"GPTBot", "::ffff:192.0.2.9", true}, // IPv4-mapped
		{"ClaudeBot", "192.0.2.77", false},   // no ranges: never verified
		{"", "192.0.2.77", false},
	}
	for _, tc := range cases {
		if got := v.Verified(tc.agent, netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("Verified(%s, %s) = %v, want %v", tc.agent, tc.ip, got, tc.want)
		}
	}
	if v.Verified("GPTBot", netip.Addr{}) {
		t.Error("invalid addr verified")
	}
	var nilV *Verifier
	if nilV.Verified("GPTBot", netip.MustParseAddr("192.0.2.1")) {
		t.Error("nil verifier verified")
	}
	v.Set("GPTBot", []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")})
	if v.Verified("GPTBot", netip.MustParseAddr("192.0.2.77")) || !v.Verified("GPTBot", netip.MustParseAddr("203.0.113.5")) {
		t.Error("Set did not swap the ranges")
	}
}

func TestVerifierConcurrent(t *testing.T) {
	v := NewVerifier(nil)
	p := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	ip := netip.MustParseAddr("192.0.2.1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); v.Set("GPTBot", p) }()
		go func() { defer wg.Done(); _ = v.Verified("GPTBot", ip) }()
	}
	wg.Wait()
	if !v.Verified("GPTBot", ip) {
		t.Fatal("not verified after concurrent sets")
	}
}

func TestFetchRanges(t *testing.T) {
	body, err := os.ReadFile("testdata/ranges.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.json":
			_, _ = w.Write(body)
		case "/big.json":
			_, _ = w.Write([]byte(`{"prefixes":[` + strings.Repeat(" ", MaxRangesBytes) + `]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if p, err := FetchRanges(ctx, srv.Client(), srv.URL+"/ok.json"); err != nil || len(p) != 3 {
		t.Fatalf("ok: %v %v", p, err)
	}
	if _, err := FetchRanges(ctx, srv.Client(), srv.URL+"/missing.json"); err == nil {
		t.Fatal("404 accepted")
	}
	if _, err := FetchRanges(ctx, srv.Client(), srv.URL+"/big.json"); err == nil {
		t.Fatal("oversized body accepted")
	}
	if _, err := FetchRanges(ctx, nil, "://bad"); err == nil {
		t.Fatal("bad URL accepted")
	}
}

// rtFunc serves every request in-process, so Refresh runs with no network.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRefresh(t *testing.T) {
	body, err := os.ReadFile("testdata/ranges.json")
	if err != nil {
		t.Fatal(err)
	}
	fetches := map[string]int{}
	var mu sync.Mutex
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		fetches[r.URL.String()]++
		mu.Unlock()
		rec := httptest.NewRecorder()
		if strings.Contains(r.URL.Host, "bing.com") {
			rec.WriteHeader(http.StatusServiceUnavailable)
		} else {
			_, _ = rec.Write(body)
		}
		return rec.Result(), nil
	})}
	keep := []netip.Prefix{netip.MustParsePrefix("157.55.39.0/24")}
	v := NewVerifier(map[string][]netip.Prefix{"bingbot": keep})
	if err := v.Refresh(context.Background(), client); err == nil || !strings.Contains(err.Error(), "bing.com") {
		t.Fatalf("Refresh error = %v, want the bing failure", err)
	}
	ip := netip.MustParseAddr("192.0.2.1")
	for _, name := range []string{"GPTBot", "Googlebot", "Googlebot-Image", "Applebot", "PerplexityBot"} {
		if !v.Verified(name, ip) {
			t.Errorf("%s not verified after Refresh", name)
		}
	}
	if v.Verified("ClaudeBot", ip) {
		t.Error("ClaudeBot has no published ranges but was verified")
	}
	if !v.Verified("bingbot", netip.MustParseAddr("157.55.39.5")) {
		t.Error("a failed fetch dropped bingbot's previous ranges")
	}
	for u, n := range fetches {
		if n != 1 {
			t.Errorf("%s fetched %d times, want once", u, n)
		}
	}
}
