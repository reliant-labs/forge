package devidp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SignInRoutes serves the sign-in methods a LOGIN_POLICY document turns on —
// passkeys, magic links, social sign-in, invitations, address verification
// and sign-up per mode — for an app's native login broker.
//
// The app's own internal/app/login_broker.go mounts each handler on its own
// route (the route constants below), so an app can drop, wrap or replace any
// one of them. The broker keeps what is the app's: the session cookie
// (SignedIn), reading it back (SessionToken) and the password checklist
// (PasswordPolicy, RejectPassword).
//
// ── THE RULE EVERY HANDLER HERE FOLLOWS ───────────────────────────────
//
// The broker's credential can do almost anything to any account: set a
// password, mark an address verified, mint codes, register a passkey. So
// every privileged call below follows a PROOF that succeeded for the same
// user IN THE SAME REQUEST — an invitation code, an email-verification code,
// an emailed one-time code, a passkey assertion, a provider intent, or a
// session token the issuer validated. The user id a privileged call acts on
// comes from that proof, never from a request body.
//
// And no route issues a session for an unverified address while the policy
// requires verified email: CompleteSignIn reads the session back and refuses.
type SignInRoutes struct {
	cfg    SignInRoutesConfig
	origin string // scheme://host[:port] of AppBase: the only origin a passkey may come from
	rpID   string
	seal   *sealer
	decoy  []byte

	magicByAddress  *limiter
	magicByIP       *limiter
	magicByCode     *limiter
	passkeyByIP     *limiter
	verifyResendBy  *limiter
	verifyResendIP  *limiter
	codeByUser      *limiter
	registerByIP    *limiter
	socialStartByIP *limiter
}

// The routes. The scaffolded broker mounts each with
// mux.Handle(<constant>, http.HandlerFunc(routes.Handle<Name>)); the
// scaffolded frontend is written against these exact paths.
const (
	PasskeyBeginPath          = "POST /auth/passkey/begin"
	PasskeyFinishPath         = "POST /auth/passkey/finish"
	PasskeyRegisterBeginPath  = "POST /auth/passkey/register/begin"
	PasskeyRegisterFinishPath = "POST /auth/passkey/register/finish"
	PasskeysPath              = "GET /auth/passkeys"
	MagicRequestPath          = "POST /auth/magic/request"
	MagicConsumePath          = "POST /auth/magic/consume"
	SocialStartPath           = "POST /auth/social/start"
	SocialCallbackPath        = "POST /auth/social/callback"
	InviteAcceptPath          = "POST /auth/invite/accept"
	VerifyPath                = "POST /auth/verify"
	VerifyResendPath          = "POST /auth/verify/resend"
)

// Error codes in a refusal's `code` field, which the sign-in screen branches on.
const (
	CodeEmailUnverified    = "email_unverified"
	CodePasskeyFailed      = "passkey_failed"
	CodeNotSignedIn        = "not_signed_in"
	CodeInvalidCode        = "invalid_code"
	CodeMagicOtherBrowser  = "magic_other_browser"
	CodeSocialFailed       = "social_failed"
	CodeSocialLinkRequired = "social_link_required"
	CodeSocialNoAccount    = "social_no_account"
	CodeSignupNotAllowed   = "signup_not_allowed"
	CodePasswordRequired   = "password_required"
	CodeMethodNotOffered   = "method_not_offered"
	CodeUnknownProvider    = "unknown_provider"
	CodeRateLimited        = "rate_limited"
	CodeAccountExists      = "account_exists"
	CodeSocialTaken        = "social_taken"
)

// The sentences the browser is shown. None says whether an address has an
// account.
const (
	msgMagicSent     = "If that address can sign in, a sign-in link is on its way."
	msgVerifySent    = "Check your email: we sent a link to confirm the address."
	msgVerifyResent  = "If that address is waiting to be confirmed, a new link is on its way."
	msgUnverified    = "Confirm your email address first: use the link we sent, or ask for a new one."
	msgPasskeyFailed = "That passkey was not accepted."
	msgInvalidCode   = "That code or link is not valid. Codes are single-use and expire; ask for a new one."
	msgOtherBrowser  = "This link only works in the browser where you asked for it. Type the code from the email there."
	msgSocialFailed  = "That sign-in did not complete. Please try again."
	msgSocialLink    = "Sign in another way, then connect %s from your account."
	msgSocialNoAcct  = "No account for this address — ask an owner for an invitation."
	msgSignupDomain  = "Accounts with that email address cannot be created here."
	msgTooMany       = "Too many attempts. Please wait a while before trying again."
	msgUnavailable   = "Sign-in is unavailable right now. Please try again."
	msgNotSignedIn   = "Sign in first."
	msgMethod        = "That sign-in method is not offered here."
	msgAccountExists = "That address already has an account. Sign in instead."
	msgSocialTaken   = "That %s account is already connected to another user. Use another one, or another way to sign in."
)

// SignInRoutesConfig is what the app's broker supplies.
type SignInRoutesConfig struct {
	Client *Client
	// Flow is the OAuth client the session is finalized into. Its
	// RequireVerifiedEmail is set from Policy.
	Flow   FlowConfig
	Policy LoginPolicy
	// AppBase is the app's public origin plus base path ("https://app.example.com"),
	// from idp_login_uri. Every mailed link and the passkey origin derive from
	// it — never from a request header the caller controls.
	AppBase string
	// ProjectID is the identity project (jwt_audience), which a signup_role
	// is granted on. "" grants nothing.
	ProjectID string
	// CookiePrefix names the binding cookies (<prefix>_pk, _ml, …).
	CookiePrefix string
	SecureCookie bool
	Logger       *slog.Logger

	// SignedIn stores tokens as the app's session and returns the identity
	// body the browser renders. Required.
	SignedIn func(http.ResponseWriter, Tokens) any
	// SessionToken returns the request's session access token, "" when none.
	// Required.
	SessionToken func(*http.Request) string
	// PasswordPolicy is the checklist a new password is held to before the
	// issuer is asked (nil: none). RejectPassword answers a rejected one.
	PasswordPolicy func(context.Context) (PasswordPolicy, error)
	RejectPassword func(ctx context.Context, w http.ResponseWriter, password string, err *PasswordPolicyError)
}

