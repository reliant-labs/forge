// Package auth provides JWT and API-key authentication for forge-generated
// Connect RPC services.
//
// The library exposes a [Validator] that authenticates incoming Connect
// requests against a configured provider ("jwt", "api_key", "both", or
// "none") and a Connect interceptor that attaches the resulting [Claims] to
// the request context.
//
// Generated forge projects import this package via a thin shim in
// pkg/middleware/auth_gen.go. The shim wires the project's [Config] from
// forge.yaml and exposes a project-local Claims alias (type Claims = auth.Claims).
package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
)

// Provider names recognized by [Config].
const (
	ProviderNone   = "none"
	ProviderJWT    = "jwt"
	ProviderAPIKey = "api_key"
	ProviderBoth   = "both"
)

// Claims is the canonical claims shape produced by all forge auth flows.
//
// The struct mirrors the per-project pkg/middleware/Claims that earlier
// forge versions generated. The generated shim re-exports it via
// type Claims = auth.Claims so existing project code (referring to
// middleware.Claims) keeps compiling.
type Claims struct {
	UserID string   `json:"user_id"`
	Email  string   `json:"email"`
	OrgID  string   `json:"org_id"`
	Role   string   `json:"role"`
	Roles  []string `json:"roles"`
}

// KeyValidator validates an API key and returns the associated claims.
// Implementations typically look up the key in a database or cache.
type KeyValidator interface {
	ValidateKey(ctx context.Context, key string) (*Claims, error)
}

// Config configures a [Validator].
//
// Provider selects the authentication scheme. JWT, APIKey and SkipMethods
// further customize each scheme. The library reads JWT_SECRET from the
// environment when [JWTConfig.Secret] is empty (preserving the legacy
// behaviour of the generated auth_gen.go template).
type Config struct {
	// Provider is one of "jwt", "api_key", "both", "none". Required.
	Provider string

	// JWT configures JWT validation. Used when Provider is "jwt" or "both".
	JWT JWTConfig

	// APIKey configures API key validation. Used when Provider is
	// "api_key" or "both". A [KeyValidator] must be supplied separately
	// via [Validator.SetKeyValidator] or [Config.KeyValidator].
	APIKey APIKeyConfig

	// SkipMethods is the list of fully-qualified Connect procedure names
	// that bypass auth (in addition to the built-in /Health/ skip).
	SkipMethods []string

	// KeyValidator validates API keys. Required when APIKey auth is enabled.
	KeyValidator KeyValidator
}

// JWTConfig holds JWT-specific settings.
type JWTConfig struct {
	// SigningMethod is the expected JWT alg value (e.g. "HS256", "RS256").
	// Defaults to "RS256" when empty.
	SigningMethod string

	// Issuer, when set, is enforced via jwt.WithIssuer.
	Issuer string

	// Audience, when set, is enforced via jwt.WithAudience.
	Audience string

	// JWKSURL, when set, signals JWKS-based key resolution. The current
	// implementation does not auto-fetch JWKS (matching the legacy template
	// behaviour) — callers that need JWKS should populate Secret with a
	// fetched key or supply a KeyFunc.
	JWKSURL string

	// Secret is the symmetric secret (HS*) or PEM-encoded public key
	// (RS*/ES*). When empty the validator falls back to os.Getenv("JWT_SECRET").
	Secret string

	// KeyFunc, when non-nil, fully overrides key resolution. Useful for tests
	// and for callers wiring custom JWKS clients.
	KeyFunc jwt.Keyfunc
}

// EffectiveSigningMethod returns SigningMethod or the "RS256" default.
func (j JWTConfig) EffectiveSigningMethod() string {
	if j.SigningMethod == "" {
		return "RS256"
	}
	return j.SigningMethod
}

// APIKeyConfig holds API-key-specific settings.
type APIKeyConfig struct {
	// Header is the HTTP header name carrying the API key.
	// Defaults to "X-API-Key" when empty.
	Header string
}

// EffectiveHeader returns Header or the "X-API-Key" default.
func (a APIKeyConfig) EffectiveHeader() string {
	if a.Header == "" {
		return "X-API-Key"
	}
	return a.Header
}

// InterceptorOptions configures Validator.Interceptor behaviour beyond the
// fields already on [Config].
type InterceptorOptions struct {
	// SkipMethods overrides Config.SkipMethods when non-nil.
	SkipMethods []string

	// AllowDevMode, when true, skips real authentication and injects
	// DevClaims when no Authorization header is present. Only honour this
	// in non-production builds; the constructor does not gate on env.
	AllowDevMode bool

	// DevClaims is injected when AllowDevMode is true and the request has
	// no credentials. Defaults to a non-nil empty *Claims.
	DevClaims *Claims
}

