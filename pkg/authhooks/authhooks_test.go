package authhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recorder serves a hook through Handler with a logger the test reads.
type recorder struct {
	t   *testing.T
	buf bytes.Buffer
	h   http.Handler
}

func newRecorder(t *testing.T, src Source, opts Options) *recorder {
	r := &recorder{t: t}
	opts.Logger = slog.New(slog.NewJSONHandler(&r.buf, nil))
	r.h = Handler(src, opts)
	return r
}

func (r *recorder) post(body string, header map[string]string) int {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/_forge/auth/hooks/x", strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w.Code
}

// events are the product records logged so far.
func (r *recorder) events() []map[string]any {
	r.t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(r.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			r.t.Fatalf("bad log line %q: %v", line, err)
		}
		if name, _ := m["event.name"].(string); strings.HasPrefix(name, "product.") {
			out = append(out, m)
		}
	}
	return out
}

const secret = "s3cret-hook-token"

var bearer = map[string]string{"Authorization": "Bearer " + secret}

func TestHandlerRefusesWhatItCannotVerify(t *testing.T) {
	r := newRecorder(t, supabaseSource{}, Options{Secret: secret})
	body := `{"v":1,"event":"signed_up","user_id":"u1","provider":"email","password":true,"verified":true,"dedupe":"u1"}`
	cases := map[string]map[string]string{
		"no header":    nil,
		"wrong secret": {"Authorization": "Bearer nope"},
		"not bearer":   {"Authorization": secret},
	}
	for name, h := range cases {
		if code := r.post(body, h); code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
	big := `{"pad":"` + strings.Repeat("x", MaxBodyBytes) + `"}`
	if code := r.post(big, bearer); code != http.StatusUnauthorized {
		t.Errorf("oversized body: status %d, want 401", code)
	}
	if ev := r.events(); len(ev) != 0 {
		t.Errorf("recorded %v from unverified deliveries", ev)
	}
	// An empty secret verifies nothing, not everything.
	open := newRecorder(t, supabaseSource{}, Options{})
	if code := open.post(body, map[string]string{"Authorization": "Bearer "}); code != http.StatusUnauthorized {
		t.Errorf("empty secret: status %d, want 401", code)
	}
}

func TestHandlerOnlyAcceptsPost(t *testing.T) {
	h := Supabase(Options{Secret: secret})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET: status %d Allow %q, want 405 POST", w.Code, w.Header().Get("Allow"))
	}
}

func TestSupabaseEventsAreRecordedOnceEach(t *testing.T) {
	r := newRecorder(t, supabaseSource{}, Options{Secret: secret})
	signup := `{"v":1,"event":"signed_up","user_id":"6f1c0b9e-0000-4000-8000-000000000001","at":"2026-11-11T10:00:00Z","provider":"email","password":false,"verified":true,"dedupe":"6f1c0b9e-0000-4000-8000-000000000001"}`
	signin := `{"v":1,"event":"signed_in","user_id":"6f1c0b9e-0000-4000-8000-000000000001","at":"2026-11-11T10:00:01Z","provider":"google","dedupe":"sess-1"}`
	for _, b := range []string{signup, signin, signup, signin} { // retried deliveries
		if code := r.post(b, bearer); code != http.StatusNoContent {
			t.Fatalf("status %d, want 204", code)
		}
	}
	ev := r.events()
	if len(ev) != 2 {
		t.Fatalf("recorded %d events, want 2 (retries deduped): %v", len(ev), ev)
	}
	up, in := ev[0], ev[1]
	want := map[string]any{
		"event.name": "product.auth_signed_up", "user.id": "6f1c0b9e-0000-4000-8000-000000000001",
		"product.provider": "supabase", "product.method": "magic_link", "product.verified": "true",
	}
	for k, v := range want {
		if up[k] != v {
			t.Errorf("signed_up %s = %v, want %v (%v)", k, up[k], v, up)
		}
	}
	want = map[string]any{
		"event.name": "product.auth_signed_in", "product.kind": "credential",
		"product.method": "social", "product.social_provider": "google", "product.provider": "supabase",
	}
	for k, v := range want {
		if in[k] != v {
			t.Errorf("signed_in %s = %v, want %v (%v)", k, in[k], v, in)
		}
	}
}

func TestHandlerAcknowledgesWhatItDoesNotMap(t *testing.T) {
	r := newRecorder(t, supabaseSource{}, Options{Secret: secret})
	for _, b := range []string{
		`{"v":1,"event":"token_refreshed","user_id":"u1"}`, // an event it does not map
		`{"v":2,"event":"signed_up","user_id":"u1"}`,       // a newer SQL than this forge
		`not json`, // garbage, but verified
		`{"v":1,"event":"signed_up","user_id":"a@b.c"}`, // an email is refused by TrackAuth
	} {
		if code := r.post(b, bearer); code != http.StatusNoContent {
			t.Errorf("%s: status %d, want 204 so the provider never retries", b, code)
		}
	}
	for _, e := range r.events() {
		t.Errorf("recorded %v", e)
	}
}