// NewSignInRoutes validates the configuration. It refuses a policy-bearing
// broker with no AppBase: every mailed link and the passkey origin need it.
func NewSignInRoutes(cfg SignInRoutesConfig) (*SignInRoutes, error) {
	if cfg.Client == nil || cfg.SignedIn == nil || cfg.SessionToken == nil {
		return nil, fmt.Errorf("sign-in routes need a client, SignedIn and SessionToken")
	}
	u, err := url.Parse(strings.TrimRight(cfg.AppBase, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("sign-in routes need the app's public origin (idp_login_uri), got %q", cfg.AppBase)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CookiePrefix == "" {
		cfg.CookiePrefix = "app"
	}
	cfg.AppBase = strings.TrimRight(cfg.AppBase, "/")
	cfg.Flow.RequireVerifiedEmail = cfg.Policy.RequireVerifiedEmail
	rpID := strings.ToLower(strings.TrimSpace(cfg.Policy.PasskeyRPID))
	if rpID == "" {
		rpID = u.Hostname()
	}
	host := strings.ToLower(u.Hostname())
	if host != rpID && !strings.HasSuffix(host, "."+rpID) {
		return nil, fmt.Errorf("passkey_rp_id %q is not %q or a parent domain of it", rpID, host)
	}
	decoy := make([]byte, 32)
	if _, err := rand.Read(decoy); err != nil {
		return nil, err
	}
	client := cfg.Client
	r := &SignInRoutes{
		cfg:    cfg,
		origin: u.Scheme + "://" + u.Host,
		rpID:   rpID,
		seal:   &sealer{key: func() []byte { return []byte(client.bearer()) }, now: time.Now},
		decoy:  decoy,

		magicByAddress:  newLimiter(3, 15*time.Minute),
		magicByIP:       newLimiter(10, time.Hour),
		magicByCode:     newLimiter(5, 15*time.Minute),
		passkeyByIP:     newLimiter(30, 15*time.Minute),
		verifyResendBy:  newLimiter(3, time.Hour),
		verifyResendIP:  newLimiter(10, time.Hour),
		codeByUser:      newLimiter(5, time.Hour),
		registerByIP:    newLimiter(10, time.Hour),
		socialStartByIP: newLimiter(30, 15*time.Minute),
	}
	return r, nil
}

// RPID is the passkey relying-party id in use.
func (r *SignInRoutes) RPID() string { return r.rpID }

// Options is the v2 part of GET /auth/options.
type Options struct {
	SelfSignup bool            `json:"selfSignup"`
	Signup     string          `json:"signup"`
	Methods    []string        `json:"methods"`
	Social     []OptionsSocial `json:"social"`
	Mail       bool            `json:"mail"`
}

// OptionsSocial is one social button, without its issuer id.
type OptionsSocial struct {
	Type  string `json:"type"`
	Label string `json:"label"`
}

// OptionsFor is what GET /auth/options answers for a policy. selfSignup is
// the legacy switch (development or auth_self_signup), used only when the
// policy is the legacy one.
func OptionsFor(p LoginPolicy, legacySelfSignup bool) Options {
	if !p.Present {
		signup := SignupInvite
		if legacySelfSignup {
			signup = SignupOpen
		}
		return Options{SelfSignup: legacySelfSignup, Signup: signup, Methods: []string{MethodPassword}, Social: []OptionsSocial{}}
	}
	o := Options{SelfSignup: p.RegisterServed(), Signup: p.Signup, Methods: append([]string{}, p.Methods...),
		Social: []OptionsSocial{}, Mail: p.MailDelivers()}
	for _, s := range p.Social {
		label := s.Label
		if label == "" {
			label = s.Type
		}
		o.Social = append(o.Social, OptionsSocial{Type: s.Type, Label: label})
	}
	return o
}

// ── Binding cookies ────────────────────────────────────────────────────

const (
	cookiePasskey  = "_pk"
	cookieRegister = "_pkr"
	cookieMagic    = "_ml"
	cookieSocial   = "_sx"
	cookieEnroll   = "_enroll"
	cookieLink     = "_sxl"
)

type cookieSpec struct {
	suffix   string
	ttl      time.Duration
	sameSite http.SameSite
}

var (
	passkeyCookie  = cookieSpec{cookiePasskey, 5 * time.Minute, http.SameSiteStrictMode}
	registerCookie = cookieSpec{cookieRegister, 5 * time.Minute, http.SameSiteStrictMode}
	magicCookie    = cookieSpec{cookieMagic, 10 * time.Minute, http.SameSiteLaxMode}
	socialCookie   = cookieSpec{cookieSocial, 10 * time.Minute, http.SameSiteLaxMode}
	enrollCookie   = cookieSpec{cookieEnroll, 10 * time.Minute, http.SameSiteStrictMode}
	// linkCookie holds a social account waiting for its new account's
	// address to be proven: the verification link is opened from a mail
	// client, so Lax, and it lives as long as a person takes to read mail.
	linkCookie = cookieSpec{cookieLink, time.Hour, http.SameSiteLaxMode}
)

// setBinding writes a sealed binding cookie. Path=/ because the dev server
// proxies the API under /_api: a path-scoped cookie would never come back.
func (r *SignInRoutes) setBinding(w http.ResponseWriter, c cookieSpec, payload any) error {
	v, err := r.seal.seal(c.suffix, payload, c.ttl)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: r.cfg.CookiePrefix + c.suffix, Value: v, Path: "/", MaxAge: int(c.ttl.Seconds()),
		HttpOnly: true, Secure: r.cfg.SecureCookie, SameSite: c.sameSite,
	})
	return nil
}

func (r *SignInRoutes) readBinding(req *http.Request, c cookieSpec, out any) bool {
	ck, err := req.Cookie(r.cfg.CookiePrefix + c.suffix)
	if err != nil {
		return false
	}
	return r.seal.open(c.suffix, ck.Value, out) == nil
}

func (r *SignInRoutes) clearBinding(w http.ResponseWriter, c cookieSpec) {
	http.SetCookie(w, &http.Cookie{
		Name: r.cfg.CookiePrefix + c.suffix, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: r.cfg.SecureCookie, SameSite: c.sameSite,
	})
}

// sessionBinding is a challenge session held across two requests, or a decoy.
type sessionBinding struct {
	Decoy bool   `json:"d,omitempty"`
	ID    string `json:"i,omitempty"`
	Token string `json:"t,omitempty"`
}

// magicBinding is a pending magic link: a one-time-code session ("s"), an
// email-verification code for an unverified account ("v"), or nothing ("n",
// the address cannot sign in — indistinguishable from outside).
type magicBinding struct {
	Kind   string `json:"k"`
	ID     string `json:"i,omitempty"`
	Token  string `json:"t,omitempty"`
	UserID string `json:"u,omitempty"`
	New    bool   `json:"n,omitempty"`
}

type enrollBinding struct {
	UserID string `json:"u"`
	CodeID string `json:"c"`
	Code   string `json:"s"`
}

type registerBinding struct {
	UserID    string `json:"u"`
	PasskeyID string `json:"p"`
}

// socialBinding is a social sign-in in flight. InviteUserID and InviteCode
// are set when it accepts an invitation: the code is checked at the callback,
// in the same request that links the provider account, and never before, so
// an abandoned provider round trip spends nothing.
type socialBinding struct {
	IntentID     string `json:"i"`
	ReturnTo     string `json:"r"`
	InviteUserID string `json:"u,omitempty"`
	InviteCode   string `json:"c,omitempty"`
}

// pendingLinkBinding is a provider account that asserted an UNVERIFIED
// address for a new account. It is linked only when the browser that holds
// it proves that address's code (HandleVerify); linking at creation would let
// whoever's provider reports a victim's address keep a way into the account
// once the victim confirms it.
type pendingLinkBinding struct {
	UserID string  `json:"u"`
	Link   IDPLink `json:"l"`
}

// ── Passkey sign-in ────────────────────────────────────────────────────

type passkeyBeginRequest struct {
	Email string `json:"email"`
}

