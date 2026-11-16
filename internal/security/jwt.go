package security

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"socialpublisher/internal/model"
)

var ErrInvalidJWT = errors.New("authentication token is invalid or expired")

const (
	maxJWTLength   = 8192
	maxIdentityLen = 256
	maxJWTLifetime = 24 * time.Hour
)

// JWT signs and verifies only this service's HS256 tokens. A trusted backend
// establishes Principal; tenant or subject values supplied in HTTP headers are
// not an authentication mechanism.
type JWT struct {
	secret   []byte
	issuer   string
	audience string
}

func NewJWT(secret []byte, issuer, audience string) (*JWT, error) {
	if len(secret) < 32 || !boundedText(issuer, 2048) || !boundedText(audience, 2048) {
		return nil, errors.New("JWT requires a strong secret, issuer, and audience")
	}
	return &JWT{secret: append([]byte(nil), secret...), issuer: issuer, audience: audience}, nil
}

func boundedText(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func validPrincipal(p model.Principal) bool {
	return boundedText(p.TenantID, maxIdentityLen) && boundedText(p.Subject, maxIdentityLen)
}

func (j *JWT) Mint(principal model.Principal, ttl time.Duration, now time.Time) (string, error) {
	if j == nil || !validPrincipal(principal) || ttl < time.Second || ttl > maxJWTLifetime || now.Unix() < 0 {
		return "", ErrInvalidJWT
	}
	issued, expires := now.Unix(), now.Add(ttl).Unix()
	if expires <= issued || expires-issued > int64(maxJWTLifetime/time.Second) {
		return "", ErrInvalidJWT
	}
	header := []byte(`{"alg":"HS256","typ":"JWT"}`)
	claims := struct {
		Issuer    string `json:"iss"`
		Audience  string `json:"aud"`
		Expires   int64  `json:"exp"`
		Issued    int64  `json:"iat"`
		NotBefore int64  `json:"nbf"`
		Tenant    string `json:"tenant_id"`
		Subject   string `json:"sub"`
	}{j.issuer, j.audience, expires, issued, issued, principal.TenantID, principal.Subject}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", ErrInvalidJWT
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, j.secret)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify requires exp, iat and nbf as integral NumericDate values, applies no
// clock-skew allowance, and rejects lifetimes longer than 24 hours. Audience may
// be a string or a nonempty array containing the configured audience.
func (j *JWT) Verify(raw string, now time.Time) (model.Principal, error) {
	invalid := func() (model.Principal, error) { return model.Principal{}, ErrInvalidJWT }
	if j == nil || len(raw) == 0 || len(raw) > maxJWTLength || strings.Count(raw, ".") != 2 {
		return invalid()
	}
	for _, c := range raw {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return invalid()
		}
	}
	parts := strings.Split(raw, ".")
	encoding := base64.RawURLEncoding.Strict()
	signature, err := encoding.DecodeString(parts[2])
	if err != nil || len(signature) != sha256.Size {
		return invalid()
	}
	mac := hmac.New(sha256.New, j.secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return invalid()
	}
	headerBytes, err := encoding.DecodeString(parts[0])
	if err != nil {
		return invalid()
	}
	header, err := jsonObject(headerBytes)
	if err != nil || len(header) == 0 || len(header) > 2 {
		return invalid()
	}
	for key := range header {
		if key != "alg" && key != "typ" {
			return invalid()
		}
	}
	var algorithm string
	if json.Unmarshal(header["alg"], &algorithm) != nil || algorithm != "HS256" {
		return invalid()
	}
	if typ, exists := header["typ"]; exists {
		var value string
		if json.Unmarshal(typ, &value) != nil || value != "JWT" {
			return invalid()
		}
	}
	payload, err := encoding.DecodeString(parts[1])
	if err != nil {
		return invalid()
	}
	claims, err := jsonObject(payload)
	if err != nil {
		return invalid()
	}
	var issuer, tenant, subject string
	if json.Unmarshal(claims["iss"], &issuer) != nil || issuer != j.issuer ||
		json.Unmarshal(claims["tenant_id"], &tenant) != nil || json.Unmarshal(claims["sub"], &subject) != nil ||
		!validPrincipal(model.Principal{TenantID: tenant, Subject: subject}) || !j.validAudience(claims["aud"]) {
		return invalid()
	}
	expires, okExp := integerClaim(claims, "exp")
	issued, okIat := integerClaim(claims, "iat")
	notBefore, okNbf := integerClaim(claims, "nbf")
	current := now.Unix()
	if !okExp || !okIat || !okNbf || issued < 0 || notBefore < 0 || expires <= 0 ||
		expires <= current || issued > current || notBefore > current || expires <= issued || expires <= notBefore ||
		expires-issued > int64(maxJWTLifetime/time.Second) {
		return invalid()
	}
	return model.Principal{TenantID: tenant, Subject: subject}, nil
}

func integerClaim(claims map[string]json.RawMessage, key string) (int64, bool) {
	value, exists := claims[key]
	if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return 0, false
	}
	var number int64
	if json.Unmarshal(value, &number) != nil {
		return 0, false
	}
	return number, true
}

func (j *JWT) validAudience(raw json.RawMessage) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == j.audience
	}
	var audience []string
	if json.Unmarshal(raw, &audience) != nil || len(audience) == 0 || len(audience) > 32 {
		return false
	}
	found := false
	for _, item := range audience {
		if !boundedText(item, 2048) {
			return false
		}
		found = found || item == j.audience
	}
	return found
}

// encoding/json normally accepts duplicate keys. Reject them so no alternate
// parser can attribute a signed request to a different identity or algorithm.
func jsonObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrInvalidJWT
	}
	result := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, ErrInvalidJWT
		}
		name, ok := key.(string)
		if !ok {
			return nil, ErrInvalidJWT
		}
		if _, exists := result[name]; exists {
			return nil, ErrInvalidJWT
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, ErrInvalidJWT
		}
		result[name] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, ErrInvalidJWT
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, ErrInvalidJWT
	}
	return result, nil
}
