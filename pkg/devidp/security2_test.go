package devidp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The second security re-review of forge#1821 (at 72d47a1c): the race in the
// takeover fix, the sealed-password size oracle, and the low findings.

// ── A. No sign-in in the window between "verified" and "revoked" ───────

// raceProof runs proof while an attacker signs in with the planted password
// as fast as it can, with the issuer holding each proof's answer for a while
// after marking the address verified. It returns how many sign-ins got tokens.
func raceProof(t *testing.T, h *harness, proof func()) int64 {
	t.Helper()
	h.z.mu.Lock()
	h.z.proofDelay = 150 * time.Millisecond
	h.z.mu.Unlock()
	stop := plantedSignIns(h)
	proof()
	time.Sleep(20 * time.Millisecond)
	return stop()
}

// plantedSignIns signs in to victim@acme.com with the planted password as
// fast as it can until the returned stop is called, which reports how many
// of those sign-ins got tokens.
func plantedSignIns(h *harness) (stop func() int64) {
	var got atomic.Int64
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := FlowConfig{ClientID: "c", RedirectURI: "r", RequireVerifiedEmail: true}
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := h.z.client().SignIn(context.Background(), cfg, Credentials{LoginName: "victim@acme.com", Password: attackerPassword}); err == nil {
					got.Add(1)
				}
			}
		}()
	}
	return func() int64 {
		close(done)
		wg.Wait()
		return got.Load()
	}
}

func TestTakeover_NoTokensWhileTheProofIsInFlight(t *testing.T) {
	for name, proof := range map[string]func(*testing.T, *harness, *fakeUser){
		"verify": func(t *testing.T, h *harness, u *fakeUser) {
			if status, body := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": h.z.lastMail(t).code}); status != 200 {
				t.Fatalf("verify: %d %v", status, body)
			}
		},
		"magic link": func(t *testing.T, h *harness, u *fakeUser) {
			b := h.browser()
			b.post("/auth/magic/request", map[string]any{"email": "victim@acme.com"})
			if status, body := b.post("/auth/magic/consume", map[string]any{"code": h.z.lastMail(t).code}); status != 200 {
				t.Fatalf("consume: %d %v", status, body)
			}
		},
		"invitation": func(t *testing.T, h *harness, u *fakeUser) {
			h.z.mu.Lock()
			u.inviteCode = "inv-race"
			h.z.mu.Unlock()
			if status, body := h.browser().post("/auth/invite/accept", map[string]any{"userID": u.id, "code": "inv-race", "method": MethodPasskey}); status != 200 {
				t.Fatalf("accept: %d %v", status, body)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, policyWith(SignupOpen, allMethods))
			u := preRegister(t, h)
			if n := raceProof(t, h, func() { proof(t, h, u) }); n != 0 {
				t.Fatalf("TAKEOVER RACE: the planted password got tokens %d time(s) while the proof was in flight", n)
			}
			if h.z.user(u.id).locked {
				t.Fatal("the account was left locked after a successful proof")
			}
			if signsIn(t, h, "victim@acme.com", attackerPassword) {
				t.Fatal("the planted password works after the proof")
			}
		})
	}
}

func TestProof_AWrongCodeUnlocksAndAPreviouslyLockedAccountStaysLocked(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	u := preRegister(t, h)
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": "wrong"}); status != 401 {
		t.Fatalf("wrong code: %d", status)
	}
	if h.z.user(u.id).locked {
		t.Fatal("a wrong code left the account locked")
	}
	h.z.mu.Lock()
	u.locked = true
	h.z.mu.Unlock()
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": h.z.lastMail(t).code}); status != 200 {
		t.Fatalf("verify: %d", status)
	}
	if !h.z.user(u.id).locked {
		t.Fatal("a proof unlocked an account the issuer had locked")
	}
}

