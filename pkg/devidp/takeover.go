package devidp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
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
//     by anything else — is revoked by the proof (proveAddress): a random
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
// as read before the proof; it decides only whether the address still needs
// proving, and is read again once this proof has the account to itself.
//
// An address already verified decides nothing: proof runs alone. Otherwise
// the account is LOCKED FIRST, so nothing can sign in to it in the window
// between "verified" and "revoked" (once VerifyEmail succeeds, a password
// planted before it would otherwise work until the revocation lands):
//
//	mark → lock → end every session → proof → re-lock → revoke credentials
//	→ end every session → unlock → clear the mark
//
// WHO MAY UNLOCK. Two proofs of one account can run at once — an attacker
// who knows the user id keeps a wrong-code proof in flight while the owner
// proves the address — so "was it locked when I looked" cannot say whose
// lock it is: the wrong-code proof would lift the lock under the owner's
// succeeding one. Ownership is explicit instead:
//
//   - In this process, proofs of one account run one at a time (proofs).
//   - Across replicas, the proof writes its own nonce to MetaProofLock
//     before it locks. A FAILED proof clears the mark and unlocks only
//     while its nonce is still the current one; a later proof that took
//     the lock over owns it.
//   - A SUCCESSFUL proof always re-locks (LockUser treats "already locked"
//     as done), whatever a concurrent failed proof did, then revokes,
//     unlocks and clears the mark.
//   - A lock with no mark is not a proof's: the issuer's own lockout, or
//     one a failed revocation left. No proof lifts it.
//
// A revocation that fails leaves the account locked, clears the mark so the
// lock reads as one nothing lifts, and returns errRevokeFailed. revoked
// reports that credentials were taken back.
//
// What remains across replicas, without a compare-and-swap at the issuer: a
// failed proof that reads its own nonce and then, two round-trips later,
// unlocks, can land that unlock after another replica's proof has marked,
// locked and verified — until that proof's re-lock. Within one process the
// proofs lock closes it entirely.
func (r *SignInRoutes) proveAddress(ctx context.Context, before User, proof func() error) (revoked bool, err error) {
	if before.EmailVerified {
		return false, proof()
	}
	userID := before.ID
	release, err := r.proofs.lock(ctx, userID)
	if err != nil {
		return false, err
	}
	defer release()

	c := r.cfg.Client
	// Read again: a proof that held the account a moment ago may have
	// verified it, or left it locked.
	now, err := c.FindUserByID(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("read the account for its proof: %w", err)
	}
	if now.EmailVerified {
		return false, proof()
	}
	held := false // locked by something other than a proof: never lifted here
	if now.Locked {
		mark, _, err := c.Metadata(ctx, userID, MetaProofLock)
		if err != nil {
			return false, fmt.Errorf("read the account's proof lock: %w", err)
		}
		held = mark == ""
	}
	nonce := ""
	if !held {
		nonce = proofNonce()
		if err := c.SetMetadata(ctx, userID, MetaProofLock, nonce); err != nil {
			return false, fmt.Errorf("mark the account's proof lock: %w", err)
		}
		if err := c.LockUser(ctx, userID); err != nil {
			// Not proven yet: refuse rather than verify an account that
			// cannot be held still, and take the mark back.
			if derr := c.DeleteMetadata(ctx, userID, MetaProofLock); derr != nil {
				r.cfg.Logger.WarnContext(ctx, "could not clear the proof lock of an account that would not lock", "user_id", userID, "error", derr)
			}
			return false, fmt.Errorf("lock the account for its proof: %w", err)
		}
	}
	// releaseFailed lifts this proof's lock after it failed — only while
	// the mark is still this proof's, and clearing the mark first: a mark
	// left on an unlocked account would make a later issuer lockout look
	// like a proof's. Unsure is locked: fail closed (a later proof takes a
	// marked lock over).
	releaseFailed := func(why string) {
		if held {
			return
		}
		mark, _, err := c.Metadata(ctx, userID, MetaProofLock)
		if err != nil {
			r.cfg.Logger.ErrorContext(ctx, "could not read the proof lock after "+why+"; the account stays locked", "user_id", userID, "error", err)
			return
		}
		if mark != nonce {
			return // a later proof owns the lock now
		}
		if err := c.DeleteMetadata(ctx, userID, MetaProofLock); err != nil {
			r.cfg.Logger.ErrorContext(ctx, "could not clear the proof lock after "+why+"; the account stays locked", "user_id", userID, "error", err)
			return
		}
		if err := c.UnlockUser(ctx, userID); err != nil {
			r.cfg.Logger.ErrorContext(ctx, "could not unlock an account after "+why, "user_id", userID, "error", err)
		}
	}
	// End every session first. One created a moment before the lock
	// already carries a passed password check; the verified-email gate
	// would let it through the moment proof() marks the address verified,
	// before the revocation below could end it.
	if _, err := c.EndSessions(ctx, userID); err != nil {
		releaseFailed("a failed session cleanup")
		return false, fmt.Errorf("end the account's sessions before its proof: %w", err)
	}
	if err := proof(); err != nil {
		releaseFailed("a refused proof")
		return false, err
	}
	// Proven. Re-lock whatever a concurrent failed proof on another
	// replica did to the lock; a failure here must not stop the revocation.
	if err := c.LockUser(ctx, userID); err != nil {
		r.cfg.Logger.ErrorContext(ctx, "could not re-lock a proven account before revoking its credentials", "user_id", userID, "error", err)
	}
	n, err := r.revoke(ctx, userID)
	if err != nil {
		if !held {
			if derr := c.DeleteMetadata(ctx, userID, MetaProofLock); derr != nil {
				r.cfg.Logger.ErrorContext(ctx, "could not clear the proof lock of an account left locked", "user_id", userID, "error", derr)
			}
		}
		return false, err
	}
	if held {
		return n > 0, nil
	}
	if err := c.UnlockUser(ctx, userID); err != nil {
		r.cfg.Logger.ErrorContext(ctx, "address proven and credentials revoked, but the account could not be unlocked", "user_id", userID, "error", err)
		return n > 0, fmt.Errorf("unlock the account after its proof: %w", err)
	}
	if err := c.DeleteMetadata(ctx, userID, MetaProofLock); err != nil {
		// Inert on an unlocked, verified account.
		r.cfg.Logger.WarnContext(ctx, "could not clear the proof lock of a proven account", "user_id", userID, "error", err)
	}
	return n > 0, nil
}

// proofNonce names one proof in MetaProofLock.
func proofNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// keyedMutex serialises work per key — here, address proofs per user id —
// and forgets a key once nobody holds or waits for it. The zero value is
// ready to use.
type keyedMutex struct {
	mu   sync.Mutex
	keys map[string]*keyedSlot
}

type keyedSlot struct {
	ch   chan struct{} // holds one token while the key is locked
	refs int           // holders and waiters
}

// lock waits for key, or for ctx to end, and returns the release.
func (k *keyedMutex) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.keys == nil {
		k.keys = map[string]*keyedSlot{}
	}
	s := k.keys[key]
	if s == nil {
		s = &keyedSlot{ch: make(chan struct{}, 1)}
		k.keys[key] = s
	}
	s.refs++
	k.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
		return func() { <-s.ch; k.drop(key, s) }, nil
	case <-ctx.Done():
		k.drop(key, s)
		return nil, ctx.Err()
	}
}

func (k *keyedMutex) drop(key string, s *keyedSlot) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s.refs--; s.refs == 0 {
		delete(k.keys, key)
	}
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