func TestHandlerRateLimits(t *testing.T) {
	r := newRecorder(t, supabaseSource{}, Options{Secret: secret, RateLimit: 1, Burst: 1})
	body := `{"v":1,"event":"signed_in","user_id":"u1","dedupe":"s1"}`
	if code := r.post(body, bearer); code != http.StatusNoContent {
		t.Fatalf("first: %d", code)
	}
	if code := r.post(body, bearer); code != http.StatusTooManyRequests {
		t.Errorf("second within the burst: status %d, want 429", code)
	}
}

func TestDedupeIsBoundedAndExpires(t *testing.T) {
	d := newDedupe(2, time.Hour)
	t0 := time.Unix(0, 0)
	if !d.first("a", t0) || d.first("a", t0) {
		t.Fatal("a key is recorded once")
	}
	d.first("b", t0)
	d.first("c", t0) // evicts a, the oldest
	if !d.first("a", t0) {
		t.Error("past the bound, the oldest key is forgotten")
	}
	if d.first("c", t0.Add(59*time.Minute)) {
		t.Error("within the TTL a key is still remembered")
	}
	if !d.first("c", t0.Add(61*time.Minute)) {
		t.Error("past the TTL a key is forgotten")
	}
}

func TestSupabaseMethod(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		provider string
		password *bool
		method   string
		social   string
	}{
		{"email", &yes, "password", ""},
		{"email", &no, "magic_link", ""},
		{"email", nil, "", ""},
		{"google", nil, "social", "google"},
		{"azure", nil, "social", "microsoft"},
		{"discord", nil, "social", "other"},
		{"sso:abc", nil, "sso", ""},
		{"phone", nil, "other", ""},
		{"", nil, "", ""},
	}
	for _, c := range cases {
		m, s := supabaseMethod(c.provider, c.password)
		if string(m) != c.method || s != c.social {
			t.Errorf("supabaseMethod(%q) = %q, %q; want %q, %q", c.provider, m, s, c.method, c.social)
		}
	}
}

// clerkSecret is a Standard Webhooks secret: whsec_ + base64 key.
var clerkSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("clerk-signing-key-0123456789"))

func clerkHeaders(t *testing.T, body string, at time.Time) map[string]string {
	t.Helper()
	key, err := standardWebhooksKey(clerkSecret)
	if err != nil {
		t.Fatal(err)
	}
	id, ts := "msg_2abc", strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "." + body))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return map[string]string{"svix-id": id, "svix-timestamp": ts, "svix-signature": "v1,bm9wZQ== v1," + sig}
}

func TestClerkVerifiesTheSvixSignature(t *testing.T) {
	r := newRecorder(t, clerkSource{now: time.Now}, Options{Secret: clerkSecret})
	body := `{"type":"session.created","data":{"id":"sess_1","user_id":"user_1","created_at":1762855200000}}`
	if code := r.post(body, clerkHeaders(t, body, time.Now())); code != http.StatusNoContent {
		t.Fatalf("signed delivery: status %d, want 204", code)
	}
	if code := r.post(body+" ", clerkHeaders(t, body, time.Now())); code != http.StatusUnauthorized {
		t.Errorf("tampered body: status %d, want 401", code)
	}
	if code := r.post(body, clerkHeaders(t, body, time.Now().Add(-10*time.Minute))); code != http.StatusUnauthorized {
		t.Errorf("stale timestamp: status %d, want 401", code)
	}
	if ev := r.events(); len(ev) != 1 || ev[0]["user.id"] != "user_1" || ev[0]["product.provider"] != "clerk" {
		t.Errorf("events = %v, want one sign-in for user_1", ev)
	}
}

func TestClerkEvents(t *testing.T) {
	src := clerkSource{now: time.Now}
	created := `{"type":"user.created","data":{"id":"user_9","created_at":1762855200000,"password_enabled":false,
		"primary_email_address_id":"idn_1",
		"email_addresses":[{"id":"idn_1","email_address":"x@example.com","verification":{"status":"verified","strategy":"from_oauth_google"}}],
		"external_accounts":[{"provider":"oauth_google"}]}}`
	ev, err := src.Events(nil, []byte(created))
	if err != nil || len(ev) != 1 {
		t.Fatalf("user.created: %v %v", ev, err)
	}
	e := ev[0]
	if e.Kind != "auth_signed_up" || e.UserID != "user_9" || e.Method != "social" || e.Social != "google" || e.Verified == nil || !*e.Verified {
		t.Errorf("user.created = %+v", e)
	}
	ended := `{"type":"session.ended","data":{"id":"sess_2","user_id":"user_9","updated_at":1762855300000}}`
	if ev, _ := src.Events(nil, []byte(ended)); len(ev) != 1 || ev[0].Kind != "auth_signed_out" || ev[0].DedupeKey != "sess_2" {
		t.Errorf("session.ended = %+v", ev)
	}
	if ev, err := src.Events(nil, []byte(`{"type":"email.created","data":{}}`)); ev != nil || err != nil {
		t.Errorf("an unmapped type = %v, %v; want nil, nil", ev, err)
	}
}

