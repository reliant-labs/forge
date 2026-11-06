package devidp

import (
	"context"
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

// afterProof applies the rule once a proof verified userID's address.
// wasVerified is the address state BEFORE the proof: an address already
// verified decides nothing. revoked reports that credentials were taken back
// (the UI then offers to set a password or a passkey).
func (r *SignInRoutes) afterProof(ctx context.Context, userID string, wasVerified bool) (revoked bool, err error) {
	if wasVerified {
		return false, nil
	}
	n, err := r.revoke(ctx, userID)
	return n > 0, err
}

// revoke takes back an account's unproven credentials, retrying once, and
// locks the account if it cannot. It returns how many were taken back.
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
	r.cfg.Logger.ErrorContext(ctx, "could not revoke unproven credentials; locking the account", "user_id", userID, "error", err)
	if lerr := c.LockUser(ctx, userID); lerr != nil {
		r.cfg.Logger.ErrorContext(ctx, "could not lock the account either: unlock nothing until its credentials are reset by hand",
			"user_id", userID, "error", lerr)
	}
	return 0, err
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
