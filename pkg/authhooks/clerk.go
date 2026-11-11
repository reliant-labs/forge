package authhooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/observe"
)

// Clerk is the hook endpoint for a Clerk webhook (Clerk Dashboard >
// Webhooks), which Clerk delivers through Svix, signed per Standard Webhooks.
// opts.Secret is the endpoint's signing secret, "whsec_…". Subscribe it to
// user.created, session.created, session.ended and session.removed:
//
//   - user.created     → auth_signed_up, verified from the primary email;
//   - session.created  → auth_signed_in {kind: credential};
//   - session.ended,
//     session.removed  → auth_signed_out.
//
// Every other event type is acknowledged and recorded as nothing.
func Clerk(opts Options) http.Handler { return Handler(clerkSource{now: time.Now}, opts) }

type clerkSource struct{ now func() time.Time }

func (clerkSource) Name() string { return "clerk" }

func (c clerkSource) Verify(r *http.Request, body []byte, secret string) error {
	return verifyStandardWebhooks(r, body, secret, c.now())
}

type clerkEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type clerkUser struct {
	ID                    string `json:"id"`
	CreatedAt             int64  `json:"created_at"` // unix milliseconds
	PasswordEnabled       bool   `json:"password_enabled"`
	PrimaryEmailAddressID string `json:"primary_email_address_id"`
	EmailAddresses        []struct {
		ID           string `json:"id"`
		Verification *struct {
			Status   string `json:"status"`
			Strategy string `json:"strategy"`
		} `json:"verification"`
	} `json:"email_addresses"`
	ExternalAccounts []struct {
		Provider string `json:"provider"` // "oauth_google", …
	} `json:"external_accounts"`
	SAMLAccounts       []json.RawMessage `json:"saml_accounts"`
	EnterpriseAccounts []json.RawMessage `json:"enterprise_accounts"`
}

type clerkSession struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func (clerkSource) Events(_ *http.Request, body []byte) ([]Event, error) {
	var e clerkEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("clerk webhook payload: %w", err)
	}
	switch e.Type {
	case "user.created":
		var u clerkUser
		if err := json.Unmarshal(e.Data, &u); err != nil {
			return nil, fmt.Errorf("clerk user.created: %w", err)
		}
		ev := Event{Kind: observe.AuthSignedUp, UserID: u.ID, At: unixMilli(u.CreatedAt), DedupeKey: u.ID}
		ev.Method, ev.Social = clerkMethod(u)
		for _, a := range u.EmailAddresses {
			if a.ID == u.PrimaryEmailAddressID && a.Verification != nil {
				v := a.Verification.Status == "verified"
				ev.Verified = &v
			}
		}
		return []Event{ev}, nil
	case "session.created", "session.ended", "session.removed":
		var s clerkSession
		if err := json.Unmarshal(e.Data, &s); err != nil {
			return nil, fmt.Errorf("clerk %s: %w", e.Type, err)
		}
		ev := Event{Kind: observe.AuthSignedIn, UserID: s.UserID, At: unixMilli(s.CreatedAt), DedupeKey: s.ID}
		if e.Type != "session.created" {
			ev.Kind, ev.At = observe.AuthSignedOut, unixMilli(s.UpdatedAt)
		}
		return []Event{ev}, nil
	}
	return nil, nil
}

// clerkMethod reads how a new Clerk user signed up: a social account, an
// enterprise (SAML/OIDC) account, a password, or an emailed code or link.
func clerkMethod(u clerkUser) (observe.AuthMethod, string) {
	switch {
	case len(u.SAMLAccounts) > 0 || len(u.EnterpriseAccounts) > 0:
		return observe.AuthMethodSSO, ""
	case len(u.ExternalAccounts) > 0:
		name := strings.TrimPrefix(strings.ToLower(u.ExternalAccounts[0].Provider), "oauth_")
		switch name {
		case "google", "github", "apple", "microsoft":
			return observe.AuthMethodSocial, name
		}
		return observe.AuthMethodSocial, "other"
	case u.PasswordEnabled:
		return observe.AuthMethodPassword, ""
	}
	for _, a := range u.EmailAddresses {
		if a.ID == u.PrimaryEmailAddressID && a.Verification != nil {
			switch a.Verification.Strategy {
			case "email_link", "email_code":
				return observe.AuthMethodMagicLink, ""
			}
		}
	}
	return "", ""
}

func unixMilli(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
