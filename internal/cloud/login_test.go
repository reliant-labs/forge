package cloud

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestBrowserLogin_RoundTripsTheCallback drives the whole interactive
// flow with a fake "browser": OpenURL is handed the authorize URL and,
// instead of rendering it, extracts the redirect_uri and state and hits
// the callback the way a real browser would after the user approves.
//
// This exercises the listener, the state check and the credential
// hand-back without opening anything.
func TestBrowserLogin_RoundTripsTheCallback(t *testing.T) {
	ep, err := ResolveEndpoint("prod", &Declaration{Endpoint: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	login := BrowserLogin{
		Endpoint: ep,
		Timeout:  10 * time.Second,
		Out:      io.Discard,
		OpenURL: func(authorizeURL string) error {
			redirect, state := redirectAndState(t, authorizeURL)
			go func() {
				resp, err := http.Get(redirect + "?state=" + state + "&token=browser-issued&account=dev@example.com")
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			return nil
		},
	}

	stored, err := login.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stored.Token != "browser-issued" {
		t.Errorf("token from callback: got %q", stored.Token)
	}
	if stored.Account != "dev@example.com" {
		t.Errorf("account from callback: got %q", stored.Account)
	}
	if stored.Endpoint != ep.URL {
		t.Errorf("stored login should record which endpoint it is for; got %q", stored.Endpoint)
	}
}

// TestBrowserLogin_RejectsStateMismatch: without the state check, any
// local process could push a credential of its choosing into the
// listener and forge would store it.
func TestBrowserLogin_RejectsStateMismatch(t *testing.T) {
	ep, _ := ResolveEndpoint("prod", &Declaration{Endpoint: "https://api.example.com"})

	login := BrowserLogin{
		Endpoint: ep,
		Timeout:  10 * time.Second,
		Out:      io.Discard,
		OpenURL: func(authorizeURL string) error {
			redirect, _ := redirectAndState(t, authorizeURL)
			go func() {
				resp, err := http.Get(redirect + "?state=attacker&token=injected")
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			return nil
		},
	}

	_, err := login.Run(context.Background())
	if err == nil {
		t.Fatal("a callback with the wrong state must be rejected")
	}
	if !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("want a state-mismatch error; got %v", err)
	}
}

// TestBrowserLogin_TimesOutWithTheCITip — a login that hangs forever
// reads the same as one about to succeed, and the way out for a
// headless machine is --token.
func TestBrowserLogin_TimesOutWithTheCITip(t *testing.T) {
	ep, _ := ResolveEndpoint("prod", &Declaration{Endpoint: "https://api.example.com"})

	login := BrowserLogin{
		Endpoint: ep,
		Timeout:  50 * time.Millisecond,
		Out:      io.Discard,
		OpenURL:  func(string) error { return nil }, // nobody ever completes the flow
	}

	_, err := login.Run(context.Background())
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), "--token") {
		t.Errorf("the timeout should point at the non-interactive path; got %v", err)
	}
}

// redirectAndState pulls the loopback redirect URI and the state nonce
// back out of the authorize URL, the way a real IdP would.
func redirectAndState(t *testing.T, authorizeURL string) (redirect, state string) {
	t.Helper()
	idx := strings.Index(authorizeURL, "?")
	if idx < 0 {
		t.Fatalf("authorize URL has no query: %s", authorizeURL)
	}
	values, err := url.ParseQuery(authorizeURL[idx+1:])
	if err != nil {
		t.Fatalf("parse authorize URL query: %v", err)
	}
	redirect = values.Get("redirect_uri")
	state = values.Get("state")
	if redirect == "" || state == "" {
		t.Fatalf("authorize URL missing redirect_uri/state: %s", authorizeURL)
	}
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		t.Errorf("callback must bind loopback only; got %q", redirect)
	}
	return redirect, state
}
