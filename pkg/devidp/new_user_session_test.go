package devidp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// shrinkNewUserWait bounds the new-user wait for one test.
func shrinkNewUserWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := newUserVisibleWithin
	newUserVisibleWithin = d
	t.Cleanup(func() { newUserVisibleWithin = prev })
}

var testFlow = FlowConfig{ClientID: "client", RedirectURI: testAppBase + "/cb"}

// exactCalls counts calls to exactly "METHOD /path" (callCount matches a prefix).
func exactCalls(z *fakeZitadel, call string) int {
	z.mu.Lock()
	defer z.mu.Unlock()
	n := 0
	for _, c := range z.calls {
		if c == call {
			n++
		}
	}
	return n
}

// The issuer's user projection lags the creation: the first session
// creations after the account was made answer "User could not be found".
// Observed against a dev Zitadel 12s after it started, where the sign-up
// created the account and then answered "That account could not be created".
func TestSignUp_WaitsForTheNewAccountToBecomeVisible(t *testing.T) {
	z := newFakeZitadel(t)
	z.projectionLag = 3
	tokens, err := z.client().SignUp(context.Background(), testFlow,
		Registration{Email: "new@acme.com", Password: "a-long-password"})
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	u := z.userByEmail("new@acme.com")
	if tokens.AccessToken != "at-"+u.id {
		t.Errorf("tokens are for %q, want the new account %s", tokens.AccessToken, u.id)
	}
	if got := z.callCount("POST /v2/sessions"); got != 4 {
		t.Errorf("session creations = %d, want 4 (three refused while unprojected, one accepted)", got)
	}
	if got := exactCalls(z, "POST /v2/users"); got != 0 {
		t.Errorf("SignUp searched users %d times: the new account is named by the id its creation returned", got)
	}
}

// A new account that never becomes visible is still an account: the error
// says the sign-in failed, not the creation.
func TestSignUp_ASignInThatNeverSucceedsIsNotACreationFailure(t *testing.T) {
	shrinkNewUserWait(t, 50*time.Millisecond)
	z := newFakeZitadel(t)
	z.projectionLag = -1
	_, err := z.client().SignUp(context.Background(), testFlow,
		Registration{Email: "new@acme.com", Password: "a-long-password"})
	if !errors.Is(err, ErrSignInAfterSignUp) {
		t.Fatalf("SignUp error = %v, want ErrSignInAfterSignUp", err)
	}
	if !isUserNotFound(err) {
		t.Errorf("SignUp error = %v, want it to carry the issuer's QUERY-Dfbg2", err)
	}
	if z.userByEmail("new@acme.com") == nil {
		t.Fatal("the account was not created")
	}
}

// Only a sign-in after a creation waits. An ordinary sign-in naming an
// unknown account fails at once: waiting would make "no such account"
// slower than "wrong password", an account oracle.
func TestSignIn_AnUnknownAccountIsNotRetried(t *testing.T) {
	z := newFakeZitadel(t)
	_, err := z.client().SignIn(context.Background(), testFlow, Credentials{LoginName: "ghost@acme.com", Password: "a-long-password"})
	if !isUserNotFound(err) {
		t.Fatalf("SignIn error = %v, want QUERY-Dfbg2", err)
	}
	if got := z.callCount("POST /v2/sessions"); got != 1 {
		t.Errorf("session creations = %d, want 1", got)
	}
}

// Any refusal other than not-found returns at once, even for a new account:
// a wrong password is an answer, not lag.
func TestCreateSessionForNewUser_OnlyNotFoundIsRetried(t *testing.T) {
	z := newFakeZitadel(t)
	u := z.addUser("a@acme.com", true, "a-long-password")
	_, err := z.client().createSessionForNewUser(context.Background(), Credentials{UserID: u.id, Password: "wrong-password"})
	if err == nil || isUserNotFound(err) {
		t.Fatalf("error = %v, want the wrong-password refusal", err)
	}
	if got := z.callCount("POST /v2/sessions"); got != 1 {
		t.Errorf("session creations = %d, want 1", got)
	}
}

func TestCreateSessionForNewUser_StopsWhenTheContextEnds(t *testing.T) {
	z := newFakeZitadel(t)
	u := z.addUser("a@acme.com", true, "a-long-password")
	z.mu.Lock()
	u.unprojected = -1
	z.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := z.client().createSessionForNewUser(ctx, Credentials{UserID: u.id, Password: "a-long-password"})
	if !errors.Is(err, context.DeadlineExceeded) || !isUserNotFound(err) {
		t.Fatalf("error = %v, want the not-found and the context's deadline", err)
	}
	if waited := time.Since(start); waited > newUserVisibleWithin/2 {
		t.Errorf("waited %s after the context ended", waited)
	}
}

// The policy's mail-less sign-up creates and signs in in one request, so it
// waits for the new account too.
func TestRegisterWithoutMail_WaitsForTheNewAccount(t *testing.T) {
	z := newFakeZitadel(t)
	z.projectionLag = 2
	h := newHarnessOn(t, z, LoginPolicy{Present: true, Version: 1, Signup: SignupOpen, SignupRole: "member",
		Methods: []string{MethodPassword}, Mail: MailNone})
	if status, body := h.browser().post("/auth/register", map[string]any{"email": "new@else.com", "password": "a-long-password"}); status != 200 {
		t.Fatalf("register: %d %v", status, body)
	}
}

// A social sign-in that creates the account signs it in once it is visible.
func TestSocialSignUp_WaitsForTheNewAccount(t *testing.T) {
	z := newFakeZitadel(t)
	z.projectionLag = 2
	h := newHarnessOn(t, z, policyWith(SignupOpen, allMethods, google))
	b := h.browser()
	id := startSocial(t, b)
	z.addIntent(id, "tok", intentResult("g@acme.com", true))
	if status, body := b.post("/auth/social/callback", map[string]any{"id": id, "token": "tok"}); status != 200 {
		t.Fatalf("callback: %d %v", status, body)
	}
}
