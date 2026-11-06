package devidp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Taking back credentials nobody proved, and the account markers that say
// how an account came to be.
//
// An unverified account's credentials were set by whoever TYPED the address
// — which need not be its owner. Someone can register victim@example.com
// with a password of their own; when the owner later proves the inbox (a
// magic link, a verification link, an invitation), that password must not
// become valid with it. RevokeCredentials is the undo: it is called after the
// proof, for credentials the proving browser cannot vouch for (see
// SignInRoutes.afterProof).

// Account metadata keys this package writes.
const (
	// MetaSelfSignup marks an account created by public sign-up (register,
	// a magic link, a social sign-in), as opposed to one an owner invited
	// or the identity declares. Only such an account gets signup_role.
	MetaSelfSignup = "forge.self_signup"
	// MetaSignupRoleGranted records that signup_role was granted, so a
	// re-verification (an address change) does not grant it again after an
	// owner revoked it.
	MetaSignupRoleGranted = "forge.signup_role_granted"
)

// RevokeCredentials removes every credential on an account: the password is
// replaced by a random one nobody holds (when it has one), and every identity
// provider link and passkey is removed. The account itself, its address and
// its grants stay. It returns how many credentials it took back.
func (c *Client) RevokeCredentials(ctx context.Context, userID string) (int, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0, fmt.Errorf("a user id is required")
	}
	n := 0
	if has, err := c.HasPassword(ctx, userID); err != nil {
		return 0, err
	} else if has {
		if err := c.SetPassword(ctx, userID, unguessablePassword()); err != nil {
			return n, fmt.Errorf("replace the password: %w", err)
		}
		n++
	}
	links, err := c.ListIDPLinks(ctx, userID)
	if err != nil {
		return n, err
	}
	for _, l := range links {
		path := "/v2/users/" + url.PathEscape(userID) + "/links/" + url.PathEscape(l.IdPID) + "/" + url.PathEscape(l.UserID)
		if _, err := c.sendJSON(ctx, http.MethodDelete, path, map[string]any{}); err != nil && !isNotFound(err) {
			return n, fmt.Errorf("remove the %s link: %w", l.IdPID, err)
		}
		n++
	}
	// Second factors: a planted TOTP or SMS number is someone else's way
	// past a future password check. Absent ones answer an error the issuer
	// spells as not-found or a failed precondition; both mean "none".
	for _, factor := range []string{"totp", "otp_sms"} {
		path := "/v2/users/" + url.PathEscape(userID) + "/" + factor
		if _, err := c.sendJSON(ctx, http.MethodDelete, path, map[string]any{}); err == nil {
			n++
		} else if !isAbsent(err) {
			return n, fmt.Errorf("remove the %s factor: %w", factor, err)
		}
	}
	keys, err := c.listPasskeys(ctx, userID, true)
	if err != nil {
		return n, err
	}
	for _, k := range keys {
		path := "/v2/users/" + url.PathEscape(userID) + "/passkeys/" + url.PathEscape(k.ID)
		if _, err := c.sendJSON(ctx, http.MethodDelete, path, map[string]any{}); err != nil && !isNotFound(err) {
			return n, fmt.Errorf("remove passkey %s: %w", k.ID, err)
		}
		n++
	}
	ended, err := c.EndSessions(ctx, userID)
	return n + ended, err
}

// EndSessions deletes every issuer session of a user, so nothing created
// before a revocation can be finalized into tokens after it.
func (c *Client) EndSessions(ctx context.Context, userID string) (int, error) {
	body, err := c.postJSON(ctx, "/v2/sessions/search", map[string]any{
		"queries": []any{map[string]any{"userIdQuery": map[string]any{"id": userID}}},
	})
	if err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}
	var out struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("decode sessions: %w", err)
	}
	n := 0
	for _, s := range out.Sessions {
		if _, err := c.sendJSON(ctx, http.MethodDelete, "/v2/sessions/"+url.PathEscape(s.ID), map[string]any{}); err != nil && !isNotFound(err) {
			return n, fmt.Errorf("end session %s: %w", s.ID, err)
		}
		n++
	}
	return n, nil
}

