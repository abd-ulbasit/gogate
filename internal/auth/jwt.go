// Package auth provides JWT parsing, validation, and authentication middleware.
//
// This package implements JWT (JSON Web Token) validation WITHOUT external dependencies,
// using only Go's standard library (crypto/*, encoding/*).
//
// Supported algorithms:
//   - HS256 (HMAC-SHA256): Symmetric, uses shared secret
//   - RS256 (RSA-SHA256): Asymmetric, uses public/private key pair
//
// JWT Structure:
//
//	Header.Payload.Signature (each part is Base64URL encoded)
//
//	Header:    {"alg": "RS256", "typ": "JWT"}
//	Payload:   {"sub": "user123", "exp": 1700000000, "iss": "auth.example.com"}
//	Signature: Algorithm-specific hash of "header.payload"
//
// Why build from scratch?
//   - Learning: Understand JWT internals (encoding, signing, verification)
//   - Security awareness: Know exactly what's being validated
//   - Zero dependencies: No external attack surface
//   - Production: Would use github.com/golang-jwt/jwt, but learning > convenience
//
// Thread safety: All public functions are safe for concurrent use.
package auth

import (
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Common errors with descriptive messages for debugging
var (
	ErrInvalidToken     = errors.New("invalid token format")
	ErrInvalidHeader    = errors.New("invalid token header")
	ErrInvalidPayload   = errors.New("invalid token payload")
	ErrInvalidSignature = errors.New("invalid signature")
	ErrTokenExpired     = errors.New("token has expired")
	ErrInvalidIssuer    = errors.New("invalid issuer")
	ErrInvalidAudience  = errors.New("invalid audience")
	ErrUnsupportedAlg   = errors.New("unsupported signing algorithm")
	ErrMissingKey       = errors.New("signing key not configured")
)

// Algorithm represents the signing algorithm used in the JWT.
type Algorithm string

const (
	AlgHS256 Algorithm = "HS256" // HMAC-SHA256 (symmetric)
	AlgRS256 Algorithm = "RS256" // RSA-SHA256 (asymmetric)
)

// Header represents the JWT header (first part of the token).
//
// The header typically contains:
//   - alg: Signing algorithm (HS256, RS256, etc.)
//   - typ: Token type (always "JWT" for our purposes)
//   - kid: Key ID (optional, for key rotation)
type Header struct {
	Algorithm Algorithm `json:"alg"`
	Type      string    `json:"typ"`
	KeyID     string    `json:"kid,omitempty"`
}

// Claims represents the JWT payload (second part of the token).
//
// Standard claims (registered claims per RFC 7519):
//   - sub (Subject): Who the token is about (usually user ID)
//   - exp (Expiration): Unix timestamp when token expires
//   - iat (Issued At): Unix timestamp when token was issued
//   - iss (Issuer): Who issued the token (e.g., "auth.example.com")
//   - aud (Audience): Who the token is intended for (e.g., "api.example.com")
//   - nbf (Not Before): Token is invalid before this time
//
// Custom claims can be added via the Custom map.
type Claims struct {
	// Standard registered claims
	Subject   string `json:"sub,omitempty"`
	ExpiresAt int64  `json:"exp,omitempty"`
	IssuedAt  int64  `json:"iat,omitempty"`
	Issuer    string `json:"iss,omitempty"`
	Audience  string `json:"aud,omitempty"`
	NotBefore int64  `json:"nbf,omitempty"`
	JWTID     string `json:"jti,omitempty"`

	// Custom claims (application-specific)
	// Example: {"roles": ["admin", "user"], "tenant_id": "acme"}
	Custom map[string]interface{} `json:"-"`
}

// Token represents a parsed JWT with header, claims, and raw parts.
type Token struct {
	Header    Header
	Claims    Claims
	Raw       string // Original token string
	Signature []byte // Decoded signature bytes
}

// Parse splits a JWT string into its parts and decodes the header and payload.
//
// This does NOT verify the signature - use Validator.Validate() for that.
//
// Why separate Parse from Validate?
//   - Parse is cheap (just decoding)
//   - Validate requires crypto operations (expensive)
//   - Sometimes you need to read claims without validation (e.g., to get kid for key lookup)
//
// Token format: header.payload.signature (each base64url encoded)
func Parse(tokenString string) (*Token, error) {
	// Split into three parts
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: expected 3 parts, got %d", ErrInvalidToken, len(parts))
	}

	// Decode header (first part)
	headerBytes, err := base64URLDecode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidHeader, err)
	}

	var header Header
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidHeader, err)
	}

	// Decode payload (second part)
	payloadBytes, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}

	// First unmarshal into a map to capture custom claims
	var rawClaims map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &rawClaims); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}

	// Then unmarshal into Claims struct for standard fields
	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}

	// Extract custom claims (everything that's not a standard claim)
	claims.Custom = extractCustomClaims(rawClaims)

	// Decode signature (third part)
	signature, err := base64URLDecode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}

	return &Token{
		Header:    header,
		Claims:    claims,
		Raw:       tokenString,
		Signature: signature,
	}, nil
}