// The security re-review at 9703d20c: an attacker who knows the user id (the
// legacy sign-up answers it as `subject`) keeps a WRONG-code proof in flight
// while the owner proves the address. The failing proof must not lift the
// lock under the succeeding one — on one replica (the per-account proof
// lock) or across two (the MetaProofLock mark).
func TestTakeover_AConcurrentFailedProofNeverUnlocksTheRealOne(t *testing.T) {
	owner := map[string]func(*testing.T, *harness, string){
		"verify": func(t *testing.T, h *harness, code string) {
			if status, body := h.browser().post("/auth/verify", map[string]any{"userID": h.z.userByEmail("victim@acme.com").id, "code": code}); status != 200 {
				t.Errorf("owner's verify: %d %v", status, body)
			}
		},
		"magic link": func(t *testing.T, h *harness, _ string) {
			b := h.browser()
			b.post("/auth/magic/request", map[string]any{"email": "victim@acme.com"})
			if status, body := b.post("/auth/magic/consume", map[string]any{"code": h.z.lastMail(t).code}); status != 200 {
				t.Errorf("owner's magic link: %d %v", status, body)
			}
		},
	}
	for _, replicas := range []int{1, 2} {
		for name, prove := range owner {
			t.Run(fmt.Sprintf("%d replica(s)/%s", replicas, name), func(t *testing.T) {
				policy := policyWith(SignupOpen, allMethods)
				attacker := newHarness(t, policy)
				ownerSide := attacker
				if replicas == 2 {
					ownerSide = newHarnessOn(t, attacker.z, policy)
				}
				u := preRegister(t, attacker)
				code := attacker.z.lastMail(t).code
				attacker.z.mu.Lock()
				attacker.z.refusedProofDelay = 150 * time.Millisecond
				attacker.z.proofDelay = 150 * time.Millisecond
				attacker.z.mu.Unlock()

				stop := plantedSignIns(attacker)
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					if status, _ := attacker.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": "wrong"}); status != 401 {
						t.Errorf("attacker's wrong code: %d, want 401", status)
					}
				}()
				time.Sleep(60 * time.Millisecond)
				prove(t, ownerSide, code)
				wg.Wait()
				time.Sleep(20 * time.Millisecond)
				if n := stop(); n != 0 {
					t.Fatalf("TAKEOVER RACE: the planted password got tokens %d time(s) while a failed proof ran beside the real one", n)
				}
				got := attacker.z.user(u.id)
				if !got.verified {
					t.Fatal("the owner's proof did not verify the address")
				}
				if got.locked {
					t.Fatal("the account was left locked after the owner's proof")
				}
				if _, marked := got.metadata[MetaProofLock]; marked {
					t.Error("the proof lock mark survived a successful proof")
				}
				if signsIn(t, attacker, "victim@acme.com", attackerPassword) {
					t.Fatal("the planted password works after the proof: revocation did not run")
				}
			})
		}
	}
}

// A lock with no mark is the issuer's (or a failed revocation's): neither a
// wrong code nor a proof lifts it.
func TestProof_AnUnmarkedLockIsNeverLifted(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	u := preRegister(t, h)
	h.z.mu.Lock()
	u.locked = true
	h.z.mu.Unlock()
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": "wrong"}); status != 401 {
		t.Fatalf("wrong code: %d", status)
	}
	if !h.z.user(u.id).locked {
		t.Fatal("a wrong code lifted a lock that was not a proof's")
	}
	// A FRESH mark from a proof that died mid-way is a proof's lock: the
	// next failed proof takes it over and may lift it.
	h.z.mu.Lock()
	u.metadata = map[string]string{MetaProofLock: newProofMark(time.Now().Add(-time.Minute))}
	h.z.mu.Unlock()
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": "wrong"}); status != 401 {
		t.Fatalf("wrong code: %d", status)
	}
	if h.z.user(u.id).locked {
		t.Fatal("a fresh proof lock was never lifted")
	}
}

