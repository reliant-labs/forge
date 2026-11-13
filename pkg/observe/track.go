package observe

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/reliant-labs/forge/pkg/authn"
)

// The product-event contract. A product event is one INFO log record whose
// event.name attribute is "product.<name>"; the telemetry pipeline selects on
// that attribute, so the scope and logger name do not matter. The attribute
// keys below are the cross-repo seam: the web runtime, the Go emitter and the
// ClickHouse view must agree on them.
const (
	// EventNamePrefix starts every product event's event.name.
	EventNamePrefix = "product."
	// AttrEventName is the OTel event semconv attribute carrying the name.
	AttrEventName = "event.name"
	// AttrUserID is the authenticated user id; absent for anonymous callers.
	AttrUserID = "user.id"
	// AttrAppClient says which kind of emitter wrote the event.
	AttrAppClient = "app.client"
	// AttrAppVersion is the emitting app's version.
	AttrAppVersion = "app.version"
	// AttrPropPrefix prefixes every event property.
	AttrPropPrefix = "product."

	// AppClientBackend is the app.client value for events emitted by Track.
	AppClientBackend = "backend"

	// PropActivity is the reserved event property that says whose activity
	// an event is. It is a property, so it is stored in product_events'
	// Props (Props['forge.activity']) by every pipeline that stores events,
	// and the log record carries it as product.forge.activity. Unset: the
	// event is the activity of the user it names, which is every event's
	// default.
	PropActivity = "forge.activity"
	// ActivitySystem is PropActivity's value for an event the system
	// recorded about a user rather than one the user caused: a daily billing
	// snapshot attributed to an org's owner, a background job's result, an
	// account the platform created. Active, new and returning user counts
	// ignore such events (pkg/obsquery.ActiveEvidence); every other read of
	// the event is unchanged.
	ActivitySystem = "system"

	// MaxProductProps is the most properties one event keeps; the rest are dropped.
	MaxProductProps = 32
	// MaxProductPropValueLen is the longest property value in characters;
	// longer values are truncated.
	MaxProductPropValueLen = 256
)

// EnvProductEvents switches off the product events forge emits on the app's
// behalf (the generated CRUD handlers' <entity>_created|_updated|_deleted).
// "off" (or "false", "0") turns them off; anything else, or unset, leaves them
// on. KCL renders FORGE_PRODUCT_EVENTS=off when the env declares
// forge.ObservabilitySignals{product_events = False}. Track itself is never
// gated: an event the app's own code emits is a decision that code made.
const EnvProductEvents = "FORGE_PRODUCT_EVENTS"

var eventNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// autoProductEvents reads EnvProductEvents once: the process's environment
// does not change under it, and the check sits on every CRUD write. The read
// goes through otelenv.go's envValue, the one place this package reads the
// environment.
var autoProductEvents = sync.OnceValue(func() bool { return productEventsOn(envValue(EnvProductEvents)) })

// AutoProductEvents reports whether forge's automatic product events are on
// for this process (see EnvProductEvents). The value is read once.
func AutoProductEvents() bool { return autoProductEvents() }

func productEventsOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0":
		return false
	}
	return true
}

var appVersion atomic.Pointer[string]

// SetAppVersion sets the app.version stamped on product events. Setup calls it
// with Config.ServiceVersion; call it directly in a process that does not use
// Setup.
func SetAppVersion(v string) { appVersion.Store(&v) }

// ValidEventName reports whether name is a legal product event name: snake_case,
// starting with a letter, at most 64 characters.
func ValidEventName(name string) bool { return eventNameRE.MatchString(name) }

// Track records one product event: a named, countable thing a user did or that
// happened to them ("project_created", "subscription_started"). It writes an
// INFO record through the context's logger (FromContext) carrying
// event.name="product.<name>", app.client="backend", app.version, user.id when
// ctx holds an authenticated user (the claims forge's auth interceptor
// installs), and each attr as product.<key>.
//
// Properties are bounded the same way the telemetry pipeline bounds them: at
// most MaxProductProps are kept and each value is truncated to
// MaxProductPropValueLen characters. Group attrs are flattened to "group.key".
// Property values are stringified; keep them low-cardinality and never put
// secrets or personal data in them.
//
// Track never fails the caller. A name that is not snake_case is dropped with
// a warning, because the pipeline would drop it anyway.
//
//	observe.Track(ctx, "project_created", slog.String("template", "go"), slog.Int("repos", 2))
func Track(ctx context.Context, name string, attrs ...slog.Attr) {
	if ctx == nil {
		ctx = context.Background()
	}
	userID := ""
	if claims, ok := authn.ClaimsFromContext(ctx); ok && claims != nil {
		userID = claims.UserID
	}
	trackAs(ctx, name, userID, attrs)
}

