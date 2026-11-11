package authhooks

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/observe"
)

//go:embed sql/supabase_forge_auth_events.sql
var supabaseSQL string

// SupabaseSQL is the SQL that turns a Supabase project's auth database into
// this endpoint's sender: triggers on auth.users and auth.sessions posting
// through pg_net, with the endpoint URL and the hook secret read from Vault.
// It is idempotent and never blocks or fails auth; its header says how to
// install, verify and remove it.
func SupabaseSQL() string { return supabaseSQL }

// Supabase is the hook endpoint for the triggers SupabaseSQL installs:
//
//	mux.Handle(authhooks.Path("supabase"), authhooks.Supabase(authhooks.Options{Secret: secret}))
//
// The triggers authenticate with "Authorization: Bearer <secret>", where
// secret is the value stored in the project's Vault as
// forge_auth_hook_secret; opts.Secret is the same value, read by the app.
func Supabase(opts Options) http.Handler { return Handler(supabaseSource{}, opts) }

type supabaseSource struct{}

func (supabaseSource) Name() string { return "supabase" }

func (supabaseSource) Verify(r *http.Request, _ []byte, secret string) error {
	return verifyBearer(r, secret)
}

// supabasePayload is what forge_auth.emit posts: version 1.
type supabasePayload struct {
	V        int       `json:"v"`
	Event    string    `json:"event"`
	UserID   string    `json:"user_id"`
	At       time.Time `json:"at"`
	Provider string    `json:"provider"`
	Password *bool     `json:"password"`
	Verified *bool     `json:"verified"`
	Dedupe   string    `json:"dedupe"`
}

func (supabaseSource) Events(_ *http.Request, body []byte) ([]Event, error) {
	var p supabasePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("supabase hook payload: %w", err)
	}
	if p.V != 1 {
		return nil, nil // a newer SQL than this forge: nothing it can map
	}
	ev := Event{UserID: p.UserID, At: p.At, DedupeKey: p.Dedupe}
	switch p.Event {
	case "signed_up":
		ev.Kind = observe.AuthSignedUp
		ev.Verified = p.Verified
	case "signed_in":
		ev.Kind = observe.AuthSignedIn
	default:
		return nil, nil
	}
	ev.Method, ev.Social = supabaseMethod(p.Provider, p.Password)
	return []Event{ev}, nil
}

// supabaseMethod maps the account's Supabase sign-in provider onto forge's
// method vocabulary. "email" is a password when the account has one, else a
// magic link or one-time code; an OAuth provider is a social login. A sign-in
// says nothing about the password, so its "email" method stays unknown.
func supabaseMethod(provider string, password *bool) (observe.AuthMethod, string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch {
	case provider == "":
		return "", ""
	case provider == "email":
		if password == nil {
			return "", ""
		}
		if *password {
			return observe.AuthMethodPassword, ""
		}
		return observe.AuthMethodMagicLink, ""
	case provider == "phone":
		return observe.AuthMethodOther, ""
	case provider == "sso" || strings.HasPrefix(provider, "sso:"):
		return observe.AuthMethodSSO, ""
	case provider == "azure":
		return observe.AuthMethodSocial, "microsoft"
	case provider == "google", provider == "github", provider == "apple":
		return observe.AuthMethodSocial, provider
	}
	// Supabase's other OAuth providers: bitbucket, discord, facebook,
	// gitlab, keycloak, linkedin_oidc, notion, slack_oidc, spotify, twitch,
	// twitter, workos, zoom, ….
	return observe.AuthMethodSocial, "other"
}
