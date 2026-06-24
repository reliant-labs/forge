package kclplugin

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// DeriveES256JWK derives the PUBLIC JWK document from an ES256 (P-256)
// private-key PEM, so a forge.TestJWKS can publish the public half of the
// exact key its signer uses — the JWKS and the signer can never drift.
//
// Ported from control-plane's e2e deriveTestJWK: parse the EC private key,
// left-pad the 32-byte P-256 field elements (so leading-zero coordinates
// still encode to the fixed JWK width), and base64url-encode x/y. kid/alg
// are caller-supplied; kty/crv/use are fixed for ES256.
//
// Returns a map[string]any (KCL-plugin-friendly) shaped exactly like one
// entry in a JWKS `keys` array.
func DeriveES256JWK(privateKeyPEM, kid, alg string) (map[string]any, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("derive_jwk: no PEM block in private key")
	}
	key, err := parseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("derive_jwk: parse EC private key: %w", err)
	}
	pub := &key.PublicKey
	// P-256 field elements are 32 bytes; FillBytes left-pads so a
	// leading-zero coordinate still encodes to the fixed width.
	const coordLen = 32
	xBytes := make([]byte, coordLen)
	yBytes := make([]byte, coordLen)
	pub.X.FillBytes(xBytes)
	pub.Y.FillBytes(yBytes)
	if alg == "" {
		alg = "ES256"
	}
	return map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(xBytes),
		"y":   base64.RawURLEncoding.EncodeToString(yBytes),
		"kid": kid,
		"alg": alg,
		"use": "sig",
	}, nil
}

// parseECPrivateKey parses a SEC1 ("EC PRIVATE KEY") or PKCS#8 ("PRIVATE
// KEY") encoded ES256 key, so either PEM header works.
func parseECPrivateKey(der []byte) (*ecdsa.PrivateKey, error) {
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	ec, ok := k8.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ECDSA private key (got %T)", k8)
	}
	return ec, nil
}
