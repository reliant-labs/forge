package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/reliant-labs/forge/pkg/oauth2"
)

func TestBrowserLoginRefusesOrgBoundMachineToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"rlat_legacy","token_type":"Bearer","org_id":"A"}`))
	}))
	defer srv.Close()
	open := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		back, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			return err
		}
		v := back.Query()
		v.Set("code", "code")
		v.Set("state", q.Get("state"))
		back.RawQuery = v.Encode()
		resp, err := http.Get(back.String())
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	_, err := (BrowserLogin{Endpoint: Endpoint{URL: srv.URL}, OpenURL: open}).Run(context.Background())
	if err == nil {
		t.Fatal("org-bound automation-style login silently reclassified as identity")
	}
}

func TestCredentialKindComesFromOAuthResponseNotTokenPrefix(t *testing.T) {
	token := &oauth2.Token{AccessToken: "opaque-human", Raw: map[string]any{"credential_kind": "human"}}
	if tokenCredentialKind(token) != "human" {
		t.Fatal("human identity metadata lost")
	}
}