// HandlePasskeyBegin requests a passkey challenge for an address. An unknown
// address (or one the issuer refuses a challenge for) gets a DECOY of the same
// shape, so the answer does not say whether the account exists. With no
// address, the enrolment cookie of an invite or verification names the user.
func (r *SignInRoutes) HandlePasskeyBegin(w http.ResponseWriter, req *http.Request) {
	var body passkeyBeginRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	if !r.passkeyByIP.reserve(clientIP(req)) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, msgTooMany)
		return
	}
	email := normalizeEmail(body.Email)
	userID := ""
	if email == "" {
		var e enrollBinding
		if !r.readBinding(req, enrollCookie, &e) {
			writeErr(w, http.StatusBadRequest, "", "Enter your email address.")
			return
		}
		userID = e.UserID
	} else if u, err := r.cfg.Client.FindUserByEmail(ctx, email); err == nil {
		userID = u.ID
	} else if !errors.Is(err, ErrUserNotFound) {
		r.cfg.Logger.ErrorContext(ctx, "passkey sign-in: user lookup failed", "error", err)
	}
	if userID != "" {
		cs, err := r.cfg.Client.CreateChallengeSession(ctx, "", userID, Challenge{WebAuthnDomain: r.rpID})
		if err == nil {
			if err := r.setBinding(w, passkeyCookie, sessionBinding{ID: cs.ID, Token: cs.Token}); err != nil {
				r.unavailable(ctx, w, "passkey sign-in: seal", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"options": cs.WebAuthnOptions})
			return
		}
		r.cfg.Logger.InfoContext(ctx, "passkey sign-in: no challenge for the account; answering a decoy", "user_id", userID, "error", err)
	}
	if err := r.setBinding(w, passkeyCookie, sessionBinding{Decoy: true}); err != nil {
		r.unavailable(ctx, w, "passkey sign-in: seal", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": r.decoyOptions(email)})
}

// decoyOptions is a request-options document shaped like the issuer's, with
// one credential id that is stable for the address (so asking twice does not
// tell a decoy from a real account) and a fresh challenge.
func (r *SignInRoutes) decoyOptions(email string) json.RawMessage {
	m := hmac.New(sha256.New, r.decoy)
	m.Write([]byte(email))
	challenge := make([]byte, 32)
	_, _ = rand.Read(challenge)
	b, _ := json.Marshal(map[string]any{"publicKey": map[string]any{
		"challenge":        base64.RawURLEncoding.EncodeToString(challenge),
		"timeout":          300000,
		"rpId":             r.rpID,
		"allowCredentials": []any{map[string]any{"type": "public-key", "id": base64.RawURLEncoding.EncodeToString(m.Sum(nil))}},
		"userVerification": "required",
	}})
	return b
}

type credentialRequest struct {
	Credential json.RawMessage `json:"credential"`
	Name       string          `json:"name,omitempty"`
}

// HandlePasskeyFinish checks a passkey assertion and signs in. The origin and
// ceremony type are checked HERE, before the issuer, because the issuer
// accepts whatever origin the client reports.
func (r *SignInRoutes) HandlePasskeyFinish(w http.ResponseWriter, req *http.Request) {
	var body credentialRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	var b sessionBinding
	if !r.readBinding(req, passkeyCookie, &b) || b.Decoy {
		r.clearBinding(w, passkeyCookie)
		writeErr(w, http.StatusUnauthorized, CodePasskeyFailed, msgPasskeyFailed)
		return
	}
	if err := CheckClientData(body.Credential, r.origin, WebAuthnGet); err != nil {
		r.cfg.Logger.WarnContext(ctx, "passkey sign-in refused before the issuer", "error", err)
		writeErr(w, http.StatusUnauthorized, CodePasskeyFailed, msgPasskeyFailed)
		return
	}
	r.clearBinding(w, passkeyCookie)
	session, err := r.cfg.Client.SetSession(ctx, ChallengeSession{ID: b.ID, Token: b.Token}, Answer{WebAuthnAssertion: body.Credential})
	if err != nil {
		r.cfg.Logger.WarnContext(ctx, "passkey sign-in rejected", "error", err)
		writeErr(w, http.StatusUnauthorized, CodePasskeyFailed, msgPasskeyFailed)
		return
	}
	r.complete(w, req, session, func(tokens Tokens) {
		r.clearBinding(w, enrollCookie)
		writeJSON(w, http.StatusOK, r.cfg.SignedIn(w, tokens))
	})
}

// complete finalizes a session, answering the gate's refusals itself.
func (r *SignInRoutes) complete(w http.ResponseWriter, req *http.Request, s Session, ok func(Tokens)) {
	ctx := req.Context()
	tokens, err := r.cfg.Client.CompleteSignIn(ctx, r.cfg.Flow, s)
	switch {
	case err == nil:
		ok(tokens)
	case errors.Is(err, ErrEmailUnverified):
		writeErr(w, http.StatusForbidden, CodeEmailUnverified, msgUnverified)
	default:
		r.unavailable(ctx, w, "sign-in could not be completed", err)
	}
}

// ── Passkey registration ───────────────────────────────────────────────

// enrollingUser is the user a passkey registration is for: the signed-in
// user (from the session token, validated by the issuer), or the user an
// enrolment cookie names (set only after an invitation or verification code
// was proven). Never a request body.
func (r *SignInRoutes) enrollingUser(req *http.Request) (userID string, code *PasskeyRegistrationCode, ok bool) {
	if tok := r.cfg.SessionToken(req); tok != "" {
		if info, err := r.cfg.Client.UserInfo(req.Context(), tok); err == nil {
			return info.Subject, nil, true
		}
	}
	var e enrollBinding
	if r.readBinding(req, enrollCookie, &e) && e.UserID != "" {
		return e.UserID, &PasskeyRegistrationCode{ID: e.CodeID, Code: e.Code}, true
	}
	return "", nil, false
}

// HandlePasskeyRegisterBegin starts registering a passkey for the signed-in
// (or enrolling) user.
func (r *SignInRoutes) HandlePasskeyRegisterBegin(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	userID, code, ok := r.enrollingUser(req)
	if !ok {
		writeErr(w, http.StatusUnauthorized, CodeNotSignedIn, msgNotSignedIn)
		return
	}
	reg, err := r.cfg.Client.RegisterPasskey(ctx, userID, r.rpID, code)
	if err != nil {
		r.unavailable(ctx, w, "passkey registration could not start", err)
		return
	}
	if err := r.setBinding(w, registerCookie, registerBinding{UserID: userID, PasskeyID: reg.PasskeyID}); err != nil {
		r.unavailable(ctx, w, "passkey registration: seal", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": reg.Options})
}

// HandlePasskeyRegisterFinish verifies the browser's attestation.
func (r *SignInRoutes) HandlePasskeyRegisterFinish(w http.ResponseWriter, req *http.Request) {
	var body credentialRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	var b registerBinding
	userID, _, ok := r.enrollingUser(req)
	if !ok || !r.readBinding(req, registerCookie, &b) || b.UserID != userID {
		writeErr(w, http.StatusUnauthorized, CodeNotSignedIn, msgNotSignedIn)
		return
	}
	if err := CheckClientData(body.Credential, r.origin, WebAuthnCreate); err != nil {
		r.cfg.Logger.WarnContext(ctx, "passkey registration refused before the issuer", "error", err)
		writeErr(w, http.StatusBadRequest, CodePasskeyFailed, "That passkey could not be registered.")
		return
	}
	r.clearBinding(w, registerCookie)
	if err := r.cfg.Client.VerifyPasskeyRegistration(ctx, userID, b.PasskeyID, body.Credential, body.Name); err != nil {
		r.cfg.Logger.WarnContext(ctx, "passkey registration rejected", "user_id", userID, "error", err)
		writeErr(w, http.StatusBadRequest, CodePasskeyFailed, "That passkey could not be registered.")
		return
	}
	r.cfg.Logger.InfoContext(ctx, "passkey registered", "user_id", userID)
	writeJSON(w, http.StatusOK, map[string]any{"registered": true})
}

// HandlePasskeys lists the signed-in user's passkeys.
func (r *SignInRoutes) HandlePasskeys(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	tok := r.cfg.SessionToken(req)
	info, err := r.cfg.Client.UserInfo(ctx, tok)
	if tok == "" || err != nil {
		writeErr(w, http.StatusUnauthorized, CodeNotSignedIn, msgNotSignedIn)
		return
	}
	keys, err := r.cfg.Client.ListPasskeys(ctx, info.Subject)
	if err != nil {
		r.unavailable(ctx, w, "list passkeys", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": keys})
}

// ── Magic link ─────────────────────────────────────────────────────────

type emailRequest struct {
	Email string `json:"email"`
}

func (r *SignInRoutes) magicTemplate() string { return r.cfg.AppBase + "/auth/magic?code={{.Code}}" }

func (r *SignInRoutes) verifyTemplate() string {
	return r.cfg.AppBase + "/auth/verify?userID={{.UserID}}&code={{.Code}}"
}

// HandleMagicRequest mails a sign-in link and binds it to THIS browser.
//
// Always 202 with one body, and always a binding cookie of one size: known,
// unknown, unverified, rate-limited and refused addresses are
// indistinguishable. A verified account gets the issuer's one-time-code
// session; an unverified one (or a new one, where sign-up allows the address)
// gets an email-verification code instead, because the issuer refuses the
// one-time-code method for an unverified address. Both links have one shape.
func (r *SignInRoutes) HandleMagicRequest(w http.ResponseWriter, req *http.Request) {
	var body emailRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	email := normalizeEmail(body.Email)
	if email == "" || !strings.Contains(email, "@") {
		writeErr(w, http.StatusBadRequest, "", "Enter your email address.")
		return
	}
	binding := magicBinding{Kind: "n"}
	if !r.magicByIP.reserve(clientIP(req)) || !r.magicByAddress.reserve(email) {
		r.cfg.Logger.WarnContext(ctx, "magic link refused: rate limited")
	} else {
		binding = r.startMagic(ctx, email)
	}
	if err := r.setBinding(w, magicCookie, binding); err != nil {
		r.cfg.Logger.ErrorContext(ctx, "magic link: seal", "error", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": msgMagicSent})
}

func (r *SignInRoutes) startMagic(ctx context.Context, email string) magicBinding {
	c := r.cfg.Client
	user, err := c.FindUserByEmail(ctx, email)
	switch {
	case err == nil && user.EmailVerified:
		return r.magicForVerified(ctx, user.ID)
	case err == nil:
		if err := c.ResendEmailCode(ctx, user.ID, r.magicTemplate()); err != nil {
			r.cfg.Logger.ErrorContext(ctx, "magic link: could not mail a verification code", "user_id", user.ID, "error", err)
			return magicBinding{Kind: "n"}
		}
		return magicBinding{Kind: "v", UserID: user.ID}
	case errors.Is(err, ErrUserNotFound) && r.cfg.Policy.SignupAllows(email):
		id, err := c.CreateUser(ctx, NewUser{Email: email, VerifyURLTemplate: r.magicTemplate()})
		if err != nil {
			r.cfg.Logger.ErrorContext(ctx, "magic link: could not create the account", "error", err)
			return magicBinding{Kind: "n"}
		}
		r.cfg.Logger.InfoContext(ctx, "account created (unverified) for a magic link", "user_id", id)
		return magicBinding{Kind: "v", UserID: id, New: true}
	case errors.Is(err, ErrUserNotFound):
		r.cfg.Logger.InfoContext(ctx, "magic link requested for an address with no single account")
	default:
		r.cfg.Logger.ErrorContext(ctx, "magic link: user lookup failed", "error", err)
	}
	return magicBinding{Kind: "n"}
}

func (r *SignInRoutes) magicForVerified(ctx context.Context, userID string) magicBinding {
	c := r.cfg.Client
	if err := c.EnsureOTPEmail(ctx, userID); err != nil {
		r.cfg.Logger.ErrorContext(ctx, "magic link: could not enable the one-time-code method", "user_id", userID, "error", err)
		return magicBinding{Kind: "n"}
	}
	cs, err := c.CreateChallengeSession(ctx, "", userID, Challenge{OTPEmailURLTemplate: r.magicTemplate()})
	if err != nil {
		r.cfg.Logger.ErrorContext(ctx, "magic link: the issuer refused the code", "user_id", userID, "error", err)
		return magicBinding{Kind: "n"}
	}
	r.cfg.Logger.InfoContext(ctx, "magic link requested", "user_id", userID)
	return magicBinding{Kind: "s", ID: cs.ID, Token: cs.Token}
}

type codeRequest struct {
	Code string `json:"code"`
}

// HandleMagicConsume redeems the code from a magic link (or typed from the
// email) in the browser that asked for it. Without that browser's binding
// cookie it refuses BEFORE touching the issuer, so a link scanner or a
// forwarded link burns nothing.
func (r *SignInRoutes) HandleMagicConsume(w http.ResponseWriter, req *http.Request) {
	var body codeRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	var b magicBinding
	if !r.readBinding(req, magicCookie, &b) {
		writeErr(w, http.StatusBadRequest, CodeMagicOtherBrowser, msgOtherBrowser)
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		writeErr(w, http.StatusBadRequest, CodeInvalidCode, msgInvalidCode)
		return
	}
	key := b.ID + b.UserID
	if b.Kind == "n" {
		writeErr(w, http.StatusUnauthorized, CodeInvalidCode, msgInvalidCode)
		return
	}
	if !r.magicByCode.reserve(key) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, msgTooMany)
		return
	}
	c := r.cfg.Client
	var session Session
	var err error
	switch b.Kind {
	case "s":
		session, err = c.SetSession(ctx, ChallengeSession{ID: b.ID, Token: b.Token}, Answer{OTPEmailCode: code})
	case "v":
		session, err = r.sessionFromVerification(ctx, b.UserID, code, b.New)
	default:
		err = ErrInvalidCode
	}
	if errors.Is(err, ErrInvalidCode) {
		writeErr(w, http.StatusUnauthorized, CodeInvalidCode, msgInvalidCode)
		return
	}
	if err != nil {
		r.unavailable(ctx, w, "magic link could not be redeemed", err)
		return
	}
	r.magicByCode.forget(key)
	r.clearBinding(w, magicCookie)
	r.complete(w, req, session, func(tokens Tokens) { writeJSON(w, http.StatusOK, r.cfg.SignedIn(w, tokens)) })
}

// sessionFromVerification turns a proven email-verification code into a
// session. PROOF FIRST: VerifyEmail must succeed for this user in this
// request. Only then does the broker mint a one-time code with its own
// credential and redeem it at once — the address is now verified, so the
// issuer allows the method, and the person already proved the inbox.
func (r *SignInRoutes) sessionFromVerification(ctx context.Context, userID, code string, isNew bool) (Session, error) {
	c := r.cfg.Client
	if err := c.VerifyEmail(ctx, userID, code); err != nil {
		return Session{}, err
	}
	if isNew {
		r.grantSignupRole(ctx, userID)
	}
	if err := c.EnsureOTPEmail(ctx, userID); err != nil {
		return Session{}, err
	}
	cs, err := c.CreateChallengeSession(ctx, "", userID, Challenge{OTPEmailReturnCode: true})
	if err != nil {
		return Session{}, err
	}
	return c.SetSession(ctx, cs, Answer{OTPEmailCode: cs.OTPEmailCode})
}

func (r *SignInRoutes) grantSignupRole(ctx context.Context, userID string) {
	role := r.cfg.Policy.SignupRole
	if role == "" || r.cfg.ProjectID == "" {
		return
	}
	if err := r.cfg.Client.GrantProjectRole(ctx, userID, r.cfg.ProjectID, role); err != nil {
		r.cfg.Logger.ErrorContext(ctx, "could not grant the sign-up role", "user_id", userID, "role", role, "error", err)
		return
	}
	r.cfg.Logger.InfoContext(ctx, "sign-up role granted", "user_id", userID, "role", role)
}

// ── Social ─────────────────────────────────────────────────────────────

type socialStartRequest struct {
	Provider string `json:"provider"`
	ReturnTo string `json:"returnTo,omitempty"`
}

// HandleSocialStart begins a social sign-in. The intent id goes into a
// binding cookie; the callback requires it, which defeats login CSRF.
func (r *SignInRoutes) HandleSocialStart(w http.ResponseWriter, req *http.Request) {
	var body socialStartRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	social, ok := r.cfg.Policy.SocialByType(strings.TrimSpace(body.Provider))
	if !ok {
		writeErr(w, http.StatusNotFound, CodeUnknownProvider, msgMethod)
		return
	}
	if !r.socialStartByIP.reserve(clientIP(req)) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, msgTooMany)
		return
	}
	authURL, intentID, err := r.cfg.Client.StartIDPIntent(ctx, social.IdPID,
		r.cfg.AppBase+"/auth/social/callback", r.cfg.AppBase+"/auth/sign-in?error=social")
	if err != nil {
		r.unavailable(ctx, w, "social sign-in could not start", err)
		return
	}
	if err := r.setBinding(w, socialCookie, socialBinding{IntentID: intentID, ReturnTo: SafeReturnTo(body.ReturnTo)}); err != nil {
		r.unavailable(ctx, w, "social sign-in: seal", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": authURL})
}

// SafeReturnTo keeps a same-origin relative path and turns anything else —
// an absolute URL, a scheme-relative "//host", a backslash trick — into "/".
func SafeReturnTo(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return "/"
	}
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return p
}

type socialCallbackRequest struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// HandleSocialCallback completes a social sign-in. The frontend's
// /auth/social/callback page posts the id and token the issuer appended.
//
// A provider identity already linked signs in. Otherwise:
//   - an account with the provider's address exists: link ONLY when the
//     provider asserts the address verified AND the account's is verified —
//     an unverified auto-link is an account takeover;
//   - no account: create one where sign-up allows the address (domain mode
//     needs a provider-verified one), else refuse.
func (r *SignInRoutes) HandleSocialCallback(w http.ResponseWriter, req *http.Request) {
	var body socialCallbackRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	var b socialBinding
	if !r.readBinding(req, socialCookie, &b) || b.IntentID == "" || b.IntentID != strings.TrimSpace(body.ID) {
		writeErr(w, http.StatusUnauthorized, CodeSocialFailed, msgSocialFailed)
		return
	}
	r.clearBinding(w, socialCookie)
	c := r.cfg.Client
	res, err := c.RetrieveIDPIntent(ctx, b.IntentID, body.Token)
	if err != nil {
		r.cfg.Logger.WarnContext(ctx, "social sign-in: intent not retrieved", "error", err)
		writeErr(w, http.StatusUnauthorized, CodeSocialFailed, msgSocialFailed)
		return
	}
	social, ok := r.cfg.Policy.SocialByIdPID(res.Link.IdPID)
	if !ok {
		r.cfg.Logger.WarnContext(ctx, "social sign-in: intent for an undeclared provider", "idp_id", res.Link.IdPID)
		writeErr(w, http.StatusUnauthorized, CodeSocialFailed, msgSocialFailed)
		return
	}
	intent := &IDPIntent{ID: b.IntentID, Token: strings.TrimSpace(body.Token)}
	signIn := func(userID string) {
		s, err := c.CreateSession(ctx, Credentials{UserID: userID, IDPIntent: intent})
		if err != nil {
			r.cfg.Logger.WarnContext(ctx, "social sign-in: session refused", "user_id", userID, "error", err)
			writeErr(w, http.StatusUnauthorized, CodeSocialFailed, msgSocialFailed)
			return
		}
		r.complete(w, req, s, func(tokens Tokens) {
			writeJSON(w, http.StatusOK, map[string]any{"identity": r.cfg.SignedIn(w, tokens), "returnTo": b.ReturnTo})
		})
	}
	if b.InviteUserID != "" {
		r.acceptInviteBySocial(w, req, b, res, social, signIn)
		return
	}
	if res.LinkedUserID != "" {
		signIn(res.LinkedUserID)
		return
	}
	email := normalizeEmail(res.Email)
	var existing User
	err = ErrUserNotFound
	if email != "" {
		existing, err = c.FindUserByEmail(ctx, email)
	}
	switch {
	case err == nil:
		if !res.EmailVerified || !existing.EmailVerified {
			r.cfg.Logger.InfoContext(ctx, "social sign-in: not linking an unverified address", "user_id", existing.ID,
				"provider_verified", res.EmailVerified, "account_verified", existing.EmailVerified)
			writeErr(w, http.StatusConflict, CodeSocialLinkRequired, fmt.Sprintf(msgSocialLink, social.Label))
			return
		}
		if err := c.AddIDPLink(ctx, existing.ID, res.Link); err != nil {
			r.unavailable(ctx, w, "social sign-in: link", err)
			return
		}
		r.cfg.Logger.InfoContext(ctx, "social account linked", "user_id", existing.ID, "provider", social.Type)
		signIn(existing.ID)
	case !errors.Is(err, ErrUserNotFound):
		r.unavailable(ctx, w, "social sign-in: user lookup", err)
	case !r.cfg.Policy.SelfSignup() || email == "":
		writeErr(w, http.StatusForbidden, CodeSocialNoAccount, msgSocialNoAcct)
	case !r.cfg.Policy.SignupAllows(email) || (r.cfg.Policy.Signup == SignupDomain && !res.EmailVerified):
		writeErr(w, http.StatusForbidden, CodeSignupNotAllowed, msgSignupDomain)
	default:
		// The provider account is linked at creation only when the provider
		// asserted the address. Otherwise the account is created bare and
		// the link waits, sealed in THIS browser, for the address's code.
		nu := NewUser{Email: email, GivenName: res.GivenName, FamilyName: res.FamilyName,
			EmailVerified: res.EmailVerified, VerifyURLTemplate: r.verifyTemplate()}
		if res.EmailVerified {
			nu.IDPLinks = []IDPLink{res.Link}
		}
		id, err := c.CreateUser(ctx, nu)
		if err != nil {
			r.unavailable(ctx, w, "social sign-in: create account", err)
			return
		}
		r.cfg.Logger.InfoContext(ctx, "account created from a social sign-in", "user_id", id, "provider", social.Type, "verified", res.EmailVerified)
		if !res.EmailVerified {
			if err := r.setBinding(w, linkCookie, pendingLinkBinding{UserID: id, Link: res.Link}); err != nil {
				r.cfg.Logger.ErrorContext(ctx, "social sign-in: the pending link could not be sealed", "user_id", id, "error", err)
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"verify": "sent", "message": msgVerifySent})
			return
		}
		r.grantSignupRole(ctx, id)
		signIn(id)
	}
}

// acceptInviteBySocial finishes a social invitation acceptance. PROOF FIRST:
// the invitation code sealed at the start is checked HERE, in the request
// that links the provider account, for the user the invitation names. A
// provider account already linked to someone else is refused before the
// invitation is spent.
func (r *SignInRoutes) acceptInviteBySocial(w http.ResponseWriter, req *http.Request, b socialBinding, res IDPResult,
	social LoginPolicySocial, signIn func(string)) {
	ctx := req.Context()
	c := r.cfg.Client
	userID := b.InviteUserID
	if res.LinkedUserID != "" && res.LinkedUserID != userID {
		r.cfg.Logger.WarnContext(ctx, "invitation: the provider account belongs to another user", "user_id", userID, "provider", social.Type)
		writeErr(w, http.StatusConflict, CodeSocialTaken, fmt.Sprintf(msgSocialTaken, social.Label))
		return
	}
	if !r.codeByUser.reserve(userID) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, msgTooMany)
		return
	}
	if err := c.VerifyInviteCode(ctx, userID, b.InviteCode); err != nil {
		if errors.Is(err, ErrInvalidCode) {
			r.cfg.Logger.WarnContext(ctx, "invitation rejected", "user_id", userID, "error", err)
			writeErr(w, http.StatusUnauthorized, CodeInvalidCode, msgInvalidCode)
			return
		}
		r.codeByUser.refund(userID)
		r.unavailable(ctx, w, "invitation could not be checked", err)
		return
	}
	r.codeByUser.forget(userID)
	if res.LinkedUserID == "" {
		if err := c.AddIDPLink(ctx, userID, res.Link); err != nil {
			r.unavailable(ctx, w, "invitation: link the provider account", err)
			return
		}
	}
	r.cfg.Logger.InfoContext(ctx, "invitation accepted", "user_id", userID, "method", InviteMethodSocial, "provider", social.Type)
	signIn(userID)
}

// ── Invitations ────────────────────────────────────────────────────────

type inviteRequest struct {
	UserID   string `json:"userID"`
	Code     string `json:"code"`
	OrgID    string `json:"orgID,omitempty"`
	Method   string `json:"method"`
	Password string `json:"password,omitempty"`
	// Provider is the social button's type, for method "social".
	Provider string `json:"provider,omitempty"`
}

// HandleInviteAccept redeems an invitation and sets the invitee up with the
// method they chose. NOTHING after the invitation check runs unless it
// succeeded for this user in this request.
func (r *SignInRoutes) HandleInviteAccept(w http.ResponseWriter, req *http.Request) {
	var body inviteRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	userID, code := strings.TrimSpace(body.UserID), strings.TrimSpace(body.Code)
	if userID == "" || code == "" {
		writeErr(w, http.StatusBadRequest, CodeInvalidCode, msgInvalidCode)
		return
	}
	if body.Method == InviteMethodSocial {
		r.startInviteSocial(w, req, userID, code, body.Provider)
		return
	}
	if !r.cfg.Policy.Offers(body.Method) {
		writeErr(w, http.StatusBadRequest, CodeMethodNotOffered, msgMethod)
		return
	}
	if body.Method == MethodPassword {
		if body.Password == "" {
			writeErr(w, http.StatusBadRequest, CodePasswordRequired, "Choose a password.")
			return
		}
		// Checked BEFORE the single-use invitation is spent, so a weak
		// password does not strand the invitee.
		if r.rejectWeak(ctx, w, body.Password) {
			return
		}
	}
	if !r.codeByUser.reserve(userID) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, msgTooMany)
		return
	}
	c := r.cfg.Client
	if err := c.VerifyInviteCode(ctx, userID, code); err != nil {
		if errors.Is(err, ErrInvalidCode) {
			r.cfg.Logger.WarnContext(ctx, "invitation rejected", "user_id", userID, "error", err)
			writeErr(w, http.StatusUnauthorized, CodeInvalidCode, msgInvalidCode)
			return
		}
		r.codeByUser.refund(userID)
		r.unavailable(ctx, w, "invitation could not be checked", err)
		return
	}
	r.codeByUser.forget(userID)
	r.cfg.Logger.InfoContext(ctx, "invitation accepted", "user_id", userID, "method", body.Method)

	switch body.Method {
	case MethodPassword:
		r.setPasswordAndSignIn(w, req, userID, body.Password)
	case MethodPasskey:
		r.enroll(w, req, userID)
	case MethodMagicLink:
		b := r.magicForVerified(ctx, userID)
		if b.Kind != "s" {
			r.unavailable(ctx, w, "invitation: magic link", fmt.Errorf("the issuer refused the one-time code"))
			return
		}
		if err := r.setBinding(w, magicCookie, b); err != nil {
			r.unavailable(ctx, w, "invitation: seal", err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"message": "A sign-in link is on its way to your email."})
	}
}

