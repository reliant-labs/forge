package authhooks

import (
	"container/list"
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/reliant-labs/forge/pkg/observe"
)

// PathPrefix is where the hook endpoints live; Path appends the provider.
const PathPrefix = "/_forge/auth/hooks/"

// MaxBodyBytes is the largest payload the endpoint reads. A provider's auth
// event is a few hundred bytes; an Auth0 Log Stream batch a few KiB.
const MaxBodyBytes = 64 << 10

// The dedupe memory's bounds: a key seen within DedupeTTL is recorded once,
// and past DedupeEntries keys the oldest are forgotten first.
const (
	DedupeEntries = 100_000
	DedupeTTL     = 24 * time.Hour
)

// Path is the conventional hook endpoint for provider: PathPrefix +
// provider. forge mounts nothing itself; the app mounts the handler there
// (or anywhere it likes) and gives the provider that URL.
func Path(provider string) string { return PathPrefix + provider }

// Event is one auth event a provider reported, normalised to forge's
// vocabulary.
type Event struct {
	// Kind is observe.AuthSignedUp, observe.AuthSignedIn or
	// observe.AuthSignedOut.
	Kind observe.AuthEvent
	// UserID is the provider's opaque subject: the same value its tokens
	// carry as sub. observe.TrackAuth refuses an id with '@'.
	UserID string
	// At is when the provider says it happened; zero when it does not say.
	At time.Time
	// Method is how the person authenticated; "" when the provider does not
	// say.
	Method observe.AuthMethod
	// Social names the social login when Method is observe.AuthMethodSocial.
	Social string
	// Verified says whether a new account's email is verified (signed_up
	// only); nil when the provider does not say.
	Verified *bool
	// DedupeKey identifies the event at the provider (a session id, a log
	// id, a webhook id), so a retried delivery is recorded once. It is held
	// in memory only, never recorded.
	DedupeKey string
}

// Source is one provider's side of the hook: how its deliveries prove who
// sent them, and how its payloads read as Events.
type Source interface {
	// Name is the provider ("supabase", "clerk", "auth0"): the route's last
	// segment and the provider the events are recorded under.
	Name() string
	// Verify proves the delivery came from the provider with secret (a
	// bearer token, or a signing key over body). Any error is answered 401
	// with no detail.
	Verify(r *http.Request, body []byte, secret string) error
	// Events normalises a verified payload. A payload of a type the source
	// does not map is nil, nil: answered 2xx and recorded as nothing.
	Events(r *http.Request, body []byte) ([]Event, error)
}

// Options configure a hook endpoint.
type Options struct {
	// Secret authenticates the provider's deliveries: the bearer token
	// Supabase's triggers and an Auth0 Log Stream send, or Clerk's Svix
	// signing secret ("whsec_…"). The app reads it from wherever it keeps
	// secrets. Empty refuses every delivery (401), with one ERROR log.
	Secret string
	// Allow is the app's own rule for whether an event is recorded — a
	// per-user analytics opt-out, say. It runs after verification and
	// dedupe; nil records every event. It must not block for long: the
	// provider is waiting on the answer.
	Allow func(ctx context.Context, ev Event) bool
	// Logger is what the events are recorded through (they are
	// product-event log records); nil is the process's slog.Default().
	Logger *slog.Logger
	// RateLimit bounds the deliveries per second the endpoint accepts,
	// with Burst; past it the answer is 429, which every provider retries.
	// Zero is 50 per second with a burst of 200; negative is no bound.
	RateLimit float64
	Burst     int
}

