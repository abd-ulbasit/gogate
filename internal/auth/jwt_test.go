package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Test secret key for HS256
var testSecretKey = []byte("super-secret-key-for-testing-only")

// testSignHS256 creates a signed HS256 token for testing.
// This is a helper that duplicates SignHS256 logic for tests.
func testSignHS256(claims Claims, key []byte) (string, error) {
	// Build header
	header := `{"alg":"HS256","typ":"JWT"}`
	encodedHeader := base64URLEncode([]byte(header))

	// Build payload
	payload := fmt.Sprintf(`{"sub":"%s"`, claims.Subject)
	if claims.ExpiresAt != 0 {
		payload += fmt.Sprintf(`,"exp":%d`, claims.ExpiresAt)
	}
	if claims.Issuer != "" {
		payload += fmt.Sprintf(`,"iss":"%s"`, claims.Issuer)
	}
	if claims.Audience != "" {
		payload += fmt.Sprintf(`,"aud":"%s"`, claims.Audience)
	}
	payload += "}"

	encodedPayload := base64URLEncode([]byte(payload))
	signedPortion := encodedHeader + "." + encodedPayload

	// Sign with HMAC-SHA256
	h := hmac.New(sha256.New, key)
	h.Write([]byte(signedPortion))
	signature := base64URLEncode(h.Sum(nil))

	return signedPortion + "." + signature, nil
}

func TestParseValidToken(t *testing.T) {
	// Create a valid test token
	claims := Claims{
		Subject:   "user123",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Issuer:    "test-issuer",
	}

	tokenString, err := testSignHS256(claims, testSecretKey)
	if err != nil {
		t.Fatalf("testSignHS256: %v", err)
	}

	// Parse the token
	token, err := Parse(tokenString)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Verify claims
	if token.Claims.Subject != "user123" {
		t.Errorf("Subject = %s, want user123", token.Claims.Subject)
	}
	if token.Claims.Issuer != "test-issuer" {
		t.Errorf("Issuer = %s, want test-issuer", token.Claims.Issuer)
	}
	if token.Header.Algorithm != AlgHS256 {
		t.Errorf("Algorithm = %s, want HS256", token.Header.Algorithm)
	}
}

func TestParseInvalidToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"one part", "abc"},
		{"two parts", "abc.def"},
		{"invalid base64 header", "!!!.abc.def"},
		{"invalid json header", "YWJj.abc.def"}, // "abc" base64 encoded
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.token)
			if err == nil {
				t.Error("expected error for invalid token")
			}
		})
	}
}

func TestValidatorHS256(t *testing.T) {
	validator := NewValidator(ValidatorConfig{
		SecretKey: testSecretKey,
		Issuer:    "test-issuer",
		Audience:  "test-audience",
	})

	// Create valid token
	claims := Claims{
		Subject:   "user123",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Issuer:    "test-issuer",
		Audience:  "test-audience",
	}

	tokenString, err := testSignHS256(claims, testSecretKey)
	if err != nil {
		t.Fatalf("testSignHS256: %v", err)
	}

	// Validate
	token, err := validator.Validate(tokenString)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if token.Claims.Subject != "user123" {
		t.Errorf("Subject = %s, want user123", token.Claims.Subject)
	}
}

func TestValidatorExpiredToken(t *testing.T) {
	validator := NewValidator(ValidatorConfig{
		SecretKey: testSecretKey,
	})

	// Create expired token
	claims := Claims{
		Subject:   "user123",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(), // Expired 1 hour ago
	}

	tokenString, err := testSignHS256(claims, testSecretKey)
	if err != nil {
		t.Fatalf("testSignHS256: %v", err)
	}

	// Should fail validation
	_, err = validator.Validate(tokenString)
	if err == nil {
		t.Error("expected error for expired token")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error should mention 'expired': %v", err)
	}
}

func TestValidatorClockSkew(t *testing.T) {
	validator := NewValidator(ValidatorConfig{
		SecretKey: testSecretKey,
		ClockSkew: 5 * time.Minute, // Allow 5 minutes of clock skew
	})

	// Create token that expired 1 minute ago (should pass with 5 min skew)
	claims := Claims{
		Subject:   "user123",
		ExpiresAt: time.Now().Add(-1 * time.Minute).Unix(),
	}

	tokenString, err := testSignHS256(claims, testSecretKey)
	if err != nil {
		t.Fatalf("testSignHS256: %v", err)
	}

	// Should pass with clock skew tolerance
	_, err = validator.Validate(tokenString)
	if err != nil {
		t.Errorf("token should be valid with clock skew: %v", err)
	}
}