// startInviteSocial begins accepting an invitation by connecting a social
// account — the only way in for an invite-only policy that offers social
// sign-in alone. The invitation is NOT checked here: its code is sealed into
// the social binding cookie and checked at the callback, in the request that
// links the account (acceptInviteBySocial).
func (r *SignInRoutes) startInviteSocial(w http.ResponseWriter, req *http.Request, userID, code, provider string) {
	ctx := req.Context()
	social, ok := r.cfg.Policy.SocialByType(strings.TrimSpace(provider))
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeMethodNotOffered, msgMethod)
		return
	}
	if !r.socialStartByIP.reserve(clientIP(req)) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, msgTooMany)
		return
	}
	back := url.Values{"userID": {userID}, "code": {code}, "error": {"social"}}
	authURL, intentID, err := r.cfg.Client.StartIDPIntent(ctx, social.IdPID,
		r.cfg.AppBase+"/auth/social/callback", r.cfg.AppBase+"/auth/invite?"+back.Encode())
	if err != nil {
		r.unavailable(ctx, w, "invitation: social sign-in could not start", err)
		return
	}
	if err := r.setBinding(w, socialCookie, socialBinding{IntentID: intentID, ReturnTo: "/", InviteUserID: userID, InviteCode: code}); err != nil {
		r.unavailable(ctx, w, "invitation: seal", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": authURL})
}

// setPasswordAndSignIn sets an invitee's first password: the broker mints a
// reset code with its own credential and redeems it at once — allowed only
// because the invitation was proven in this request — then signs in.
func (r *SignInRoutes) setPasswordAndSignIn(w http.ResponseWriter, req *http.Request, userID, password string) {
	ctx := req.Context()
	c := r.cfg.Client
	reset, err := c.MintPasswordResetCode(ctx, userID)
	if err == nil {
		err = c.ResetPassword(ctx, PasswordReset{UserID: userID, Code: reset, NewPassword: password})
	}
	var policyErr *PasswordPolicyError
	if errors.As(err, &policyErr) {
		r.rejectPassword(ctx, w, password, policyErr)
		return
	}
	if err != nil {
		r.unavailable(ctx, w, "invitation: set password", err)
		return
	}
	user, err := c.FindUserByID(ctx, userID)
	if err != nil {
		r.unavailable(ctx, w, "invitation: read account", err)
		return
	}
	tokens, err := c.SignIn(ctx, r.cfg.Flow, Credentials{LoginName: user.LoginName, Password: password})
	if errors.Is(err, ErrEmailUnverified) {
		writeErr(w, http.StatusForbidden, CodeEmailUnverified, msgUnverified)
		return
	}
	if err != nil {
		r.unavailable(ctx, w, "invitation: sign in", err)
		return
	}
	writeJSON(w, http.StatusOK, r.cfg.SignedIn(w, tokens))
}

// enroll lets a user who just proved an emailed code register a passkey: a
// registration code is minted and sealed into an enrolment cookie.
func (r *SignInRoutes) enroll(w http.ResponseWriter, req *http.Request, userID string) {
	ctx := req.Context()
	code, err := r.cfg.Client.MintPasskeyRegistrationCode(ctx, userID)
	if err != nil {
		r.unavailable(ctx, w, "passkey enrolment", err)
		return
	}
	if err := r.setBinding(w, enrollCookie, enrollBinding{UserID: userID, CodeID: code.ID, Code: code.Code}); err != nil {
		r.unavailable(ctx, w, "passkey enrolment: seal", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enroll": true})
}

// ── Verification ───────────────────────────────────────────────────────

type verifyRequest struct {
	UserID string `json:"userID"`
	Code   string `json:"code"`
}

// HandleVerify redeems an email-verification code. A self-signed-up account
// gets its signup_role now, never before. An account with no password, where
// passkeys are offered, may enrol one next (enroll: true).
func (r *SignInRoutes) HandleVerify(w http.ResponseWriter, req *http.Request) {
	var body verifyRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	userID, code := strings.TrimSpace(body.UserID), strings.TrimSpace(body.Code)
	if userID == "" || code == "" {
		writeErr(w, http.StatusBadRequest, CodeInvalidCode, msgInvalidCode)
		return
	}
	if !r.codeByUser.reserve(userID) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, msgTooMany)
		return
	}
	c := r.cfg.Client
	if err := c.VerifyEmail(ctx, userID, code); err != nil {
		if errors.Is(err, ErrInvalidCode) {
			writeErr(w, http.StatusUnauthorized, CodeInvalidCode, msgInvalidCode)
			return
		}
		r.codeByUser.refund(userID)
		r.unavailable(ctx, w, "verification could not be checked", err)
		return
	}
	r.codeByUser.forget(userID)
	r.cfg.Logger.InfoContext(ctx, "email address verified", "user_id", userID)
	r.attachPendingLink(w, req, userID)
	if r.cfg.Policy.SelfSignup() {
		if u, err := c.FindUserByID(ctx, userID); err == nil && r.cfg.Policy.SignupAllows(u.Email) {
			r.grantSignupRole(ctx, userID)
		}
	}
	if r.cfg.Policy.Offers(MethodPasskey) {
		if has, err := c.HasPassword(ctx, userID); err == nil && !has {
			code, err := c.MintPasskeyRegistrationCode(ctx, userID)
			if err == nil {
				err = r.setBinding(w, enrollCookie, enrollBinding{UserID: userID, CodeID: code.ID, Code: code.Code})
			}
			if err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"verified": true, "enroll": true})
				return
			}
			r.cfg.Logger.ErrorContext(ctx, "verification: passkey enrolment unavailable", "user_id", userID, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "enroll": false})
}