// The re-review at ce91073b, finding 4: a mark some earlier proof failed to
// clear must not make a later issuer lockout look like a proof's. A mark
// older than proofLockTTL names no proof; the lock under it stays, through
// a wrong code and through a successful proof alike.
func TestProof_AStaleMarkLeavesTheIssuersLockoutInPlace(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	u := preRegister(t, h)
	h.z.mu.Lock()
	u.locked = true
	u.metadata = map[string]string{MetaProofLock: newProofMark(time.Now().Add(-proofLockTTL - time.Minute))}
	h.z.mu.Unlock()
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": "wrong"}); status != 401 {
		t.Fatalf("wrong code: %d", status)
	}
	if !h.z.user(u.id).locked {
		t.Fatal("a wrong code lifted an issuer lockout under a stale mark")
	}
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": h.z.lastMail(t).code}); status != 200 {
		t.Fatalf("verify: %d", status)
	}
	if !h.z.user(u.id).locked {
		t.Fatal("a proof lifted an issuer lockout under a stale mark")
	}
	for mark, fresh := range map[string]bool{
		"": false, "stale": false, "x.nonce": false, "123.": false,
		newProofMark(time.Now()): true, newProofMark(time.Now().Add(-proofLockTTL)): false,
		newProofMark(time.Now().Add(time.Minute)): true, newProofMark(time.Now().Add(proofLockTTL + time.Minute)): false,
	} {
		if got := freshProofMark(mark, time.Now()); got != fresh {
			t.Errorf("freshProofMark(%q) = %v, want %v", mark, got, fresh)
		}
	}
}

// Finding 4, the other half: a mark that fails to clear once is retried.
func TestProof_AMarkThatFailsToClearOnceIsRetried(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	u := preRegister(t, h)
	h.z.mu.Lock()
	h.z.failMetadataDeletes = 1
	h.z.mu.Unlock()
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": h.z.lastMail(t).code}); status != 200 {
		t.Fatalf("verify: %d", status)
	}
	if _, marked := h.z.user(u.id).metadata[MetaProofLock]; marked {
		t.Fatal("one failed delete left the proof lock mark on the account")
	}
}

// Finding 1: on another replica, a failed proof cleared its mark, this
// proof read "locked, no mark" (so not its lock to lift), and then the
// failed proof's unlock landed before this proof's re-lock. The re-lock
// locked the account, so this call lifts it: before, it stayed locked for
// good (locked, unmarked — read by every later proof as the issuer's).
func TestProof_ARelockThatLockedTheAccountIsLiftedAfterwards(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	u := preRegister(t, h)
	code := h.z.lastMail(t).code
	h.z.mu.Lock()
	u.locked = true // the other replica's lock, its mark already cleared
	h.z.mu.Unlock()
	c := h.z.client()
	ctx := context.Background()
	before, err := c.FindUserByID(ctx, u.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.routes.proveAddress(ctx, before, func() error {
		if err := c.UnlockUser(ctx, u.id); err != nil { // the other replica's unlock lands
			return err
		}
		return c.VerifyEmail(ctx, u.id, code)
	}); err != nil {
		t.Fatal(err)
	}
	got := h.z.user(u.id)
	if got.locked {
		t.Fatal("OWNER LOCKED OUT: the proof re-locked the account and never lifted that lock")
	}
	if !got.verified || signsIn(t, h, "victim@acme.com", attackerPassword) {
		t.Fatal("the proof did not verify and revoke")
	}
}

// Finding 1, the other half: an unlock that fails after a refused proof puts
// the mark back, so the lock stays a proof's and the owner's proof lifts it.
func TestProof_AFailedUnlockPutsTheMarkBack(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	u := preRegister(t, h)
	h.z.mu.Lock()
	h.z.failUnlock = true
	h.z.mu.Unlock()
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": "wrong"}); status != 401 {
		t.Fatalf("wrong code: %d", status)
	}
	got := h.z.user(u.id)
	if !got.locked || !freshProofMark(got.metadata[MetaProofLock], time.Now()) {
		t.Fatalf("after a failed unlock: locked=%v mark=%q; want locked under a fresh mark", got.locked, got.metadata[MetaProofLock])
	}
	h.z.mu.Lock()
	h.z.failUnlock = false
	h.z.mu.Unlock()
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": h.z.lastMail(t).code}); status != 200 {
		t.Fatalf("verify: %d", status)
	}
	if h.z.user(u.id).locked {
		t.Fatal("OWNER LOCKED OUT: the owner's proof did not lift a lock a failed unlock left")
	}
}