// Handler serves a hook endpoint for any Source (Supabase, Clerk and Auth0
// build theirs with this): POST only; the body read up to
// MaxBodyBytes; verified by src; normalised; deduped; and each event
// recorded with observe.TrackAuth under src.Name() as the provider.
//
// It never blocks the provider on a failure of its own: a verified payload
// it cannot read is answered 204 and logged at WARN, once per handler. Server
// auth events are not gated by FORGE_PRODUCT_EVENTS: an app that wants none
// mounts no handler.
//
// forge mounts nothing: the app mounts the handler on its own mux, outside
// its auth chain (the provider holds no user token; the secret authenticates
// it), conventionally at Path(src.Name()).
func Handler(src Source, opts Options) http.Handler {
	if src == nil {
		panic("authhooks.Handler: nil Source")
	}
	h := &handler{
		src:    src,
		secret: opts.Secret,
		logger: opts.Logger,
		allow:  opts.Allow,
		seen:   newDedupe(DedupeEntries, DedupeTTL),
		now:    time.Now,
	}
	switch {
	case opts.RateLimit == 0:
		h.limiter = rate.NewLimiter(50, 200)
	case opts.RateLimit > 0:
		burst := opts.Burst
		if burst < 1 {
			burst = 1
		}
		h.limiter = rate.NewLimiter(rate.Limit(opts.RateLimit), burst)
	}
	if h.secret == "" {
		l := h.logger
		if l == nil {
			l = slog.Default()
		}
		l.Error("auth hook has no secret: every delivery is refused", slog.String("provider", src.Name()))
	}
	return h
}

type handler struct {
	src     Source
	secret  string
	allow   func(context.Context, Event) bool
	logger  *slog.Logger
	seen    *dedupe
	limiter *rate.Limiter
	now     func() time.Time

	warnedUnreadable sync.Once
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	if h.limiter != nil && !h.limiter.Allow() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil || len(body) > MaxBodyBytes {
		unauthorized(w)
		return
	}
	if err := h.src.Verify(r, body, h.secret); err != nil {
		unauthorized(w)
		return
	}
	ctx := r.Context()
	if h.logger != nil {
		ctx = observe.WithLogger(ctx, h.logger)
	}
	events, err := h.src.Events(r, body)
	if err != nil {
		h.warnedUnreadable.Do(func() {
			observe.FromContext(ctx).WarnContext(ctx, "auth hook payload not understood; recorded nothing",
				slog.String("provider", h.src.Name()), slog.String("error", err.Error()))
		})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, ev := range events {
		h.record(ctx, ev)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) record(ctx context.Context, ev Event) {
	if ev.DedupeKey != "" && !h.seen.first(string(ev.Kind)+"\x00"+ev.DedupeKey, h.now()) {
		return
	}
	if h.allow != nil && !h.allow(ctx, ev) {
		return
	}
	opts := []observe.AuthOption{observe.WithProvider(h.src.Name())}
	if ev.Kind == observe.AuthSignedIn {
		opts = append(opts, observe.WithKind(observe.AuthKindCredential))
	}
	if ev.Method != "" {
		opts = append(opts, observe.WithMethod(ev.Method))
		if ev.Social != "" {
			opts = append(opts, observe.WithSocialProvider(ev.Social))
		}
	}
	if ev.Verified != nil {
		opts = append(opts, observe.WithVerified(*ev.Verified))
	}
	observe.TrackAuth(ctx, ev.Kind, ev.UserID, opts...)
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

// dedupe is a bounded, time-limited memory of the keys already recorded.
type dedupe struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	order *list.List // front = newest; values are *dedupeEntry
	index map[string]*list.Element
}

type dedupeEntry struct {
	key string
	at  time.Time
}

func newDedupe(max int, ttl time.Duration) *dedupe {
	return &dedupe{max: max, ttl: ttl, order: list.New(), index: make(map[string]*list.Element)}
}

// first reports whether key has not been seen within ttl of now, and
// remembers it.
func (d *dedupe) first(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Forget what has expired, oldest first.
	for e := d.order.Back(); e != nil; e = d.order.Back() {
		if now.Sub(e.Value.(*dedupeEntry).at) < d.ttl {
			break
		}
		delete(d.index, e.Value.(*dedupeEntry).key)
		d.order.Remove(e)
	}
	if _, ok := d.index[key]; ok {
		return false
	}
	d.index[key] = d.order.PushFront(&dedupeEntry{key: key, at: now})
	for d.order.Len() > d.max {
		e := d.order.Back()
		delete(d.index, e.Value.(*dedupeEntry).key)
		d.order.Remove(e)
	}
	return true
}