// attachPendingLink links the provider account a social sign-up left waiting,
// now that THIS browser proved the address of the account it created. A
// verification opened in any other browser links nothing.
func (r *SignInRoutes) attachPendingLink(w http.ResponseWriter, req *http.Request, userID string) {
	ctx := req.Context()
	var pl pendingLinkBinding
	if !r.readBinding(req, linkCookie, &pl) || pl.UserID != userID {
		return
	}
	r.clearBinding(w, linkCookie)
	if err := r.cfg.Client.AddIDPLink(ctx, userID, pl.Link); err != nil {
		r.cfg.Logger.ErrorContext(ctx, "verification: the pending social link could not be added", "user_id", userID, "error", err)
		return
	}
	r.cfg.Logger.InfoContext(ctx, "social account linked after verification", "user_id", userID)
}

// HandleVerifyResend mails a fresh verification link. Always 202.
func (r *SignInRoutes) HandleVerifyResend(w http.ResponseWriter, req *http.Request) {
	var body emailRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	email := normalizeEmail(body.Email)
	if email != "" && r.verifyResendIP.reserve(clientIP(req)) && r.verifyResendBy.reserve(email) {
		if u, err := r.cfg.Client.FindUserByEmail(ctx, email); err == nil && !u.EmailVerified {
			if err := r.cfg.Client.ResendEmailCode(ctx, u.ID, r.verifyTemplate()); err != nil {
				r.cfg.Logger.ErrorContext(ctx, "verification resend failed", "user_id", u.ID, "error", err)
			}
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": msgVerifyResent})
}

// ── Sign-up ────────────────────────────────────────────────────────────

type registerRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

// HandleRegister creates an account under an open or domain sign-up. It
// answers 202 with NO session, identically whether or not the address
// already has an account: the verification mail is what proves the address,
// and the session comes after it.
//
// Where no mail can arrive, the policy decides (RegisterServed): sign-up is
// refused unless it is open, with a password, and the policy does not
// require a verified address — then registerWithoutMail signs in at once.
func (r *SignInRoutes) HandleRegister(w http.ResponseWriter, req *http.Request) {
	var body registerRequest
	if !decode(w, req, &body) {
		return
	}
	ctx := req.Context()
	email := normalizeEmail(body.Email)
	if email == "" || !strings.Contains(email, "@") {
		writeErr(w, http.StatusBadRequest, "", "Enter your email address.")
		return
	}
	p := r.cfg.Policy
	if !p.RegisterServed() {
		writeErr(w, http.StatusForbidden, CodeSignupNotAllowed, "Accounts cannot be created here.")
		return
	}
	if !p.SignupAllows(email) {
		writeErr(w, http.StatusForbidden, CodeSignupNotAllowed, msgSignupDomain)
		return
	}
	// Without mail, a password is the only credential a new account can
	// have: passkey enrolment and magic links both follow a mailed code.
	passwordless := p.MailDelivers() && (p.Offers(MethodPasskey) || p.Offers(MethodMagicLink))
	switch {
	case body.Password == "" && !passwordless:
		writeErr(w, http.StatusBadRequest, CodePasswordRequired, "Choose a password.")
		return
	case body.Password != "" && !p.Offers(MethodPassword):
		writeErr(w, http.StatusBadRequest, CodeMethodNotOffered, msgMethod)
		return
	}
	// The password is judged BEFORE the address is looked up, so a weak
	// password is refused for every address alike.
	if body.Password != "" && r.rejectWeak(ctx, w, body.Password) {
		return
	}
	if !r.registerByIP.reserve(clientIP(req)) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, msgTooMany)
		return
	}
	if !p.MailDelivers() {
		r.registerWithoutMail(w, req, email, body)
		return
	}
	c := r.cfg.Client
	user, err := c.FindUserByEmail(ctx, email)
	switch {
	case err == nil:
		if !user.EmailVerified {
			if err := c.ResendEmailCode(ctx, user.ID, r.verifyTemplate()); err != nil {
				r.cfg.Logger.ErrorContext(ctx, "sign-up: resend for an existing account failed", "user_id", user.ID, "error", err)
			}
		}
		r.cfg.Logger.InfoContext(ctx, "sign-up for an address that already has an account", "user_id", user.ID)
	case errors.Is(err, ErrUserNotFound):
		id, err := c.CreateUser(ctx, NewUser{Email: email, Password: body.Password, GivenName: body.GivenName,
			FamilyName: body.FamilyName, VerifyURLTemplate: r.verifyTemplate()})
		var policyErr *PasswordPolicyError
		if errors.As(err, &policyErr) {
			r.rejectPassword(ctx, w, body.Password, policyErr)
			return
		}
		if err != nil {
			r.unavailable(ctx, w, "sign-up: create account", err)
			return
		}
		r.cfg.Logger.InfoContext(ctx, "account created (unverified)", "user_id", id)
	default:
		r.unavailable(ctx, w, "sign-up: user lookup", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"verify": "sent", "message": msgVerifySent})
}

