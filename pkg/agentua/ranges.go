package agentua

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/reliant-labs/forge/pkg/observe"
)

// MaxRangesBytes bounds a fetched range document. The largest published list
// (Googlebot's) is well under 100 KiB.
const MaxRangesBytes = 4 << 20

// ParseRanges reads the shared range format Google, OpenAI, Bing, Perplexity,
// Apple and DuckDuckGo publish:
//
//	{"prefixes":[{"ipv4Prefix":"66.249.64.0/27"},{"ipv6Prefix":"2001:4860:4801:10::/64"}]}
//
// Entries with neither field are skipped; a malformed prefix is an error, and
// so is a document with no prefixes at all (an operator outage served as an
// empty or HTML page must not wipe the last good ranges).
func ParseRanges(r io.Reader) ([]netip.Prefix, error) {
	var doc struct {
		Prefixes []struct {
			IPv4 string `json:"ipv4Prefix"`
			IPv6 string `json:"ipv6Prefix"`
		} `json:"prefixes"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("agentua: decode ranges: %w", err)
	}
	out := make([]netip.Prefix, 0, len(doc.Prefixes))
	for _, p := range doc.Prefixes {
		for _, s := range [2]string{p.IPv4, p.IPv6} {
			if s == "" {
				continue
			}
			pfx, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("agentua: bad prefix %q: %w", s, err)
			}
			out = append(out, pfx.Masked())
		}
	}
	if len(out) == 0 {
		return nil, errors.New("agentua: ranges document has no prefixes")
	}
	return out, nil
}

// Verifier checks IP-range membership, not agent identity. Operators may
// share ranges between agents. It is safe for concurrent use; Set and Refresh
// swap an agent's ranges atomically.
type Verifier struct {
	mu     sync.RWMutex
	ranges map[string][]netip.Prefix // agent Name -> prefixes
}

// NewVerifier returns a Verifier holding ranges, keyed by agent Name. The map
// and slices are copied.
func NewVerifier(ranges map[string][]netip.Prefix) *Verifier {
	v := &Verifier{ranges: make(map[string][]netip.Prefix, len(ranges))}
	for name, p := range ranges {
		v.ranges[name] = append([]netip.Prefix(nil), p...)
	}
	return v
}

// Set replaces agent's ranges.
func (v *Verifier) Set(agent string, prefixes []netip.Prefix) {
	cp := append([]netip.Prefix(nil), prefixes...)
	v.mu.Lock()
	v.ranges[agent] = cp
	v.mu.Unlock()
}

// Verified reports only IP-range membership, not the identity of agent.
// Shared or overlapping ranges cannot distinguish agents or their classes.
//
// Deprecated: use MatchesPublishedRange to make this limitation explicit.
func (v *Verifier) Verified(agent string, ip netip.Addr) bool {
	return v.MatchesPublishedRange(agent, ip)
}

// MatchesPublishedRange reports whether ip belongs to the loaded ranges keyed
// by agent. With Refresh these are the ranges published by the agent's operator;
// with Set or NewVerifier the caller is responsible for the source of the ranges.
// A match is evidence of range membership only: shared egress and overlapping
// ranges do not prove the claimed agent's identity or its AI/search class. Even
// a match must be displayed as "claims to be", not as a verified agent.
// It is false for an agent with no ranges loaded, an invalid ip, or a nil
// Verifier. IPv4-mapped IPv6 addresses are compared as IPv4.
func (v *Verifier) MatchesPublishedRange(agent string, ip netip.Addr) bool {
	if v == nil || agent == "" || !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	v.mu.RLock()
	prefixes := v.ranges[agent]
	v.mu.RUnlock()
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// FetchRanges GETs url and parses it with ParseRanges, reading at most
// MaxRangesBytes. A nil client uses a traced observe.NewHTTPClient with a 30s timeout; ctx bounds the
// request either way.
func FetchRanges(ctx context.Context, client *http.Client, url string) ([]netip.Prefix, error) {
	if client == nil {
		client = observe.NewHTTPClient(observe.HTTPClientOptions{Timeout: 30 * time.Second})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("agentua: ranges request %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentua: fetch ranges %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agentua: fetch ranges %s: HTTP %d", url, resp.StatusCode)
	}
	body := io.LimitReader(resp.Body, MaxRangesBytes+1)
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("agentua: read ranges %s: %w", url, err)
	}
	if len(b) > MaxRangesBytes {
		return nil, fmt.Errorf("agentua: ranges %s exceed %d bytes", url, MaxRangesBytes)
	}
	p, err := ParseRanges(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w (from %s)", err, url)
	}
	return p, nil
}

// Refresh fetches the ranges of every listed agent that publishes them and
// swaps each into v. An agent whose fetch fails keeps its previous ranges; the
// failures are joined into the returned error, so a caller can log them and
// retry later without losing range membership data. Several agents share one URL
// (Googlebot's family), which is fetched once.
func (v *Verifier) Refresh(ctx context.Context, client *http.Client) error {
	fetched := map[string][]netip.Prefix{}
	failed := map[string]bool{}
	var errs []error
	for _, a := range list.Agents {
		if a.RangesURL == "" {
			continue
		}
		if failed[a.RangesURL] {
			continue
		}
		p, ok := fetched[a.RangesURL]
		if !ok {
			var err error
			p, err = FetchRanges(ctx, client, a.RangesURL)
			if err != nil {
				failed[a.RangesURL] = true
				errs = append(errs, err)
				continue
			}
			fetched[a.RangesURL] = p
		}
		v.Set(a.Name, p)
	}
	return errors.Join(errs...)
}
