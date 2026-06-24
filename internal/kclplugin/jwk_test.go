package kclplugin

import (
	"encoding/base64"
	"testing"
)

const jwkTestPEM = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIB2fmGPM1CXk+kC+GBvg5pD/zwwK+XRy8+mKNfr0PABuoAoGCCqGSM49
AwEHoUQDQgAEGdqz6sZl229WS3ixXQmFory5kkkus2UT4cBGQuO3dpMN2FQ/8260
9YszSMpty7qF7I3/9elHmcVvzBglAF7CrQ==
-----END EC PRIVATE KEY-----`

func TestDeriveES256JWK(t *testing.T) {
	jwk, err := DeriveES256JWK(jwkTestPEM, "e2e-test-es256", "ES256")
	if err != nil {
		t.Fatalf("DeriveES256JWK: %v", err)
	}
	if jwk["kty"] != "EC" || jwk["crv"] != "P-256" {
		t.Errorf("kty/crv wrong: %v / %v", jwk["kty"], jwk["crv"])
	}
	if jwk["kid"] != "e2e-test-es256" || jwk["alg"] != "ES256" || jwk["use"] != "sig" {
		t.Errorf("kid/alg/use wrong: %v", jwk)
	}
	// x/y must be 32-byte (P-256) base64url-no-pad coordinates.
	for _, f := range []string{"x", "y"} {
		s, _ := jwk[f].(string)
		dec, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Errorf("%s not base64url: %v", f, err)
		}
		if len(dec) != 32 {
			t.Errorf("%s decoded len = %d want 32", f, len(dec))
		}
	}
	// Deterministic: same PEM => same JWK.
	jwk2, _ := DeriveES256JWK(jwkTestPEM, "e2e-test-es256", "ES256")
	if jwk["x"] != jwk2["x"] || jwk["y"] != jwk2["y"] {
		t.Error("derivation not deterministic")
	}
}

func TestDeriveES256JWK_AlgDefault(t *testing.T) {
	jwk, err := DeriveES256JWK(jwkTestPEM, "k", "")
	if err != nil {
		t.Fatalf("DeriveES256JWK: %v", err)
	}
	if jwk["alg"] != "ES256" {
		t.Errorf("empty alg should default to ES256, got %v", jwk["alg"])
	}
}

func TestDeriveES256JWK_BadPEM(t *testing.T) {
	if _, err := DeriveES256JWK("not a pem", "k", "ES256"); err == nil {
		t.Error("expected error for non-PEM input")
	}
}
