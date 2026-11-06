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
// SignInRoutes.proveAddress).

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
	// MetaProofLock names the proof that holds an account locked across
	// its address proof: a random nonce, written before the lock. Only
	// the proof whose nonce is still current may lift a lock after a
	// failed proof; a lock with no marker (the issuer's, or one a failed
	// revocation left) is never lifted by a proof. See proveAddress.
	MetaProofLock = "forge.proof_lock"
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
	removed, err := drain(func() ([]IDPLink, error) { return c.ListIDPLinks(ctx, userID) },
		func(l IDPLink) string { return l.IdPID + "/" + l.UserID },
		func(l IDPLink) error {
			path := "/v2/users/" + url.PathEscape(userID) + "/links/" + url.PathEscape(l.IdPID) + "/" + url.PathEscape(l.UserID)
			if _, err := c.sendJSON(ctx, http.MethodDelete, path, map[string]any{}); err != nil && !isNotFound(err) {
				return fmt.Errorf("remove the %s link: %w", l.IdPID, err)
			}
			return nil
		})
	n += removed
	if err != nil {
		return n, err
	}
	// Second factors: a planted TOTP or SMS number is someone else's way
	// past a future password check. An absent one answers the issuer's
	// "not existing" error for that factor (isAbsent); any other refusal
	// fails the revocation.
	for _, factor := range []string{"totp", "otp_sms"} {
		path := "/v2/users/" + url.PathEscape(userID) + "/" + factor
		if _, err := c.sendJSON(ctx, http.MethodDelete, path, map[string]any{}); err == nil {
			n++
		} else if !isAbsent(err) {
			return n, fmt.Errorf("remove the %s factor: %w", factor, err)
		}
	}
	removed, err = drain(func() ([]Passkey, error) { return c.listPasskeys(ctx, userID, true) },
		func(k Passkey) string { return k.ID },
		func(k Passkey) error {
			path := "/v2/users/" + url.PathEscape(userID) + "/passkeys/" + url.PathEscape(k.ID)
			if _, err := c.sendJSON(ctx, http.MethodDelete, path, map[string]any{}); err != nil && !isNotFound(err) {
				return fmt.Errorf("remove passkey %s: %w", k.ID, err)
			}
			return nil
		})
	n += removed
	if err != nil {
		return n, err
	}
	ended, err := c.EndSessions(ctx, userID)
	return n + ended, err
}

// EndSessions deletes every issuer session of a user, so nothing created
// before a revocation can be finalized into tokens after it.
func (c *Client) EndSessions(ctx context.Context, userID string) (int, error) {
	return drain(func() ([]string, error) { return c.searchSessions(ctx, userID) },
		func(id string) string { return id },
		func(id string) error {
			if _, err := c.sendJSON(ctx, http.MethodDelete, "/v2/sessions/"+url.PathEscape(id), map[string]any{}); err != nil && !isNotFound(err) {
				return fmt.Errorf("end session %s: %w", id, err)
			}
			return nil
		})
}

