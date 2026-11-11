package authhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errUnverified = errors.New("authhooks: delivery not verified")

// verifyBearer accepts a request whose Authorization header is
// "Bearer <secret>", compared in constant time. An empty secret verifies
// nothing.
func verifyBearer(r *http.Request, secret string) error {
	if secret == "" {
		return errUnverified
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return errUnverified
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(secret)) != 1 {
		return errUnverified
	}
	return nil
}

// StandardWebhooksTolerance is how far a Standard Webhooks delivery's
// timestamp may be from now, either way, before it is refused as a replay.
const StandardWebhooksTolerance = 5 * time.Minute

// verifyStandardWebhooks accepts a delivery signed per the Standard Webhooks
// spec (standardwebhooks.com), which Svix — and so Clerk — implements: the
// headers webhook-id, webhook-timestamp and webhook-signature (or their
// svix- spellings), and a signature "v1,<base64 HMAC-SHA256>" over
// "<id>.<timestamp>.<body>" keyed by the secret's base64 payload
// ("whsec_<base64>"). The signature header may carry several, space
// separated, during a key rotation; one match is enough.
func verifyStandardWebhooks(r *http.Request, body []byte, secret string, now time.Time) error {
	key, err := standardWebhooksKey(secret)
	if err != nil {
		return errUnverified
	}
	id := webhookHeader(r, "id")
	ts := webhookHeader(r, "timestamp")
	sigs := webhookHeader(r, "signature")
	if id == "" || ts == "" || sigs == "" {
		return errUnverified
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errUnverified
	}
	if d := now.Sub(time.Unix(secs, 0)); d > StandardWebhooksTolerance || d < -StandardWebhooksTolerance {
		return errUnverified
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, s := range strings.Fields(sigs) {
		version, sig, ok := strings.Cut(s, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return errUnverified
}

// standardWebhooksKey decodes a "whsec_<base64>" secret; a secret without the
// prefix is taken as the base64 payload itself.
func standardWebhooksKey(secret string) ([]byte, error) {
	secret = strings.TrimPrefix(strings.TrimSpace(secret), "whsec_")
	if secret == "" {
		return nil, errUnverified
	}
	return base64.StdEncoding.DecodeString(secret)
}

// webhookHeader reads a Standard Webhooks header in either spelling.
func webhookHeader(r *http.Request, name string) string {
	if v := r.Header.Get("webhook-" + name); v != "" {
		return v
	}
	return r.Header.Get("svix-" + name)
}