// Validator authenticates Connect RPC requests against a configured provider.
// Construct with [NewValidator]; reuse a single Validator per service.
type Validator struct {
	cfg          Config
	skipMethods  map[string]bool
	keyValidator KeyValidator
}

// NewValidator returns a Validator wired for cfg.Provider.
//
// It returns an error when Provider is unrecognized. The "none" provider is
// allowed and produces a Validator whose Interceptor is a no-op (useful for
// tests and for projects that haven't enabled auth yet).
func NewValidator(cfg Config) (*Validator, error) {
	switch cfg.Provider {
	case ProviderNone, ProviderJWT, ProviderAPIKey, ProviderBoth, "":
	default:
		return nil, fmt.Errorf("auth: unknown provider %q", cfg.Provider)
	}

	v := &Validator{
		cfg:          cfg,
		keyValidator: cfg.KeyValidator,
		skipMethods:  make(map[string]bool, len(cfg.SkipMethods)),
	}
	for _, m := range cfg.SkipMethods {
		v.skipMethods[m] = true
	}
	return v, nil
}

// Provider returns the configured provider name.
func (v *Validator) Provider() string { return v.cfg.Provider }

// SetKeyValidator replaces the configured [KeyValidator]. Useful when the
// validator is constructed before the storage backend is ready.
func (v *Validator) SetKeyValidator(kv KeyValidator) { v.keyValidator = kv }

// Close releases any resources held by the Validator. Currently a no-op;
// kept for forward compatibility with JWKS-cache implementations.
func (v *Validator) Close() error { return nil }

// Validate authenticates a single bearer JWT and returns the parsed claims.
// Useful outside the interceptor (e.g. webhook auth, CLI tooling).
func (v *Validator) Validate(token string) (*Claims, error) {
	return v.validateJWT(token)
}

// IsUnauthenticatedProcedure reports whether procedure should bypass auth.
// This includes the built-in /Health/ skip plus any explicit skip list.
func (v *Validator) IsUnauthenticatedProcedure(procedure string, skipMethods []string) bool {
	if strings.Contains(procedure, "Health") {
		return true
	}
	if len(skipMethods) > 0 {
		for _, m := range skipMethods {
			if m == procedure {
				return true
			}
		}
		return false
	}
	return v.skipMethods[procedure]
}

// AuthenticateHeaders authenticates a request given its headers and returns
// the parsed claims (or an error). It is the testable core of [Validator.Interceptor]
// — the interceptor wraps this with the procedure-skip logic and context plumbing.
func (v *Validator) AuthenticateHeaders(ctx context.Context, headers http.Header, opts InterceptorOptions) (*Claims, error) {
	provider := v.cfg.Provider
	if provider == "" {
		provider = ProviderNone
	}
	if provider == ProviderNone {
		return nil, nil
	}

	switch provider {
	case ProviderJWT:
		c, err := v.authenticateJWT(headers)
		if err != nil && opts.AllowDevMode && headers.Get("Authorization") == "" {
			return devClaims(opts), nil
		}
		return c, err
	case ProviderAPIKey:
		c, err := v.authenticateAPIKey(ctx, headers)
		if err != nil && opts.AllowDevMode && headers.Get(v.cfg.APIKey.EffectiveHeader()) == "" {
			return devClaims(opts), nil
		}
		return c, err
	case ProviderBoth:
		c, err := v.authenticateJWT(headers)
		if err == nil {
			return c, nil
		}
		c2, kerr := v.authenticateAPIKey(ctx, headers)
		if kerr == nil {
			return c2, nil
		}
		if opts.AllowDevMode && headers.Get("Authorization") == "" && headers.Get(v.cfg.APIKey.EffectiveHeader()) == "" {
			return devClaims(opts), nil
		}
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("authentication failed: provide a valid Bearer token or API key"))
	}
	return nil, nil
}

// Interceptor returns a Connect interceptor that authenticates each unary
// request and stores the resulting claims in the context using the supplied
// claims-context helper.
//
// withClaims is the function the user's pkg/middleware exposes for putting
// claims into context (typically middleware.ContextWithClaims).
func (v *Validator) Interceptor(opts InterceptorOptions, withClaims func(context.Context, *Claims) context.Context) connect.UnaryInterceptorFunc {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if v.IsUnauthenticatedProcedure(req.Spec().Procedure, opts.SkipMethods) {
				return next(ctx, req)
			}
			claims, err := v.AuthenticateHeaders(ctx, req.Header(), opts)
			if err != nil {
				return nil, err
			}
			if claims != nil && withClaims != nil {
				ctx = withClaims(ctx, claims)
			}
			return next(ctx, req)
		})
	})
}

