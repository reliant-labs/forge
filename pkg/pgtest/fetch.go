package pgtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/reliant-labs/forge/pkg/observe"
)

// Fetching the postgres binary, with retry and an honest error.
//
// embedded-postgres downloads its binary archive from Maven Central the first
// time a cache is cold, and it answers ANY non-200 from that fetch with
//
//	no version found matching 18.3.0
//
// (fergusstrange/embedded-postgres v1.34.0, remote_fetch.go). A 503 from the
// registry, a 429, a CDN hiccup — every one of them reads as "this postgres
// version does not exist", which sends whoever is reading it looking for a
// version-pinning bug that is not there. control-plane's CI hit exactly that
// on "Verify Generated Code" (`forge generate` → "start embedded postgres: no
// version found matching 18.3.0") and passed on a plain rerun.
//
// The library offers no hook into its fetch: the strategy is unexported, and
// it runs only when the cached archive is missing. What it DOES expose is the
// repository base URL. So forge points that at a loopback relay for the one
// Start call that may fetch, and the relay is where retry and diagnosis live:
//
//   - it forwards each GET to the real repository, retrying a transient
//     failure (a transport error, 408, 425, 429, 5xx) with bounded backoff;
//   - when Maven Central still has not served the file — a 404 included —
//     it asks an independent mirror of Maven Central before giving up (see
//     mavenCentralMirrorURL for why a 404 is not taken at its word);
//   - it records the last upstream failure — URL, status, attempts — so a
//     Start that still fails is reported as the fetch failure it was;
//   - everything else stays the library's: it still computes the archive
//     name for this OS/arch/version, verifies the published sha256, and
//     writes its own cache. forge re-derives none of that, so nothing here
//     drifts when the library changes its naming.
//
// # Pre-warming the cache in CI
//
// A warm cache never fetches at all, which removes the registry from the
// critical path entirely. The archive lands at
//
//	<os.UserCacheDir()>/forge/embedded-postgres/embedded-postgres-binaries-<os>-<arch>-<version>.txz
//
// which is ~/.cache/forge/embedded-postgres on a Linux runner. Cache that
// directory, and key the saved entry on what is IN it — its file names carry
// every version — not on go.sum alone. A go.sum key holds whichever archives
// the first job to save it happened to fetch, and a hit is never saved again:
// forge's own cache held only the default 18.3.0, so every main run of a
// test that boots postgres 16 fetched 16.9.0 from Maven Central until a 429
// failed it. forge's CI does this in .github/actions/embedded-postgres-cache
// (restore) and .github/actions/embedded-postgres-cache-save (save on main).

// defaultBinaryRepositoryURL is the library's own default (DefaultConfig's
// binaryRepositoryURL). Restated here because the library exposes a setter
// for it but no getter, and the relay has to know where to forward.
const defaultBinaryRepositoryURL = "https://repo1.maven.org/maven2"

// mavenCentralMirrorURL is Google's mirror of Maven Central on Cloud Storage,
// asked when Maven Central has not served a file.
//
// WHY A 404 FROM MAVEN CENTRAL FALLS BACK rather than being final. Maven
// Central's CDN has answered 404 for the published 18.3.0 linux-amd64 archive
// (on Maven Central since 2026-03-01) twice on forge's main: the Windows job
// once, then pkg/alerting and pkg/alerting/pgstore (run 44491105411,
// 2026-11-19), while the same URL answered 200 from elsewhere minutes later.
// A 404 is the registry's "no such file", so retrying the same edge buys
// nothing; a second repository on separate infrastructure (Cloud Storage, not
// Maven Central's CDN) does. It mirrors the same artifacts byte for byte — the
// 18.3.0 archive's .sha256 sidecar is identical on both — and the library
// still checks the archive against that sidecar. Only a 404 from BOTH is
// reported as "no published binary".
const mavenCentralMirrorURL = "https://maven-central.storage-download.googleapis.com/maven2"

// fetchPolicy bounds the retries. Package-level so a test can shrink the
// waits; production never changes it.
//
// Sized for Maven Central's rate limit, the failure CI actually meets: a
// 429 answered four tries spread over 7s (1+2+4s) on forge's main, all
// inside the same throttling window. Six tries backing off 2s to 30s wait up
// to ~60s in all, which outlasts a burst of parallel jobs; each wait is
// jittered so processes that were throttled together do not come back
// together, and a Retry-After the registry sends is honored up to
// maxRetryAfter.
var fetchPolicy = struct {
	attempts      int
	firstDelay    time.Duration
	maxDelay      time.Duration
	maxRetryAfter time.Duration
	perAttempt    time.Duration
	// repositories are asked in order, each with the full retry budget,
	// until one serves the file.
	repositories []string
}{
	attempts:      6,
	firstDelay:    2 * time.Second,
	maxDelay:      30 * time.Second,
	maxRetryAfter: 60 * time.Second,
	perAttempt:    2 * time.Minute,
	repositories:  []string{defaultBinaryRepositoryURL, mavenCentralMirrorURL},
}