func TestAuth0ReadsEveryContentFormat(t *testing.T) {
	src := auth0Source{}
	one := `{"log_id":"9001","data":{"date":"2026-11-11T10:00:00.000Z","type":"ss","user_id":"auth0|abc","strategy":"auth0","strategy_type":"database"}}`
	two := `{"log_id":"9002","data":{"date":"2026-11-11T10:00:01.000Z","type":"s","user_id":"google-oauth2|123","strategy":"google-oauth2","strategy_type":"social"}}`
	three := `{"log_id":"9003","data":{"type":"fp","user_id":"auth0|abc"}}`
	four := `{"log_id":"9004","data":{"type":"slo","user_id":"auth0|abc"}}`
	formats := map[string]string{
		"array":  "[" + strings.Join([]string{one, two, three, four}, ",") + "]",
		"lines":  strings.Join([]string{one, two, three, four}, "\n"),
		"object": one,
	}
	for name, body := range formats {
		ev, err := src.Events(nil, []byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "object" {
			if len(ev) != 1 || ev[0].Kind != "auth_signed_up" || ev[0].Method != "password" {
				t.Errorf("object = %+v", ev)
			}
			continue
		}
		if len(ev) != 3 {
			t.Fatalf("%s: %d events, want 3 (fp ignored): %+v", name, len(ev), ev)
		}
		if ev[1].Kind != "auth_signed_in" || ev[1].Method != "social" || ev[1].Social != "google" || ev[1].DedupeKey != "9002" {
			t.Errorf("%s: login = %+v", name, ev[1])
		}
		if ev[2].Kind != "auth_signed_out" || ev[2].Method != "" {
			t.Errorf("%s: logout = %+v", name, ev[2])
		}
	}
}

func TestAuth0Method(t *testing.T) {
	cases := [][4]string{
		{"enterprise", "samlp", "sso", ""},
		{"passwordless", "email", "magic_link", ""},
		{"", "windowslive", "social", "microsoft"},
		{"", "oidc", "sso", ""},
		{"", "facebook", "social", "other"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		m, s := auth0Method(c[0], c[1])
		if string(m) != c[2] || s != c[3] {
			t.Errorf("auth0Method(%q, %q) = %q, %q; want %q, %q", c[0], c[1], m, s, c[2], c[3])
		}
	}
}

func TestPathAndNames(t *testing.T) {
	if Path("supabase") != "/_forge/auth/hooks/supabase" {
		t.Errorf("Path = %q", Path("supabase"))
	}
	for name, src := range map[string]Source{"supabase": supabaseSource{}, "clerk": clerkSource{}, "auth0": auth0Source{}} {
		if src.Name() != name {
			t.Errorf("%T.Name() = %q, want %q: the provider each endpoint self-reports", src, src.Name(), name)
		}
	}
}

func TestProviderConstructorsVerifyWithOptionsSecret(t *testing.T) {
	body := `{"v":1,"event":"signed_in","user_id":"u-ctor","dedupe":"s-ctor"}`
	h := Supabase(Options{Secret: secret, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	for auth, want := range map[string]int{"Bearer " + secret: http.StatusNoContent, "Bearer other": http.StatusUnauthorized} {
		req := httptest.NewRequest(http.MethodPost, Path("supabase"), strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("%s: status %d, want %d", auth, w.Code, want)
		}
	}
	if Clerk(Options{Secret: clerkSecret}) == nil || Auth0(Options{Secret: secret}) == nil {
		t.Error("nil handler")
	}
}

func TestAllowIsTheAppsOwnRule(t *testing.T) {
	var asked []string
	r := newRecorder(t, supabaseSource{}, Options{Secret: secret, Allow: func(_ context.Context, ev Event) bool {
		asked = append(asked, ev.UserID)
		return ev.UserID != "opted-out"
	}})
	for _, u := range []string{"opted-in", "opted-out"} {
		body := `{"v":1,"event":"signed_in","user_id":"` + u + `","dedupe":"s-` + u + `"}`
		if code := r.post(body, bearer); code != http.StatusNoContent {
			t.Fatalf("%s: status %d, want 204 either way", u, code)
		}
	}
	ev := r.events()
	if len(ev) != 1 || ev[0]["user.id"] != "opted-in" {
		t.Errorf("events = %v, want only opted-in's", ev)
	}
	if len(asked) != 2 {
		t.Errorf("Allow asked about %v, want both users", asked)
	}
}