// registerWithoutMail is the sign-up of a policy whose mail reaches nobody
// and which therefore does not require a verified address (mail "none"):
// open sign-up, with a password. No mail means no "check your email" answer
// can be uniform, so this is the legacy broker's sign-up: the account is
// created and signed in at once, and an address that already has an account
// is told to sign in. The address stays unverified, as the policy accepts.
func (r *SignInRoutes) registerWithoutMail(w http.ResponseWriter, req *http.Request, email string, body registerRequest) {
	ctx := req.Context()
	c := r.cfg.Client
	if _, err := c.FindUserByEmail(ctx, email); err == nil {
		writeErr(w, http.StatusConflict, CodeAccountExists, msgAccountExists)
		return
	} else if !errors.Is(err, ErrUserNotFound) {
		r.unavailable(ctx, w, "sign-up: user lookup", err)
		return
	}
	id, err := c.CreateUser(ctx, NewUser{Email: email, Password: body.Password, GivenName: body.GivenName,
		FamilyName: body.FamilyName})
	var policyErr *PasswordPolicyError
	if errors.As(err, &policyErr) {
		r.rejectPassword(ctx, w, body.Password, policyErr)
		return
	}
	if err != nil {
		r.unavailable(ctx, w, "sign-up: create account", err)
		return
	}
	r.cfg.Logger.InfoContext(ctx, "account created (no mail delivery; signed in unverified)", "user_id", id)
	r.grantSignupRole(ctx, id)
	tokens, err := c.SignIn(ctx, r.cfg.Flow, Credentials{LoginName: email, Password: body.Password})
	if err != nil {
		r.unavailable(ctx, w, "sign-up: sign in", err)
		return
	}
	writeJSON(w, http.StatusOK, r.cfg.SignedIn(w, tokens))
}

