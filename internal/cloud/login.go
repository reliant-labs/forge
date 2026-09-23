package cloud

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// BrowserLogin runs the interactive half of `forge login`: bind a
// loopback listener on an ephemeral port, send the user to the
// endpoint's authorize URL with that port as the redirect, and wait for
// the callback to hand back a credential.
//
// WHY A LOOPBACK LISTENER AND NOT A DEVICE CODE. The callback is how the
// browser returns the credential to a process that has no other channel
// to it. Binding port 0 and reading back the assigned port means two
// developers — or two agents on one machine — can log in at the same
// time without colliding, which a fixed port cannot promise.
//
// This is modelled on reliant's CLI login (which does the same thing)
// but shares no code with it, deliberately. A user deploying to hosted
// infra without the reliant harness must never be required to install
// reliant first, so forge cannot depend on reliant's CLI or its
// packages — the dependency would make `forge login` a lie for exactly
// the users it exists to serve.
type BrowserLogin struct {
	// Endpoint is the control plane being logged in to.
	Endpoint Endpoint
	// OpenURL is the browser launcher. Injectable so a test drives the
	// whole flow without opening a real browser.
	OpenURL func(string) error
	// Timeout bounds the wait for the callback. A login that hangs
	// forever is indistinguishable from one that is about to succeed.
	Timeout time.Duration
	// Out receives the "open this URL" notice — printed because the
	// launcher can fail silently on a headless box, and the user needs
	// the URL to continue by hand.
	Out io.Writer
}

// Run performs the flow and returns the credential the callback
// delivered. The context bounds the whole operation; Timeout bounds the
// callback wait specifically.
func (b BrowserLogin) Run(ctx context.Context) (StoredLogin, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return StoredLogin{}, fmt.Errorf("start callback listener: %w", err)
	}
	// Discarded deliberately, and it is normally non-nil. Defers run LIFO,
	// so the srv.Shutdown below runs FIRST and closes the listener as part
	// of shutting the server down; this close is the safety net for the
	// paths that return before the server is ever started, and on the
	// common path it reports "use of closed network connection". Surfacing
	// that would mean failing a login that already succeeded.
	defer func() { _ = listener.Close() }()

	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	// A random state, checked on the callback. Without it any local
	// process could POST a credential of its choosing to the listener
	// and forge would store it.
	state, err := randomState()
	if err != nil {
		return StoredLogin{}, err
	}

	authorizeURL := b.Endpoint.URL + "/oauth/authorize?" + url.Values{
		"redirect_uri": {redirectURI},
		"state":        {state},
		"client":       {"forge"},
		"response":     {"token"},
	}.Encode()

	type event struct {
		login StoredLogin
		err   error
	}
	results := make(chan event, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			results <- event{err: fmt.Errorf("callback state mismatch — the login did not come from this `forge login`")}
			return
		}
		if msg := q.Get("error_description"); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			results <- event{err: fmt.Errorf("login rejected: %s", msg)}
			return
		}
		token := strings.TrimSpace(q.Get("token"))
		if token == "" {
			http.Error(w, "no token in callback", http.StatusBadRequest)
			results <- event{err: fmt.Errorf("callback carried no token")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, loginSuccessPage)
		results <- event{login: StoredLogin{
			Token:     token,
			Endpoint:  b.Endpoint.URL,
			Account:   q.Get("account"),
			ExpiresAt: q.Get("expires_at"),
		}}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if b.Out != nil {
		fmt.Fprintf(b.Out, "Opening your browser to log in to %s\n", b.Endpoint.URL)
		fmt.Fprintf(b.Out, "If it does not open, visit:\n    %s\n\n", authorizeURL)
	}
	open := b.OpenURL
	if open == nil {
		open = OpenBrowser
	}
	// A failed launch is NOT fatal: the URL has already been printed, so
	// a headless or locked-down machine can still complete the flow by
	// hand. Aborting here would break the SSH case for no benefit.
	_ = open(authorizeURL)

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	select {
	case ev := <-results:
		return ev.login, ev.err
	case <-time.After(timeout):
		return StoredLogin{}, fmt.Errorf(
			"timed out after %s waiting for the login callback\n"+
				"fix: complete the flow in the browser, or use `forge login --token <token>` "+
				"(also what CI uses, since a pipeline has no browser)", timeout)
	case <-ctx.Done():
		return StoredLogin{}, ctx.Err()
	}
}

// VerifyToken checks a credential against the endpoint before storing
// it, so `forge login --token` fails at login rather than at the first
// real command. Errors are advisory: the caller decides whether a
// verification failure blocks the store.
func VerifyToken(ctx context.Context, ep Endpoint, cred Credential) error {
	client := NewClient(ep, cred)
	var out json.RawMessage
	return client.Call(ctx, "controlplane.v1.DeployService/ListReleases",
		map[string]any{"limit": 1}, &out)
}

func randomState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate login state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// OpenBrowser launches the platform's URL handler.
func OpenBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "linux":
		return exec.Command("xdg-open", target).Start()
	case "windows":
		return exec.Command("cmd", "/c", "start", target).Start()
	default:
		return fmt.Errorf("cannot open a browser on %s — visit the URL above by hand", runtime.GOOS)
	}
}

const loginSuccessPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>forge login</title></head>
<body style="font-family:system-ui;text-align:center;padding-top:4rem">
<h1>Logged in</h1>
<p>You can close this tab and return to your terminal.</p>
</body></html>`