// FetchError is a postgres binary download that failed. It replaces the
// library's "no version found matching X", which it returns for any non-200.
type FetchError struct {
	// URL is the upstream artifact the library asked for.
	URL string
	// Status is the last HTTP status the repository returned, 0 when the
	// request never got a response (DNS, connection refused, TLS, timeout).
	Status int
	// Attempts is how many times forge tried.
	Attempts int
	// Err is the last transport error, when Status is 0.
	Err error
	// also backs Also. A pointer, not a slice, so FetchError stays
	// comparable: a slice field would make == on it a compile error for
	// callers, an incompatible change to pkg/ at major 1.
	also *[]*FetchError
}

// Also is what the other repositories asked answered (forge tries Maven
// Central, then a mirror of it), in the order they were asked. The fields of
// e describe the most telling failure: a transient one when there was any,
// so Status is 404 only when every repository answered 404.
func (e *FetchError) Also() []*FetchError {
	if e.also == nil {
		return nil
	}
	return *e.also
}

func (e *FetchError) Error() string {
	if e.Status == http.StatusNotFound {
		// The one status that plausibly DOES mean "no such version" — and
		// only when no repository said otherwise.
		urls := []string{e.URL}
		for _, o := range e.Also() {
			urls = append(urls, o.URL)
		}
		return fmt.Sprintf("download postgres binary: %s returned 404 Not Found — this postgres "+
			"version/platform has no published binary (not a transient failure)", strings.Join(urls, " and "))
	}
	msg := fmt.Sprintf("download postgres binary: fetch of %s failed after %d attempt(s), last: %s. "+
		"This is a registry/network failure, possibly transient — the version exists; re-run. "+
		"To take the registry off the critical path, cache the embedded-postgres archive directory "+
		"(see pkg/pgtest/fetch.go)", e.URL, e.Attempts, e.lastAnswer())
	for _, o := range e.Also() {
		msg += fmt.Sprintf(". Also tried %s: %s", o.URL, o.lastAnswer())
	}
	return msg
}

// lastAnswer is the final thing the repository said, in a few words.
func (e *FetchError) lastAnswer() string {
	switch {
	case e.Status != 0:
		return fmt.Sprintf("HTTP %d %s", e.Status, http.StatusText(e.Status))
	case e.Err != nil:
		return e.Err.Error()
	default:
		return "no response"
	}
}

// fetchFailure folds each repository's failure, in the order they were
// asked, into one FetchError led by the most telling: the first that was not
// a 404, since one transient failure means the file may well exist.
func fetchFailure(failures []*FetchError) *FetchError {
	lead := 0
	for i, f := range failures {
		if f.Status != http.StatusNotFound {
			lead = i
			break
		}
	}
	fe := *failures[lead]
	var also []*FetchError
	for i, f := range failures {
		if i != lead {
			also = append(also, f)
		}
	}
	fe.also = nil
	if len(also) > 0 {
		fe.also = &also
	}
	return &fe
}

func (e *FetchError) Unwrap() error { return e.Err }

// retryableStatus reports whether an HTTP status is worth another attempt.
// Everything 4xx other than the throttling/timeout family is a real answer.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return code >= 500
}

// fetchRelay is the loopback repository the library fetches through.
type fetchRelay struct {
	repositories []string // asked in order; see fetchPolicy.repositories
	client       *http.Client
	srv          *http.Server
	ln           net.Listener

	mu      sync.Mutex
	failure *FetchError // the last artifact GET that never succeeded
}

func startFetchRelay(repositories ...string) (*fetchRelay, error) {
	if len(repositories) == 0 {
		return nil, errors.New("pgtest: start postgres download relay: no repository to fetch from")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("pgtest: start postgres download relay: %w", err)
	}
	r := &fetchRelay{
		client: observe.NewHTTPClient(observe.HTTPClientOptions{Timeout: fetchPolicy.perAttempt}),
		ln:     ln,
	}
	for _, repo := range repositories {
		r.repositories = append(r.repositories, strings.TrimRight(repo, "/"))
	}
	r.srv = &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

// baseURL is what the library is configured with in place of the real
// repository.
func (r *fetchRelay) baseURL() string { return "http://" + r.ln.Addr().String() }

func (r *fetchRelay) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.srv.Shutdown(ctx)
}