// Finding 2: a client that hangs up right after the proof succeeded must not
// leave the account verified, locked and marked: the steps after the proof
// run detached from the request.
func TestProof_TheCleanupOutlivesTheRequest(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	u := preRegister(t, h)
	code := h.z.lastMail(t).code
	c := h.z.client()
	before, err := c.FindUserByID(context.Background(), u.id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := h.routes.proveAddress(ctx, before, func() error {
		defer cancel() // the client goes away the moment the proof lands
		return c.VerifyEmail(ctx, u.id, code)
	}); err != nil {
		t.Fatal(err)
	}
	got := h.z.user(u.id)
	if _, marked := got.metadata[MetaProofLock]; got.locked || marked {
		t.Fatalf("after a cancelled request: locked=%v marked=%v; want neither", got.locked, marked)
	}
	if signsIn(t, h, "victim@acme.com", attackerPassword) {
		t.Fatal("the planted password survived: the revocation stopped with the request")
	}
}

func TestRevokeCredentials_AnyOtherRefusalFailsTheRevocation(t *testing.T) {
	if !isAbsent(&apiError{status: 404, body: `{"code":5,"details":[{"id":"COMMAND-Hd9sd"}]}`}) ||
		!isAbsent(&apiError{status: 404, body: `{"code":5,"details":[{"id":"COMMAND-Sr3h3"}]}`}) {
		t.Fatal("the issuer's \"no such factor\" is not read as absent")
	}
	for _, body := range []string{`{"code":3,"details":[{"id":"COMMAND-SF3fg"}]}`, `{"code":9}`, `not json`} {
		if isAbsent(&apiError{status: 400, body: body}) {
			t.Errorf("isAbsent(400 %s) = true: an unrelated refusal would pass for \"no factor\"", body)
		}
	}
}

func TestRevokeCredentials_SecondFactorsAndSessions(t *testing.T) {
	z := newFakeZitadel(t)
	u := z.addUser("v@acme.com", false, "planted-password")
	z.mu.Lock()
	u.totp = true
	z.sessions["s-old"] = &fakeSession{token: "t", userID: u.id, verified: []string{"password"}}
	z.mu.Unlock()
	if _, err := z.client().RevokeCredentials(context.Background(), u.id); err != nil {
		t.Fatal(err)
	}
	if z.user(u.id).totp {
		t.Error("a planted TOTP survived the revocation")
	}
	if !z.sessions["s-old"].deleted {
		t.Error("a session from before the revocation was not ended")
	}
}

func TestTakeover_RevokeAndLockFailingStillReplacesThePassword(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	u := preRegister(t, h)
	h.z.mu.Lock()
	h.z.failLinks = true
	h.z.mu.Unlock()
	if status, _ := h.browser().post("/auth/verify", map[string]any{"userID": u.id, "code": h.z.lastMail(t).code}); status != http.StatusBadGateway {
		t.Fatalf("verify answered %d, want 502", status)
	}
	if !h.z.user(u.id).locked {
		t.Fatal("the account is not locked")
	}
	if h.z.user(u.id).password == attackerPassword {
		t.Fatal("the planted password was not replaced on the last attempt")
	}
}

// ── B. The pending password's sealed size depends only on its length ───

func TestRegister_HTMLPasswordsSealForEveryAddressAlike(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	h.z.addUser("verified@acme.com", true, "x-password-1")
	h.z.addUser("pending@acme.com", false, "")
	html := strings.Repeat("<>&", 85) + "x" // 256 bytes; 1531 if HTML-escaped
	lengths := map[string]int{}
	for _, email := range []string{"verified@acme.com", "pending@acme.com", "brand-new@acme.com"} {
		b := h.browser()
		status, body := b.post("/auth/register", map[string]any{"email": email, "password": html})
		if status != 202 {
			t.Fatalf("%s: %d %v", email, status, body)
		}
		lengths[email] = len(cookieValue(b, "app_pwp"))
		if email == "brand-new@acme.com" {
			u := h.z.userByEmail(email)
			if len(u.id) != 18 {
				t.Fatalf("fake ids must be issuer-shaped (18 digits), got %q", u.id)
			}
			if status, body := b.post("/auth/verify", map[string]any{"userID": u.id, "code": h.z.lastMail(t).code}); status != 200 || body["password"] != nil {
				t.Fatalf("verify: %d %v", status, body)
			}
			if !signsIn(t, h, email, html) {
				t.Fatal("the HTML-character password was lost between sign-up and verification")
			}
		}
	}
	if lengths["verified@acme.com"] == 0 || lengths["verified@acme.com"] != lengths["pending@acme.com"] || lengths["pending@acme.com"] != lengths["brand-new@acme.com"] {
		t.Fatalf("sealed password cookie lengths differ by account state: %v", lengths)
	}
}

func TestRegister_TooLongIsOneAnswerBeforeTheLookup(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods))
	h.z.addUser("verified@acme.com", true, "")
	long := strings.Repeat("a", maxPasswordBytes+1)
	for _, email := range []string{"verified@acme.com", "brand-new@acme.com"} {
		before := h.z.callCount("POST /v2/users")
		status, body := h.browser().post("/auth/register", map[string]any{"email": email, "password": long})
		want(t, status, body, 400, CodePasswordTooLong)
		if h.z.callCount("POST /v2/users") != before {
			t.Fatalf("%s: the address was looked up for a password that was refused anyway", email)
		}
	}
}

