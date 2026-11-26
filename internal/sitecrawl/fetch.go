package sitecrawl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/netguard"
)

// UserAgentToken is the product token the crawler sends and obeys robots.txt
// for.
const UserAgentToken = "ForgeSiteAudit"

// UserAgent is the crawler's User-Agent header: honest about what it is.
const UserAgent = UserAgentToken + "/1.0 (+https://github.com/reliant-labs/forge)"

// Response is one fetch's outcome. Status is 0 when the fetch failed, and Err
// says why.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	Err    string
}

// Fetcher fetches a URL with method (GET or HEAD), reading at most maxBody
// bytes of the body. It never follows a redirect.
type Fetcher interface {
	Fetch(ctx context.Context, method, rawURL string, maxBody int64) Response
}

// ErrNotServed is the Err of a [HandlerFetcher] fetch of a URL off the site
// it serves.
var ErrNotServed = errors.New("not served by the local build")

// HandlerFetcher answers fetches of one origin (scheme and host) from an
// in-process handler: the build directory is served without a socket. A URL
// on any other host is not fetched (its Err is [ErrNotServed]).
type HandlerFetcher struct {
	// Host is the host the handler answers for, as in a URL ("example.com",
	// "localhost:8080").
	Host    string
	Handler http.Handler
}

// Fetch implements [Fetcher].
func (f HandlerFetcher) Fetch(ctx context.Context, method, rawURL string, maxBody int64) Response {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Response{Err: err.Error()}
	}
	if !strings.EqualFold(u.Host, f.Host) {
		return Response{Err: ErrNotServed.Error()}
	}
	req := httptest.NewRequestWithContext(ctx, method, u.RequestURI(), nil)
	req.Host = u.Host
	req.Header.Set("User-Agent", UserAgent)
	rec := httptest.NewRecorder()
	f.Handler.ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	if int64(len(body)) > maxBody {
		body = body[:maxBody]
	}
	return Response{Status: rec.Code, Header: rec.Result().Header, Body: body}
}

// HTTPFetcher fetches over the network with a client that follows no
// redirect.
type HTTPFetcher struct {
	Client *http.Client
}

// Fetch implements [Fetcher].
func (f HTTPFetcher) Fetch(ctx context.Context, method, rawURL string, maxBody int64) Response {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return Response{Err: err.Error()}
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := f.Client.Do(req)
	if err != nil {
		return Response{Err: fetchError(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Response{Status: resp.StatusCode, Header: resp.Header, Body: body, Err: fetchError(err)}
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: body}
}

// fetchError is a fetch failure without Go's "Get \"url\":" wrapping.
func fetchError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return err.Error()
}

// GuardedClient is the client a URL audit fetches with: pkg/netguard's
// dialer (no loopback, private, link-local or metadata address, checked after
// DNS), no ambient proxy, and no redirect followed — a 3xx is returned as
// the response. Ports 80 and 443 are admitted, plus the site's own port.
func GuardedClient(site *url.URL, timeout time.Duration) *http.Client {
	ports := []uint16{80, 443}
	if p, err := strconv.ParseUint(site.Port(), 10, 16); err == nil && p != 80 && p != 443 {
		ports = append(ports, uint16(p))
	}
	policy := netguard.Policy{Ports: ports}
	return &http.Client{
		Timeout:   timeout,
		Transport: policy.Transport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// RefusedHost explains, when it is refused, why a site's host cannot be
// audited over the network: this machine, a private or reserved address, an
// internal name (netguard's hostname rules). It is the early, readable
// refusal; the dialer is the boundary.
func RefusedHost(site *url.URL) error {
	if reason, refused := netguard.RefusedHost(site.Hostname()); refused {
		return fmt.Errorf("%s cannot be audited over the network: %s (the URL audit fetches only public addresses)", site.Host, reason)
	}
	return nil
}