// TrackSystem records a product event the system recorded about userID
// rather than one userID caused: it is Track with the user named by the
// caller (a worker has no signed-in caller; "" attributes the event to no
// one) and the event marked as not the user's own activity (PropActivity =
// ActivitySystem). The event is stored and counted as an event as usual, but
// it never makes userID an active, new or returning user.
//
//	observe.TrackSystem(ctx, org.OwnerID, "billing_subscription_state", slog.String("plan", plan))
//
// An emitter of its own (one that does not call Track) marks an event by
// adding SystemActivity() to its properties.
func TrackSystem(ctx context.Context, userID, name string, attrs ...slog.Attr) {
	if ctx == nil {
		ctx = context.Background()
	}
	trackAs(ctx, name, userID, append(attrs[:len(attrs):len(attrs)], SystemActivity()))
}

// SystemActivity is the event property that marks a product event as not the
// user's own activity (PropActivity = ActivitySystem). Pass it among an
// event's attributes, to Track or to any emitter that writes each attribute
// as a product.<key> property; TrackSystem adds it for you.
func SystemActivity() slog.Attr { return slog.String(PropActivity, ActivitySystem) }

type systemActivityKey struct{}

// WithSystemActivity marks every product event recorded under the returned
// context as not the user's own activity: Track, TrackAuth, and the events
// forge records on the app's behalf (the login broker's sign-ins and
// sign-ups in pkg/devidp, the generated CRUD events). It is how a caller
// marks events it does not emit itself, per call: a synthetic monitor's
// sign-in, a job that writes through the generated handlers.
//
//	ctx = observe.WithSystemActivity(ctx) // the identity canary's login
func WithSystemActivity(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemActivityKey{}, true)
}

// SystemActivityFrom reports whether ctx marks its product events as not the
// user's own activity (WithSystemActivity).
func SystemActivityFrom(ctx context.Context) bool {
	marked, _ := ctx.Value(systemActivityKey{}).(bool)
	return marked
}

// trackAs is Track with the user named by the caller rather than read from
// ctx: the one record shape every product event shares. TrackAuth uses it,
// because at sign-in the request is still anonymous.
func trackAs(ctx context.Context, name, userID string, attrs []slog.Attr) {
	logger := FromContext(ctx)
	if !ValidEventName(name) {
		logger.WarnContext(ctx, "product event dropped: name must be snake_case", slog.String("event", name))
		return
	}

	out := make([]slog.Attr, 0, 4+len(attrs))
	out = append(out, slog.String(AttrEventName, EventNamePrefix+name))
	if userID != "" {
		out = append(out, slog.String(AttrUserID, userID))
	}
	// The visitor's daily hash, when the trusted edge stamped this request
	// (edge_context.go): it links a server-side event to the anonymous visit.
	if vd := EdgeContextFrom(ctx).VisitorDay; vd != "" {
		out = append(out, slog.String(AttrVisitorDay, vd))
	}
	out = append(out, slog.String(AttrAppClient, AppClientBackend))
	if v := appVersion.Load(); v != nil && *v != "" {
		out = append(out, slog.String(AttrAppVersion, *v))
	}
	props, activity := 0, ""
	out = appendProps(out, "", attrs, &props, &activity)
	// The activity marker is the event's meaning, not one of its properties:
	// it is written once, after them, so the property cap never drops it.
	if SystemActivityFrom(ctx) {
		activity = ActivitySystem
	}
	if activity != "" {
		out = append(out, slog.String(AttrPropPrefix+PropActivity, truncateRunes(activity, MaxProductPropValueLen)))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, EventNamePrefix+name, out...)
}

// appendProps writes attrs as product.<key> properties, at most
// MaxProductProps of them. The activity marker (PropActivity) is not written
// here: its value is left in activity for the caller to write once.
func appendProps(out []slog.Attr, prefix string, attrs []slog.Attr, props *int, activity *string) []slog.Attr {
	for _, a := range attrs {
		v := a.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			p := prefix
			if a.Key != "" {
				p += a.Key + "."
			}
			out = appendProps(out, p, v.Group(), props, activity)
			continue
		}
		if a.Key == "" {
			continue
		}
		if prefix+a.Key == PropActivity {
			*activity = v.String()
			continue
		}
		if *props >= MaxProductProps {
			continue
		}
		*props++
		out = append(out, slog.String(AttrPropPrefix+prefix+a.Key, truncateRunes(v.String(), MaxProductPropValueLen)))
	}
	return out
}

func truncateRunes(s string, max int) string {
	if len(s) <= max || utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}