// rejectWeak answers (and reports true) when password breaks the known
// policy. No policy known: false, and the issuer judges it.
func (r *SignInRoutes) rejectWeak(ctx context.Context, w http.ResponseWriter, password string) bool {
	if r.cfg.PasswordPolicy == nil {
		return false
	}
	policy, err := r.cfg.PasswordPolicy(ctx)
	if err != nil {
		return false
	}
	unmet := policy.Unmet(password)
	if len(unmet) == 0 {
		return false
	}
	r.rejectPassword(ctx, w, password, &PasswordPolicyError{Rule: unmet[0], Reason: policy.Requirement(unmet[0])})
	return true
}

func (r *SignInRoutes) rejectPassword(ctx context.Context, w http.ResponseWriter, password string, err *PasswordPolicyError) {
	if r.cfg.RejectPassword != nil {
		r.cfg.RejectPassword(ctx, w, password, err)
		return
	}
	writeErr(w, http.StatusBadRequest, "password_policy", "That password was not accepted: "+err.Reason+".")
}

// ── Helpers ────────────────────────────────────────────────────────────

func (r *SignInRoutes) unavailable(ctx context.Context, w http.ResponseWriter, what string, err error) {
	r.cfg.Logger.ErrorContext(ctx, what, "error", err)
	writeErr(w, http.StatusBadGateway, "", msgUnavailable)
}

func decode(w http.ResponseWriter, req *http.Request, out any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "", "malformed request")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	body := map[string]string{"error": message}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// clientIP is the connection's peer — deliberately not X-Forwarded-For, a
// header the caller sets (see the broker's own clientIP).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter is a fixed-window attempt budget per key, in memory: the same
// trade-off as the broker's attemptLimiter (N replicas, N times the budget).
type limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	now     func() time.Time
	entries map[string]*limiterEntry
}

type limiterEntry struct {
	used  int
	since time.Time
}

const limiterMaxKeys = 10_000

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, now: time.Now, entries: map[string]*limiterEntry{}}
}

func (l *limiter) reserve(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil || now.Sub(e.since) > l.window {
		if len(l.entries) >= limiterMaxKeys {
			for k, v := range l.entries {
				if now.Sub(v.since) > l.window {
					delete(l.entries, k)
				}
			}
			if len(l.entries) >= limiterMaxKeys {
				return false
			}
		}
		e = &limiterEntry{since: now}
		l.entries[key] = e
	}
	if e.used >= l.max {
		return false
	}
	e.used++
	return true
}

func (l *limiter) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[key]; e != nil && e.used > 0 {
		e.used--
	}
}

func (l *limiter) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}