func TestRegister_ASealFailureIsNeverACheckYourEmail(t *testing.T) {
	h := newHarness(t, policyWith(SignupOpen, allMethods), func(c *SignInRoutesConfig) { c.Client.Token = "" })
	status, _ := h.browser().post("/auth/register", map[string]any{"email": "new@acme.com", "password": "a-long-password"})
	if status == 202 {
		t.Fatal("register answered 202 though the password could not be kept")
	}
}

// ── Low ────────────────────────────────────────────────────────────────

func TestVerifyResendHasTheResponseFloor(t *testing.T) {
	const floor = 120 * time.Millisecond
	h := newHarness(t, policyWith(SignupOpen, allMethods), func(c *SignInRoutesConfig) { c.ResponseFloor = floor })
	start := time.Now()
	h.browser().post("/auth/verify/resend", map[string]any{"email": "nobody@acme.com"})
	if took := time.Since(start); took < floor {
		t.Fatalf("verify/resend answered in %v, under the %v floor", took, floor)
	}
}

func TestSignupRoleIsGrantedOnceOnVerify(t *testing.T) {
	p := policyWith(SignupOpen, allMethods)
	p.SignupRole = "member"
	h := newHarness(t, p)
	b := h.browser()
	b.post("/auth/register", map[string]any{"email": "self@acme.com", "password": "self-password-1"})
	u := h.z.userByEmail("self@acme.com")
	b.post("/auth/verify", map[string]any{"userID": u.id, "code": h.z.lastMail(t).code})
	if n := h.z.callCount("POST /management/v1/users/" + u.id + "/grants"); n != 1 {
		t.Fatalf("the sign-up role was asked for %d times, want 1", n)
	}
}

func TestClientKeyCutsIPv6ToItsSlash64(t *testing.T) {
	key := func(remote string) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		return ClientKey(req, 0)
	}
	a, b := key("[2001:db8:1:2:aaaa::1]:443"), key("[2001:db8:1:2:ffff:ffff:ffff:ffff]:443")
	if a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("one /64 gave keys %q and %q", a, b)
	}
	if key("[2001:db8:1:3::1]:443") == a {
		t.Fatal("another /64 shares the key")
	}
	if got := key("198.51.100.7:443"); got != "198.51.100.7" {
		t.Fatalf("IPv4 key %q", got)
	}
}

func TestKeyedMutex_SerialisesAKeyAndForgetsIt(t *testing.T) {
	var k keyedMutex
	release, err := k.lock(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := k.lock(context.Background(), "u2")
	if err != nil {
		t.Fatal("another key waited on u1")
	}
	other()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := k.lock(ctx, "u1"); err == nil {
		t.Fatal("a second holder of u1 got in while the first held it")
	}
	release()
	again, err := k.lock(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	again()
	if len(k.keys) != 0 {
		t.Fatalf("keys outlive their holders: %v", k.keys)
	}
}