// lastFailure is the most recent artifact fetch that never succeeded, or nil.
func (r *fetchRelay) lastFailure() *FetchError {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func (r *fetchRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var failures []*FetchError
	var resp *http.Response
	for _, repo := range r.repositories {
		target := repo + req.URL.Path
		got, attempts, err := r.getWithRetry(req.Context(), target)
		if err == nil && got.StatusCode == http.StatusOK {
			resp = got
			break
		}
		fe := &FetchError{URL: target, Attempts: attempts, Err: err}
		if got != nil {
			fe.Status = got.StatusCode
			_ = got.Body.Close()
		}
		failures = append(failures, fe)
		if req.Context().Err() != nil {
			break // the library gave up; asking the next repository is moot
		}
	}
	if resp == nil {
		fe := fetchFailure(failures)
		// The .sha256 sidecar is optional to the library (it skips the check
		// on a non-200), so only the archive itself is worth reporting.
		if !strings.HasSuffix(req.URL.Path, ".sha256") {
			r.mu.Lock()
			r.failure = fe
			r.mu.Unlock()
		}
		status := fe.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		http.Error(w, fe.Error(), status)
		return
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", fmt.Sprint(resp.ContentLength))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}

// getWithRetry GETs target, retrying transient failures with jittered
// exponential backoff, waiting at least as long as a Retry-After asks. It
// returns the final response (whatever its status) or the final transport
// error, and how many attempts were made.
func (r *fetchRelay) getWithRetry(ctx context.Context, target string) (*http.Response, int, error) {
	backoff := fetchPolicy.firstDelay
	var lastErr error
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, attempt, err
		}
		resp, err := r.client.Do(req)
		switch {
		case err == nil && !retryableStatus(resp.StatusCode):
			return resp, attempt, nil
		case attempt >= fetchPolicy.attempts:
			return resp, attempt, err
		}
		wait := jittered(backoff)
		if err == nil {
			if after, ok := retryAfter(resp.Header, time.Now()); ok {
				wait = max(wait, min(after, fetchPolicy.maxRetryAfter))
			}
			_ = resp.Body.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, attempt, errors.Join(ctx.Err(), lastErr)
		case <-time.After(wait):
		}
		backoff = min(backoff*2, fetchPolicy.maxDelay)
	}
}

// jittered spreads a backoff step over [d/2, d), so processes throttled by
// the same burst retry at different moments instead of in a second burst.
func jittered(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + rand.N(d-half)
}

// retryAfter reads a Retry-After header (RFC 9110 §10.2.3): delay-seconds or
// an HTTP-date. ok is false when there is none or it does not parse; a date
// already past is a zero wait.
func retryAfter(h http.Header, now time.Time) (wait time.Duration, ok bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}

// StartEmbedded builds and starts an embedded postgres from cfg, fetching
// its binary (on a cold cache) through a retrying relay. A download failure
// comes back as a *FetchError naming the URL and the upstream status, never
// as the library's "no version found".
//
// cfg must not set BinaryRepositoryURL: the relay takes that slot and
// forwards to Maven Central, the library's default, then to a mirror of it.
//
// The returned Embedded owns the temp log file the library created for this
// Start (see embeddedlog.go): Stop removes it after the library's final
// flush, and a caller whose server outlives the handle calls Detach. A failed
// Start removes its own file here. No other process's log file is ever
// touched.
func StartEmbedded(cfg embeddedpostgres.Config) (*Embedded, error) {
	relay, err := startFetchRelay(fetchPolicy.repositories...)
	if err != nil {
		return nil, err
	}
	defer relay.close()

	// Serialized so that "the log files this process newly holds open after
	// Start" is exactly this Start's. See embeddedlog.go.
	startMu.Lock()
	defer startMu.Unlock()

	tempRoot := os.TempDir()
	before, beforeErr := ownEmbeddedLogs(tempRoot, ownOpenPaths)

	ep := embeddedpostgres.NewDatabase(cfg.BinaryRepositoryURL(relay.baseURL()))
	startErr := ep.Start()

	// Whatever the outcome, the library has created its file by now.
	var mine []string
	if beforeErr == nil {
		if after, err := ownEmbeddedLogs(tempRoot, ownOpenPaths); err == nil {
			mine = claimStartedLogs(before, after)
		}
	}
	// Reclaim leftovers nobody holds; best-effort and never blocking a start.
	defer sweepStaleEmbeddedLogs(tempRoot, time.Now(), staleEmbeddedLogAge, machineOpenFiles)

	if startErr != nil {
		removeFiles(mine)
		if fe := relay.lastFailure(); fe != nil {
			// The library's own wording ("no version found matching …") is
			// wrong for everything except a 404, so it is dropped rather
			// than wrapped: the FetchError says what actually happened.
			return nil, fe
		}
		return nil, startErr
	}
	return &Embedded{ep: ep, logs: mine}, nil
}
