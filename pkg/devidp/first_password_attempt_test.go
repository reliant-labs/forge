package devidp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/testkit"
)

// forge#2179 review (password.go:385): first-password install attempts are
// records safe to leave. An ambiguous failure keeps them, a wrong code removes
// only its own, and only a password set at or after a fresh attempt — never
// a squatter's earlier one — lets a later reset verify the address.

func provenBroker(z *fakeZitadel) *Client {
	c := z.client()
	c.ProvenCredentialsOnly = true
	return c
}

func resetCode(t *testing.T, z *fakeZitadel, id string) string {
	t.Helper()
	code, err := z.client().MintPasswordResetCode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// recoverWith redeems a fresh code and reports whether the address ended
// verified with that password.
func recoverWith(t *testing.T, z *fakeZitadel, id, password string) bool {
	t.Helper()
	if err := provenBroker(z).ResetPassword(context.Background(), PasswordReset{UserID: id, Code: resetCode(t, z, id), NewPassword: password}); err != nil {
		t.Fatalf("the recovery reset: %v", err)
	}
	verified, pw := z.passwordState(id)
	return verified && pw == password
}

func TestFirstPasswordAttempt_ALostAnswerAfterTheInstallStaysRecoverable(t *testing.T) {
	z := newFakeZitadel(t)
	u := z.addUser("invitee@acme.com", false, "")
	z.mu.Lock()
	z.passwordSetAnswerLost = true
	z.mu.Unlock()
	err := provenBroker(z).ResetPassword(context.Background(), PasswordReset{UserID: u.id, Code: resetCode(t, z, u.id), NewPassword: "first-password-1"})
	if err == nil || errors.Is(err, ErrInvalidResetCode) {
		t.Fatalf("a lost answer: %v, want an ambiguous failure", err)
	}
	if verified, pw := z.passwordState(u.id); verified || pw != "first-password-1" {
		t.Fatalf("precondition: want the password installed and the address unverified (verified=%v)", verified)
	}
	if !recoverWith(t, z, u.id, "first-password-2") {
		t.Fatal("after a lost answer, a new reset code did not verify the account it installed a first password on")
	}
}

// A valid first password whose verification fails, and a wrong code, on the
// same account at once — in both orders, held deterministically at the
// issuer's password write.
func TestFirstPasswordAttempt_AWrongCodeBesideAValidOneNeverBreaksRecovery(t *testing.T) {
	for _, validFirst := range []bool{true, false} {
		name := "the wrong code lands while the valid write is held"
		if !validFirst {
			name = "the valid write lands while the wrong code is held"
		}
		t.Run(name, func(t *testing.T) {
			z := newFakeZitadel(t)
			u := z.addUser("invitee@acme.com", false, "")
			code := resetCode(t, z, u.id)
			held, release := make(chan struct{}), make(chan struct{})
			var first atomic.Bool
			z.mu.Lock()
			z.beforeRequest = func(r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/users/"+u.id+"/password") && first.CompareAndSwap(false, true) {
					close(held)
					<-release
				}
			}
			z.failEmailResends = 1 // the valid one's verification fails
			z.mu.Unlock()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			valid := func() error {
				return provenBroker(z).ResetPassword(context.Background(), PasswordReset{UserID: u.id, Code: code, NewPassword: "first-password-1"})
			}
			wrong := func() error {
				return provenBroker(z).ResetPassword(context.Background(), PasswordReset{UserID: u.id, Code: "a-wrong-code", NewPassword: "attacker-password-9"})
			}
			heldOne, other := valid, wrong
			if !validFirst {
				heldOne, other = wrong, valid
			}
			done := make(chan error, 1)
			go func() { done <- heldOne() }()
			testkit.Receive(t, "the first password write at the issuer", held, testkit.Within(10*time.Second))
			otherErr := other()
			close(release)
			heldErr := testkit.Receive(t, "the held request", done, testkit.Within(10*time.Second))
			validErr, wrongErr := heldErr, otherErr
			if !validFirst {
				validErr, wrongErr = otherErr, heldErr
			}
			if !errors.Is(wrongErr, ErrInvalidResetCode) {
				t.Fatalf("the wrong code: %v, want ErrInvalidResetCode", wrongErr)
			}
			if validErr == nil {
				t.Fatal("precondition: the valid first password's verification was meant to fail")
			}
			if verified, pw := z.passwordState(u.id); verified || pw != "first-password-1" {
				t.Fatalf("precondition: want the valid password installed, unverified (verified=%v)", verified)
			}
			if !recoverWith(t, z, u.id, "first-password-2") {
				t.Fatal("a wrong code beside a valid one broke the valid one's recovery")
			}
		})
	}
}

// A squatter's password, set before any attempt, never qualifies — even
// when a fresh attempt record exists.
func TestFirstPasswordAttempt_AnEarlierPasswordNeverQualifies(t *testing.T) {
	ctx := context.Background()
	z := newFakeZitadel(t)
	s := z.addUser(victimEmail, false, attackerPassword)
	z.mu.Lock()
	s.passwordChanged = time.Now().Add(-10 * time.Minute)
	z.mu.Unlock()
	if err := z.client().SetMetadata(ctx, s.id, MetaFirstPasswordPrefix+"later", strconv.FormatInt(time.Now().UnixMilli(), 10)); err != nil {
		t.Fatal(err)
	}
	if err := provenBroker(z).ResetPassword(ctx, PasswordReset{UserID: s.id, Code: resetCode(t, z, s.id), NewPassword: "owner-password-1"}); err != nil {
		t.Fatal(err)
	}
	if verified, _ := z.passwordState(s.id); verified {
		t.Fatal("TAKEOVER: an attempt recorded after the squatter's password made a reset verify the account")
	}
}

// An attempt past firstPasswordAttemptTTL does not qualify, and is removed.
func TestFirstPasswordAttempt_AnExpiredAttemptDoesNotQualify(t *testing.T) {
	ctx := context.Background()
	z := newFakeZitadel(t)
	u := z.addUser("invitee@acme.com", false, "first-password-1")
	start := time.Now().Add(-firstPasswordAttemptTTL - time.Hour)
	z.mu.Lock()
	u.passwordChanged = start.Add(time.Minute) // installed by that attempt, long ago
	z.mu.Unlock()
	key := MetaFirstPasswordPrefix + "old"
	if err := z.client().SetMetadata(ctx, u.id, key, strconv.FormatInt(start.UnixMilli(), 10)); err != nil {
		t.Fatal(err)
	}
	if err := provenBroker(z).ResetPassword(ctx, PasswordReset{UserID: u.id, Code: resetCode(t, z, u.id), NewPassword: "first-password-2"}); err != nil {
		t.Fatal(err)
	}
	if verified, _ := z.passwordState(u.id); verified {
		t.Fatal("an expired first-password attempt let a reset verify the account")
	}
	if _, ok, _ := z.client().Metadata(ctx, u.id, key); ok {
		t.Fatal("the expired attempt was not removed")
	}
}