// UnlockUser lifts LockUser.
func (c *Client) UnlockUser(ctx context.Context, userID string) error {
	if _, err := c.postJSON(ctx, "/v2/users/"+url.PathEscape(userID)+"/unlock", map[string]any{}); err != nil {
		return fmt.Errorf("unlock user: %w", err)
	}
	return nil
}

// isAbsent reports a refusal that means "there is none to remove".
func isAbsent(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && (apiErr.status == http.StatusNotFound || apiErr.status == http.StatusBadRequest ||
		apiErr.status == http.StatusPreconditionFailed)
}

// unguessablePassword satisfies any complexity policy (upper, lower, digit,
// symbol, 48 characters) and is known to nobody.
func unguessablePassword() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "Rv9!" + base64.RawURLEncoding.EncodeToString(b) + "aA1#"
}

// SetPassword sets a user's password with a reset code the broker mints
// itself. CALL ONLY AFTER A PROOF for this user in the same request: the
// broker's credential needs no code from the person to do this.
func (c *Client) SetPassword(ctx context.Context, userID, password string) error {
	code, err := c.MintPasswordResetCode(ctx, userID)
	if err != nil {
		return err
	}
	return c.ResetPassword(ctx, PasswordReset{UserID: userID, Code: code, NewPassword: password})
}

// LockUser locks an account so nothing can sign in to it: the fail-closed
// answer when its unproven credentials could not be revoked.
func (c *Client) LockUser(ctx context.Context, userID string) error {
	if _, err := c.postJSON(ctx, "/v2/users/"+url.PathEscape(userID)+"/lock", map[string]any{}); err != nil {
		return fmt.Errorf("lock user: %w", err)
	}
	return nil
}

// ListIDPLinks returns the identity-provider links of an account.
func (c *Client) ListIDPLinks(ctx context.Context, userID string) ([]IDPLink, error) {
	body, err := c.postJSON(ctx, "/v2/users/"+url.PathEscape(userID)+"/links/_search", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("list identity provider links: %w", err)
	}
	var out struct {
		Result []struct {
			IdPID    string `json:"idpId"`
			UserID   string `json:"userId"`
			UserName string `json:"userName"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode identity provider links: %w", err)
	}
	links := make([]IDPLink, 0, len(out.Result))
	for _, l := range out.Result {
		links = append(links, IDPLink{IdPID: l.IdPID, UserID: l.UserID, UserName: l.UserName})
	}
	return links, nil
}

// HasMetadata reports whether an account carries a metadata key.
func (c *Client) HasMetadata(ctx context.Context, userID, key string) (bool, error) {
	body, err := c.postJSON(ctx, "/v2/users/"+url.PathEscape(userID)+"/metadata/search", map[string]any{
		"filters": []any{map[string]any{"keyFilter": map[string]any{"key": key}}},
	})
	if err != nil {
		return false, fmt.Errorf("read account metadata: %w", err)
	}
	var out struct {
		Metadata []struct {
			Key string `json:"key"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, fmt.Errorf("decode account metadata: %w", err)
	}
	for _, m := range out.Metadata {
		if m.Key == key {
			return true, nil
		}
	}
	return false, nil
}

// SetMetadata writes one metadata key on an account.
func (c *Client) SetMetadata(ctx context.Context, userID, key, value string) error {
	_, err := c.postJSON(ctx, "/v2/users/"+url.PathEscape(userID)+"/metadata", map[string]any{
		"metadata": []any{metadataEntry(key, value)},
	})
	if err != nil {
		return fmt.Errorf("write account metadata: %w", err)
	}
	return nil
}

// metadataEntry is one {key, value} with the value as the API's bytes
// (standard base64 in JSON).
func metadataEntry(key, value string) map[string]any {
	return map[string]any{"key": key, "value": base64.StdEncoding.EncodeToString([]byte(value))}
}

func isNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound
}
