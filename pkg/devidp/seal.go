package devidp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Sealed binding cookies.
//
// A sign-in ceremony that spans two requests (passkey begin → finish, magic
// request → consume, social start → callback, invite → passkey enrolment)
// binds its state to the browser that started it with an HttpOnly cookie.
// The cookie is SEALED (AES-GCM) for two reasons:
//
//   - CONFIDENTIALITY FROM THE REQUESTER. Whoever asks for a magic link or a
//     passkey challenge receives the cookie, and can read it. Its contents
//     differ between "that address has an account" and "it does not" (a
//     real session versus a decoy), so in clear text it would be the
//     enumeration oracle the uniform responses exist to close.
//   - INTEGRITY. A forged cookie naming another user is refused before the
//     issuer is called, rather than relying only on the issuer's own check.
//
// The key is derived from the broker's own token, which every replica shares
// and nobody else holds, so no new secret has to be provisioned. Rotating the
// token invalidates cookies in flight; their life is minutes, and the person
// starts the ceremony again.

const sealInfo = "forge devidp binding cookie v1"

// sealer seals and opens binding-cookie payloads.
type sealer struct {
	key func() []byte // the current key material (the broker token)
	now func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]cipher.AEAD
}

type sealedEnvelope struct {
	Kind    string          `json:"k"`
	Expires int64           `json:"e"`
	Payload json.RawMessage `json:"p"`
}

var errSealed = errors.New("binding cookie is missing, expired or not ours")

func (s *sealer) aead() (cipher.AEAD, error) {
	material := s.key()
	if len(material) == 0 {
		return nil, fmt.Errorf("no key material for binding cookies (the broker has no token)")
	}
	k := sha256.Sum256(append([]byte(sealInfo+"\x00"), material...))
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.cache[k]; ok {
		return a, nil
	}
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if s.cache == nil || len(s.cache) > 4 {
		s.cache = map[[32]byte]cipher.AEAD{}
	}
	s.cache[k] = a
	return a, nil
}

// seal encodes payload under kind, valid for ttl. The kind is bound into the
// envelope so a cookie of one ceremony cannot be replayed as another's.
func (s *sealer) seal(kind string, payload any, ttl time.Duration) (string, error) {
	a, err := s.aead()
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(sealedEnvelope{Kind: kind, Expires: s.now().Add(ttl).UnixMilli(), Payload: p})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(a.Seal(nonce, nonce, plain, []byte(kind))), nil
}

// open decodes a sealed value of kind into out.
func (s *sealer) open(kind, value string, out any) error {
	if value == "" {
		return errSealed
	}
	a, err := s.aead()
	if err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < a.NonceSize() {
		return errSealed
	}
	plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(kind))
	if err != nil {
		return errSealed
	}
	var env sealedEnvelope
	if err := json.Unmarshal(plain, &env); err != nil || env.Kind != kind || s.now().UnixMilli() > env.Expires {
		return errSealed
	}
	if err := json.Unmarshal(env.Payload, out); err != nil {
		return errSealed
	}
	return nil
}