func TestValidatorInvalidIssuer(t *testing.T) {
	validator := NewValidator(ValidatorConfig{
		SecretKey: testSecretKey,
		Issuer:    "expected-issuer",
	})

	// Create token with wrong issuer
	claims := Claims{
		Subject:   "user123",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Issuer:    "wrong-issuer",
	}

	tokenString, err := testSignHS256(claims, testSecretKey)
	if err != nil {
		t.Fatalf("testSignHS256: %v", err)
	}

	// Should fail validation
	_, err = validator.Validate(tokenString)
	if err == nil {
		t.Error("expected error for invalid issuer")
	}
	if !strings.Contains(err.Error(), "issuer") {
		t.Errorf("error should mention 'issuer': %v", err)
	}
}

func TestValidatorInvalidAudience(t *testing.T) {
	validator := NewValidator(ValidatorConfig{
		SecretKey: testSecretKey,
		Audience:  "expected-audience",
	})

	// Create token with wrong audience
	claims := Claims{
		Subject:   "user123",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Audience:  "wrong-audience",
	}

	tokenString, err := testSignHS256(claims, testSecretKey)
	if err != nil {
		t.Fatalf("testSignHS256: %v", err)
	}

	// Should fail validation
	_, err = validator.Validate(tokenString)
	if err == nil {
		t.Error("expected error for invalid audience")
	}
	if !strings.Contains(err.Error(), "audience") {
		t.Errorf("error should mention 'audience': %v", err)
	}
}

func TestValidatorInvalidSignature(t *testing.T) {
	validator := NewValidator(ValidatorConfig{
		SecretKey: testSecretKey,
	})

	// Create token with different key
	claims := Claims{
		Subject:   "user123",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}

	tokenString, err := testSignHS256(claims, []byte("wrong-secret-key"))
	if err != nil {
		t.Fatalf("testSignHS256: %v", err)
	}

	// Should fail validation
	_, err = validator.Validate(tokenString)
	if err == nil {
		t.Error("expected error for invalid signature")
	}
	if err != ErrInvalidSignature {
		t.Errorf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestValidatorNoExpirationAllowed(t *testing.T) {
	validator := NewValidator(ValidatorConfig{
		SecretKey: testSecretKey,
	})

	// Create token without expiration
	claims := Claims{
		Subject: "user123",
		// No ExpiresAt
	}

	tokenString, err := testSignHS256(claims, testSecretKey)
	if err != nil {
		t.Fatalf("testSignHS256: %v", err)
	}

	// Should pass (we allow tokens without expiration by policy)
	_, err = validator.Validate(tokenString)
	if err != nil {
		t.Errorf("token without expiration should be valid: %v", err)
	}
}

func TestCustomClaims(t *testing.T) {
	// Create token with custom claims manually
	// (testSignHS256 doesn't support custom claims, so build manually)
	header := `{"alg":"HS256","typ":"JWT"}`
	payload := fmt.Sprintf(
		`{"sub":"user123","exp":%d,"roles":["admin","user"],"tenant_id":"acme"}`,
		time.Now().Add(time.Hour).Unix(),
	)

	encodedHeader := base64URLEncode([]byte(header))
	encodedPayload := base64URLEncode([]byte(payload))
	signedPortion := encodedHeader + "." + encodedPayload

	// Sign with HMAC-SHA256
	h := hmac.New(sha256.New, testSecretKey)
	h.Write([]byte(signedPortion))
	signature := base64URLEncode(h.Sum(nil))

	tokenString := signedPortion + "." + signature

	token, err := Parse(tokenString)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Check custom claims were extracted
	if token.Claims.Custom == nil {
		t.Fatal("Custom claims should not be nil")
	}

	roles, ok := token.Claims.Custom["roles"]
	if !ok {
		t.Error("Custom claim 'roles' not found")
	}

	// roles is a []interface{} when unmarshaled from JSON
	roleList, ok := roles.([]interface{})
	if !ok {
		t.Errorf("roles should be []interface{}, got %T", roles)
	}

	if len(roleList) != 2 {
		t.Errorf("expected 2 roles, got %d", len(roleList))
	}

	// Check tenant_id custom claim
	tenantID, ok := token.Claims.Custom["tenant_id"]
	if !ok {
		t.Error("Custom claim 'tenant_id' not found")
	}
	if tenantID != "acme" {
		t.Errorf("tenant_id = %v, want 'acme'", tenantID)
	}
}

func TestBase64URLEncodeDecode(t *testing.T) {
	// Test round-trip encoding/decoding
	tests := []struct {
		name string
		data string
	}{
		{"simple", "hello"},
		{"with special", "hello+world/test"},
		{"json", `{"alg":"HS256"}`},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := base64URLEncode([]byte(tt.data))
			decoded, err := base64URLDecode(encoded)
			if err != nil {
				t.Fatalf("base64URLDecode: %v", err)
			}
			if string(decoded) != tt.data {
				t.Errorf("round-trip failed: got %s, want %s", decoded, tt.data)
			}
		})
	}
}
