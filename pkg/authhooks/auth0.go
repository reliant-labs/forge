package authhooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/observe"
)

// Auth0 is the hook endpoint for an Auth0 Log Stream of type Custom Webhook
// (Auth0 Dashboard > Monitoring > Streams), with the endpoint as its Payload
// URL and "Bearer <opts.Secret>" as its Authorization Token. Any content format (JSON
// Lines, JSON Array, JSON Object) is read. Of the tenant's log events it maps
//
//   - ss  (successful signup) → auth_signed_up;
//   - s   (successful login)  → auth_signed_in {kind: credential};
//   - slo (successful logout) → auth_signed_out,
//
// with the method read from the connection's strategy. Every other event type
// is acknowledged and recorded as nothing, so the stream may be left
// unfiltered; filtering it to the Signup, Login and Logout categories saves
// the traffic.
func Auth0(opts Options) http.Handler { return Handler(auth0Source{}, opts) }

type auth0Source struct{}

func (auth0Source) Name() string { return "auth0" }

func (auth0Source) Verify(r *http.Request, _ []byte, secret string) error {
	return verifyBearer(r, secret)
}

// auth0Log is one entry of a Log Stream delivery.
type auth0Log struct {
	LogID string `json:"log_id"`
	Data  struct {
		LogID        string    `json:"log_id"`
		Date         time.Time `json:"date"`
		Type         string    `json:"type"`
		UserID       string    `json:"user_id"`
		Strategy     string    `json:"strategy"`
		StrategyType string    `json:"strategy_type"`
	} `json:"data"`
}

func (auth0Source) Events(_ *http.Request, body []byte) ([]Event, error) {
	logs, err := auth0Logs(body)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, l := range logs {
		ev := Event{UserID: l.Data.UserID, At: l.Data.Date, DedupeKey: l.LogID}
		if ev.DedupeKey == "" {
			ev.DedupeKey = l.Data.LogID
		}
		switch l.Data.Type {
		case "ss":
			ev.Kind = observe.AuthSignedUp
		case "s":
			ev.Kind = observe.AuthSignedIn
		case "slo":
			ev.Kind = observe.AuthSignedOut
		default:
			continue
		}
		if ev.Kind != observe.AuthSignedOut {
			ev.Method, ev.Social = auth0Method(l.Data.StrategyType, l.Data.Strategy)
		}
		out = append(out, ev)
	}
	return out, nil
}

// auth0Logs reads a delivery in any of the stream's content formats: a JSON
// array, one JSON object, or JSON Lines.
func auth0Logs(body []byte) ([]auth0Log, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var logs []auth0Log
		if err := json.Unmarshal(trimmed, &logs); err != nil {
			return nil, fmt.Errorf("auth0 log stream (JSON array): %w", err)
		}
		return logs, nil
	}
	// One JSON object, or JSON Lines: a stream of objects either way.
	var logs []auth0Log
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	for dec.More() {
		var l auth0Log
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("auth0 log stream (JSON lines): %w", err)
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// auth0Method maps a connection onto forge's method vocabulary: by its
// strategy type when the log names one, else by the strategy itself.
func auth0Method(strategyType, strategy string) (observe.AuthMethod, string) {
	strategy = strings.ToLower(strategy)
	switch strings.ToLower(strategyType) {
	case "database":
		return observe.AuthMethodPassword, ""
	case "passwordless":
		return observe.AuthMethodMagicLink, ""
	case "enterprise":
		return observe.AuthMethodSSO, ""
	case "social":
		return observe.AuthMethodSocial, auth0Social(strategy)
	}
	switch strategy {
	case "":
		return "", ""
	case "auth0":
		return observe.AuthMethodPassword, ""
	case "email", "sms":
		return observe.AuthMethodMagicLink, ""
	case "samlp", "oidc", "waad", "adfs", "ad", "okta", "google-apps", "pingfederate":
		return observe.AuthMethodSSO, ""
	}
	return observe.AuthMethodSocial, auth0Social(strategy)
}

func auth0Social(strategy string) string {
	switch strategy {
	case "google-oauth2":
		return "google"
	case "github":
		return "github"
	case "windowslive":
		return "microsoft"
	case "apple":
		return "apple"
	}
	return "other"
}