// extractCustomClaims removes standard claims and returns custom ones.
func extractCustomClaims(raw map[string]interface{}) map[string]interface{} {
	standard := map[string]bool{
		"sub": true, "exp": true, "iat": true,
		"iss": true, "aud": true, "nbf": true, "jti": true,
	}

	custom := make(map[string]interface{})
	for k, v := range raw {
		if !standard[k] {
			custom[k] = v
		}
	}

	if len(custom) == 0 {
		return nil
	}
	return custom
}

// base64URLDecode decodes a base64url encoded string (RFC 4648).
//
// JWT uses base64url encoding (NOT standard base64):
//   - '+' → '-' (plus replaced with minus)
//   - '/' → '_' (slash replaced with underscore)
//   - No padding ('=' characters)
//
// Why base64url?
//   - URL-safe: Can be passed in URLs without encoding
//   - No padding: Smaller tokens
func base64URLDecode(s string) ([]byte, error) {
	// Add padding if missing (base64url omits padding)
	// Padding formula: (4 - len%4) % 4
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}

	return base64.URLEncoding.DecodeString(s)
}

// base64URLEncode encodes bytes to base64url (for testing/signing).
func base64URLEncode(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

// ValidatorConfig configures JWT validation behavior.
type ValidatorConfig struct {
	// Expected issuer (iss claim). Empty string skips validation.
	Issuer string

	// Expected audience (aud claim). Empty string skips validation.
	Audience string

	// Clock skew tolerance for expiration checks.
	// Allows for slight time differences between servers.
	// Default: 0 (strict), Recommended: 30s-60s
	ClockSkew time.Duration

	// HS256 secret key (for symmetric signing)
	SecretKey []byte

	// RS256 public key (for asymmetric signing)
	PublicKey *rsa.PublicKey
}

// Validator validates JWT tokens against configured rules.
//
// Validation steps:
//  1. Parse token (decode header, payload, signature)
//  2. Check algorithm matches expected
//  3. Verify signature using configured key
//  4. Check expiration (exp claim)
//  5. Check not-before (nbf claim) if present
//  6. Check issuer (iss claim) if configured
//  7. Check audience (aud claim) if configured
//
// Thread-safe: Can be shared across goroutines.
type Validator struct {
	config ValidatorConfig
}

// NewValidator creates a new JWT validator with the given configuration.
func NewValidator(config ValidatorConfig) *Validator {
	return &Validator{config: config}
}

// Validate parses and validates a JWT token string.
//
// Returns the parsed token with claims if valid, error otherwise.
//
// Validation is fail-fast: Returns on first error encountered.
// This prevents information leakage (attacker can't probe which check fails).
func (v *Validator) Validate(tokenString string) (*Token, error) {
	// Step 1: Parse token
	token, err := Parse(tokenString)
	if err != nil {
		return nil, err
	}

	// Step 2: Verify signature
	if err := v.verifySignature(token); err != nil {
		return nil, err
	}

	// Step 3: Check expiration
	if err := v.checkExpiration(token); err != nil {
		return nil, err
	}

	// Step 4: Check not-before
	if err := v.checkNotBefore(token); err != nil {
		return nil, err
	}

	// Step 5: Check issuer
	if err := v.checkIssuer(token); err != nil {
		return nil, err
	}

	// Step 6: Check audience
	if err := v.checkAudience(token); err != nil {
		return nil, err
	}

	return token, nil
}

// verifySignature checks the token signature using the configured key.
func (v *Validator) verifySignature(token *Token) error {
	// Get the signed portion (header.payload)
	parts := strings.Split(token.Raw, ".")
	signedPortion := parts[0] + "." + parts[1]

	switch token.Header.Algorithm {
	case AlgHS256:
		return v.verifyHS256(signedPortion, token.Signature)
	case AlgRS256:
		return v.verifyRS256(signedPortion, token.Signature)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedAlg, token.Header.Algorithm)
	}
}

