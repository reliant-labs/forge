package devidp

import (
	"context"
	"errors"
	"fmt"
)

// ── Pre-registration takeover, closed ────────────────────────────────────
//
// THE ATTACK: someone registers victim@example.com with a password of their
// own, or links their own social account to it. The account is unverified,
// so it cannot sign in — until the owner of the inbox proves it, by a magic
// link, a verification link or an invitation. That proof verifies the
// ADDRESS; it must not also legitimise what the attacker set before it.
//
// TWO HALVES:
//
//   - Nothing the sign-up routes create carries a credential before its
//     proof. A sign-up's password and an unverified provider's link wait,
//     sealed in the browser that chose them (passwordCookie, linkCookie),
//     and are applied only when that browser proves the address.
//   - Whatever an unverified account DOES hold when a proof arrives — an
//     account created before this rule, by the legacy broker's sign-up, or
//     by anything else — is revoked by the proof (afterProof): a random
//     password, no links, no passkeys. Nothing proves who set it.
//
// A revocation that fails locks the account (fail closed) and the proof's
// caller answers "unavailable": a verified account must never be left with
// credentials nobody proved.

// maxPasswordBytes bounds a password the sign-up routes accept, so a sealed
// binding that carries one keeps its fixed size.
const maxPasswordBytes = 256

// errRevokeFailed marks a proof that succeeded but whose unproven
// credentials could not be taken back: the account is left locked.
var errRevokeFailed = errors.New("the proof succeeded but credentials set before it could not be revoked; the account is locked")

// proveAddress runs proof — the issuer's check of an emailed code that also
// verifies the address — and applies the rule above. before is the account
// as read before the proof.
//
// An address already verified decides nothing: proof runs alone. Otherwise
// the account is LOCKED FIRST, so nothing can sign in to it in the window
// between "verified" and "revoked" (once VerifyEmail succeeds, a password
// planted before it would otherwise work until the revocation lands):
//
//	lock → end every session → proof → revoke credentials → end every session → unlock
//
// A proof that fails (a wrong code) unlocks again: an unverified account
// cannot sign in anyway. A revocation that fails leaves the account locked
// and returns errRevokeFailed. An account the issuer had locked already is
// never unlocked here. revoked reports that credentials were taken back.
func (r *SignInRoutes) proveAddress(ctx context.Context, before User, proof func() error) (revoked bool, err error) {
	if before.EmailVerified {
		return false, proof()
	}
	c := r.cfg.Client
	userID := before.ID
	if !before.Locked {
		if err := c.LockUser(ctx, userID); err != nil {
			// Not proven yet: refuse rather than verify an account that
			// cannot be held still.
			return false, fmt.Errorf("lock the account for its proof: %w", err)
		}
	}
	unlock := func() error {
		if before.Locked {
			return nil
		}
		return c.UnlockUser(ctx, userID)
	}
	// End every session first. One created a moment before the lock
	// already carries a passed password check; the verified-email gate
	// would let it through the moment proof() marks the address verified,
	// before the revocation below could end it.
	if _, err := c.EndSessions(ctx, userID); err != nil {
		if uerr := unlock(); uerr != nil {
			r.cfg.Logger.ErrorContext(ctx, "could not unlock an account after a failed session cleanup", "user_id", userID, "error", uerr)
		}
		return false, fmt.Errorf("end the account's sessions before its proof: %w", err)
	}
	if err := proof(); err != nil {
		if uerr := unlock(); uerr != nil {
			r.cfg.Logger.ErrorContext(ctx, "could not unlock an account after a refused proof", "user_id", userID, "error", uerr)
		}
		return false, err
	}
	n, err := r.revoke(ctx, userID)
	if err != nil {
		return false, err
	}
	if err := unlock(); err != nil {
		r.cfg.Logger.ErrorContext(ctx, "address proven and credentials revoked, but the account could not be unlocked", "user_id", userID, "error", err)
		return n > 0, fmt.Errorf("unlock the account after its proof: %w", err)
	}
	return n > 0, nil
}

// revoke takes back an account's unproven credentials and ends its
// sessions, retrying once. The caller holds the account locked. If both
// tries fail it makes one last attempt at the password alone (the credential
// a planted sign-up most likely set) and raises an alert: the account stays
// locked until someone resets it by hand.
func (r *SignInRoutes) revoke(ctx context.Context, userID string) (int, error) {
	c := r.cfg.Client
	n, err := c.RevokeCredentials(ctx, userID)
	if err != nil {
		n, err = c.RevokeCredentials(ctx, userID)
	}
	if err == nil {
		if n > 0 {
			r.cfg.Logger.InfoContext(ctx, "credentials set before the address was proven were revoked", "user_id", userID, "count", n)
		}
		return n, nil
	}
	pwErr := c.SetPassword(ctx, userID, unguessablePassword())
	r.cfg.Logger.ErrorContext(ctx, "ALERT: unproven credentials could not be revoked; the account stays LOCKED until its credentials are reset by hand",
		"user_id", userID, "error", err, "password_replaced", pwErr == nil, "alert", "identity.revoke_failed")
	return 0, fmt.Errorf("%w: %v", errRevokeFailed, err)
}

// grantSignupRoleIfSelfSignup grants signup_role to an account public
// sign-up created (MetaSelfSignup), once (MetaSignupRoleGranted), and only
// when the policy still lets its now-verified address sign up. An invited or
// declared account, or a re-verification, gets nothing.
func (r *SignInRoutes) grantSignupRoleIfSelfSignup(ctx context.Context, userID, email string) {
	if r.cfg.Policy.SignupRole == "" || r.cfg.ProjectID == "" || !r.cfg.Policy.SignupAllows(email) {
		return
	}
	c := r.cfg.Client
	self, err := c.HasMetadata(ctx, userID, MetaSelfSignup)
	if err != nil || !self {
		if err != nil {
			r.cfg.Logger.ErrorContext(ctx, "could not read how the account was created; no sign-up role", "user_id", userID, "error", err)
		}
		return
	}
	if done, err := c.HasMetadata(ctx, userID, MetaSignupRoleGranted); err != nil || done {
		return
	}
	if !r.grantSignupRole(ctx, userID) {
		return
	}
	if err := c.SetMetadata(ctx, userID, MetaSignupRoleGranted, "1"); err != nil {
		r.cfg.Logger.WarnContext(ctx, "sign-up role granted but not recorded", "user_id", userID, "error", err)
	}
}