// searchSessions returns the ids of one page of a user's issuer sessions.
func (c *Client) searchSessions(ctx context.Context, userID string) ([]string, error) {
	body, err := c.postJSON(ctx, "/v2/sessions/search", map[string]any{
		"queries": []any{map[string]any{"userIdQuery": map[string]any{"id": userID}}},
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var out struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	ids := make([]string, 0, len(out.Sessions))
	for _, s := range out.Sessions {
		ids = append(ids, s.ID)
	}
	return ids, nil
}

// maxDrainRounds bounds drain: far more pages than any account holds, so
// reaching it means the issuer keeps listing what was removed.
const maxDrainRounds = 1000

// drain removes everything a search finds, then searches again, until a
// search comes back empty. The issuer answers a search with ONE page (its
// default page size), so a single search-and-delete would leave whatever
// sits on a later page — a session or a planted credential that survives a
// revocation reported complete. Re-searching from the start after each
// round needs no paging contract: what was removed is gone from the next
// page. A page that lists only what this drain already removed means the
// removal did not take, and fails the drain rather than loop on it.
func drain[T any](list func() ([]T, error), key func(T) string, remove func(T) error) (int, error) {
	done := map[string]bool{}
	n := 0
	for range maxDrainRounds {
		page, err := list()
		if err != nil {
			return n, err
		}
		if len(page) == 0 {
			return n, nil
		}
		progressed := false
		for _, item := range page {
			k := key(item)
			if done[k] {
				continue
			}
			if err := remove(item); err != nil {
				return n, err
			}
			done[k], progressed = true, true
			n++
		}
		if !progressed {
			return n, fmt.Errorf("the issuer still lists %q after it was removed", key(page[0]))
		}
	}
	return n, fmt.Errorf("still finding more to remove after %d rounds", maxDrainRounds)
}

// UnlockUser lifts LockUser. An account that is not locked is left as it
// is: the issuer's "not locked" refusal is success here.
func (c *Client) UnlockUser(ctx context.Context, userID string) error {
	if _, err := c.postJSON(ctx, "/v2/users/"+url.PathEscape(userID)+"/unlock", map[string]any{}); err != nil && !hasErrorID(err, notLockedID) {
		return fmt.Errorf("unlock user: %w", err)
	}
	return nil
}

// Zitadel's stable error ids for the refusals this file treats as "already
// so" (v4.19.5, internal/command/user_v2.go and user_human_otp.go):
//
//   - COMMAND-lgws8wtsqf "Errors.User.ShouldBeActiveOrInitial": LockUser on
//     an account that is not active — already locked, for one.
//   - COMMAND-olb9vb0oca "Errors.User.NotLocked": UnlockUser on an account
//     that is not locked.
//   - COMMAND-Hd9sd / COMMAND-Sr3h3 "Errors.User.MFA.OTP.NotExisting":
//     removing a TOTP / OTP-SMS factor the account does not have.
const (
	notActiveID = "COMMAND-lgws8wtsqf"
	notLockedID = "COMMAND-olb9vb0oca"
	noTOTPID    = "COMMAND-Hd9sd"
	noOTPSMSID  = "COMMAND-Sr3h3"
)

// isAbsent reports the issuer's refusal to remove a second factor the
// account does not have — that factor's own "not existing" error and
// nothing broader: a 400 for any other reason fails the revocation.
func isAbsent(err error) bool {
	return hasErrorID(err, noTOTPID, noOTPSMSID)
}

// hasErrorID reports whether err is an issuer refusal carrying one of ids.
func hasErrorID(err error, ids ...string) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	id, _ := apiErr.errorID()
	for _, want := range ids {
		if id != "" && id == want {
			return true
		}
	}
	return false
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

// LockUser locks an account so nothing can sign in to it. An account that
// is locked already stays so: the issuer's "should be active" refusal is
// success here, which is what lets a proof re-lock unconditionally.
func (c *Client) LockUser(ctx context.Context, userID string) error {
	_, err := c.lockUser(ctx, userID)
	return err
}

// lockUser is LockUser that also reports whether this call locked the
// account (false: it was locked, or otherwise not active, already).
func (c *Client) lockUser(ctx context.Context, userID string) (locked bool, err error) {
	if _, err := c.postJSON(ctx, "/v2/users/"+url.PathEscape(userID)+"/lock", map[string]any{}); err != nil {
		if hasErrorID(err, notActiveID) {
			return false, nil
		}
		return false, fmt.Errorf("lock user: %w", err)
	}
	return true, nil
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
	_, ok, err := c.Metadata(ctx, userID, key)
	return ok, err
}

// Metadata reads one metadata key of an account: its value, and whether
// the account carries it.
func (c *Client) Metadata(ctx context.Context, userID, key string) (string, bool, error) {
	body, err := c.postJSON(ctx, "/v2/users/"+url.PathEscape(userID)+"/metadata/search", map[string]any{
		"filters": []any{map[string]any{"keyFilter": map[string]any{"key": key}}},
	})
	if err != nil {
		return "", false, fmt.Errorf("read account metadata: %w", err)
	}
	var out struct {
		Metadata []struct {
			Key   string `json:"key"`
			Value []byte `json:"value"` // the API's bytes: standard base64 in JSON
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", false, fmt.Errorf("decode account metadata: %w", err)
	}
	for _, m := range out.Metadata {
		if m.Key == key {
			return string(m.Value), true, nil
		}
	}
	return "", false, nil
}

// DeleteMetadata removes one metadata key from an account.
func (c *Client) DeleteMetadata(ctx context.Context, userID, key string) error {
	path := "/v2/users/" + url.PathEscape(userID) + "/metadata?keys=" + url.QueryEscape(key)
	if _, err := c.sendJSON(ctx, http.MethodDelete, path, map[string]any{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete account metadata: %w", err)
	}
	return nil
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