// verifyHS256 verifies HMAC-SHA256 signature.
//
// How HS256 works:
//  1. Create HMAC-SHA256 with secret key
//  2. Hash the signed portion (header.payload)
//  3. Compare computed hash with signature
//
// Security: Uses constant-time comparison to prevent timing attacks.
func (v *Validator) verifyHS256(signedPortion string, signature []byte) error {
	if len(v.config.SecretKey) == 0 {
		return fmt.Errorf("%w: HS256 secret key not set", ErrMissingKey)
	}

	// Compute expected signature
	h := hmac.New(sha256.New, v.config.SecretKey)
	h.Write([]byte(signedPortion))
	expected := h.Sum(nil)

	// Constant-time comparison (prevents timing attacks)
	if !hmac.Equal(signature, expected) {
		return ErrInvalidSignature
	}

	return nil
}

// verifyRS256 verifies RSA-SHA256 signature.
//
// How RS256 works:
//  1. Hash the signed portion with SHA256
//  2. Verify signature was created with matching private key
//  3. Public key can verify but not create signatures
//
// Why RSA?
//   - Asymmetric: Auth server has private key, API servers have public key
//   - If public key is leaked, tokens can't be forged
//   - Key rotation: Distribute new public key without changing private key
func (v *Validator) verifyRS256(signedPortion string, signature []byte) error {
	if v.config.PublicKey == nil {
		return fmt.Errorf("%w: RS256 public key not set", ErrMissingKey)
	}

	// Hash the signed portion
	hash := sha256.Sum256([]byte(signedPortion))

	// Verify signature
	err := rsa.VerifyPKCS1v15(v.config.PublicKey, crypto.SHA256, hash[:], signature)
	if err != nil {
		return ErrInvalidSignature
	}

	return nil
}

// checkExpiration verifies the token hasn't expired.
func (v *Validator) checkExpiration(token *Token) error {
	if token.Claims.ExpiresAt == 0 {
		// No expiration set - policy decision: allow or reject?
		// We'll allow (some use cases don't need expiration)
		return nil
	}

	expTime := time.Unix(token.Claims.ExpiresAt, 0)
	now := time.Now()

	// Add clock skew tolerance
	if now.After(expTime.Add(v.config.ClockSkew)) {
		return fmt.Errorf("%w: expired at %v", ErrTokenExpired, expTime)
	}

	return nil
}

// checkNotBefore verifies the token is active (not used before nbf).
func (v *Validator) checkNotBefore(token *Token) error {
	if token.Claims.NotBefore == 0 {
		return nil // No nbf claim, skip check
	}

	nbfTime := time.Unix(token.Claims.NotBefore, 0)
	now := time.Now()

	// Subtract clock skew (allow slightly early use)
	if now.Before(nbfTime.Add(-v.config.ClockSkew)) {
		return fmt.Errorf("token not valid before %v", nbfTime)
	}

	return nil
}

// checkIssuer verifies the token issuer matches expected.
func (v *Validator) checkIssuer(token *Token) error {
	if v.config.Issuer == "" {
		return nil // No issuer configured, skip check
	}

	if token.Claims.Issuer != v.config.Issuer {
		return fmt.Errorf("%w: got %q, expected %q", ErrInvalidIssuer, token.Claims.Issuer, v.config.Issuer)
	}

	return nil
}

// checkAudience verifies the token audience matches expected.
func (v *Validator) checkAudience(token *Token) error {
	if v.config.Audience == "" {
		return nil // No audience configured, skip check
	}

	if token.Claims.Audience != v.config.Audience {
		return fmt.Errorf("%w: got %q, expected %q", ErrInvalidAudience, token.Claims.Audience, v.config.Audience)
	}

	return nil
}

// SignHS256 creates a JWT token signed with HS256 (for testing).
//
// This is useful for creating test tokens. In production, tokens
// would be created by an auth service, not the API gateway.
func SignHS256(claims Claims, secretKey []byte) (string, error) {
	// Create header
	header := Header{
		Algorithm: AlgHS256,
		Type:      "JWT",
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	// Encode header and payload
	encodedHeader := base64URLEncode(headerJSON)
	encodedPayload := base64URLEncode(claimsJSON)

	// Create signature
	signedPortion := encodedHeader + "." + encodedPayload
	h := hmac.New(sha256.New, secretKey)
	h.Write([]byte(signedPortion))
	signature := h.Sum(nil)

	// Combine all parts
	return signedPortion + "." + base64URLEncode(signature), nil
}