func devClaims(opts InterceptorOptions) *Claims {
	if opts.DevClaims != nil {
		return opts.DevClaims
	}
	return &Claims{}
}

// authenticateJWT extracts and validates a JWT from the Authorization header.
func (v *Validator) authenticateJWT(headers http.Header) (*Claims, error) {
	authorization := headers.Get("Authorization")
	if authorization == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("missing authorization header"))
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == authorization {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid authorization format: expected Bearer token"))
	}
	claims, err := v.validateJWT(token)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid token: %w", err))
	}
	return claims, nil
}

func (v *Validator) authenticateAPIKey(ctx context.Context, headers http.Header) (*Claims, error) {
	header := v.cfg.APIKey.EffectiveHeader()
	apiKey := headers.Get(header)
	if apiKey == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("missing API key header (%s)", header))
	}
	if v.keyValidator == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("API key validator not configured"))
	}
	claims, err := v.keyValidator.ValidateKey(ctx, apiKey)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid API key: %w", err))
	}
	return claims, nil
}

func (v *Validator) validateJWT(tokenString string) (*Claims, error) {
	signingMethod := v.cfg.JWT.EffectiveSigningMethod()

	keyFunc := v.cfg.JWT.KeyFunc
	if keyFunc == nil {
		keyFunc = func(token *jwt.Token) (interface{}, error) {
			if token.Method.Alg() != signingMethod {
				return nil, fmt.Errorf("unexpected signing method: %s", token.Method.Alg())
			}
			secret := v.cfg.JWT.Secret
			if secret == "" {
				secret = os.Getenv("JWT_SECRET")
			}
			if secret == "" {
				if v.cfg.JWT.JWKSURL != "" {
					return nil, fmt.Errorf("JWKS key fetching not yet implemented: set JWT.Secret/JWT_SECRET or supply JWT.KeyFunc (JWKS URL: %s)", v.cfg.JWT.JWKSURL)
				}
				return nil, fmt.Errorf("JWT_SECRET environment variable not set")
			}
			return decodeJWTKey(signingMethod, secret)
		}
	}

	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{signingMethod}),
		jwt.WithLeeway(5 * time.Second),
	}
	if v.cfg.JWT.Issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(v.cfg.JWT.Issuer))
	}
	if v.cfg.JWT.Audience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(v.cfg.JWT.Audience))
	}

	token, err := jwt.Parse(tokenString, keyFunc, parserOpts...)
	if err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	mapClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("unexpected claims type")
	}

	return &Claims{
		UserID: getStringClaim(mapClaims, "sub"),
		Email:  getStringClaim(mapClaims, "email"),
		OrgID:  getStringClaim(mapClaims, "org_id"),
		Role:   getStringClaim(mapClaims, "role"),
		Roles:  getStringSliceClaim(mapClaims, "roles"),
	}, nil
}

// decodeJWTKey converts a secret string into the type that github.com/golang-jwt/jwt/v5
// expects for the configured signing method.
func decodeJWTKey(signingMethod, secret string) (interface{}, error) {
	switch {
	case strings.HasPrefix(signingMethod, "HS"):
		return []byte(secret), nil
	case strings.HasPrefix(signingMethod, "RS") || strings.HasPrefix(signingMethod, "PS"):
		key, err := jwt.ParseRSAPublicKeyFromPEM([]byte(secret))
		if err == nil {
			return key, nil
		}
		// Fall back to detecting raw PEM with no decoded blocks.
		if block, _ := pem.Decode([]byte(secret)); block == nil {
			return nil, fmt.Errorf("RSA public key is not PEM-encoded")
		}
		return nil, err
	case strings.HasPrefix(signingMethod, "ES"):
		return jwt.ParseECPublicKeyFromPEM([]byte(secret))
	}
	return []byte(secret), nil
}

// Compile-time assertions that the parsed key types are usable by jwt-go.
var (
	_ *rsa.PublicKey   = (*rsa.PublicKey)(nil)
	_ *ecdsa.PublicKey = (*ecdsa.PublicKey)(nil)
)

// getStringClaim safely extracts a string claim from JWT map claims.
func getStringClaim(claims jwt.MapClaims, key string) string {
	if val, ok := claims[key]; ok {
		if s, ok := val.(string); ok {
			return s
		}
	}
	return ""
}

// getStringSliceClaim safely extracts a string slice claim from JWT map claims.
func getStringSliceClaim(claims jwt.MapClaims, key string) []string {
	val, ok := claims[key]
	if !ok {
		return nil
	}
	switch v := val.(type) {
	case []interface{}:
		var result []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return v
	default:
		return nil
	}
}
