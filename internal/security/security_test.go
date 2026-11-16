package security

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func TestVaultAuthenticationAndRotation(t *testing.T) {
	oldKey := bytes.Repeat([]byte{1}, 32)
	newKey := bytes.Repeat([]byte{2}, 32)
	oldVault, err := NewVault(map[string][]byte{"old": oldKey}, "old")
	if err != nil {
		t.Fatal(err)
	}
	const aad = "tenant\x00owner\x00tiktok\x00account"
	plain := []byte("refresh credential")
	blob, err := oldVault.Seal(plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := oldVault.Seal(plain, aad)
	if bytes.Equal(blob, second) {
		t.Fatal("fresh encryption reused its nonce")
	}
	rotated, err := NewVault(map[string][]byte{"old": oldKey, "new": newKey}, "new")
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]*Vault{"original": oldVault, "rotated": rotated} {
		got, err := candidate.Open(blob, aad)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%s could not decrypt: %v", name, err)
		}
	}
	newBlob, err := rotated.Seal(plain, aad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldVault.Open(newBlob, aad); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatal("old keyring accepted a new-key envelope")
	}
	if _, err := rotated.Open(blob, aad+"-another-owner"); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatal("ciphertext was transferable to another AAD")
	}
	for i := range blob {
		modified := append([]byte(nil), blob...)
		modified[i] ^= 1
		if _, err := rotated.Open(modified, aad); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("tampering at envelope byte %d was accepted", i)
		}
	}
	for size := 0; size < len(blob); size++ {
		if _, err := rotated.Open(blob[:size], aad); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("truncated envelope length %d was accepted", size)
		}
	}
	wrongKey, _ := NewVault(map[string][]byte{"old": newKey}, "old")
	if _, err := wrongKey.Open(blob, aad); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatal("wrong AES key was accepted")
	}
	oldKey[0] ^= 1
	if _, err := oldVault.Open(blob, aad); err != nil {
		t.Fatal("caller mutation changed the initialized vault")
	}
}

func TestVaultRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		keys   map[string][]byte
		active string
	}{
		{nil, "active"},
		{map[string][]byte{"active": make([]byte, 16)}, "active"},
		{map[string][]byte{"active": make([]byte, 32)}, "missing"},
		{map[string][]byte{"bad\x00id": make([]byte, 32)}, "bad\x00id"},
	} {
		if _, err := NewVault(tc.keys, tc.active); err == nil {
			t.Fatal("invalid vault configuration was accepted")
		}
	}
}

func signedJWT(secret []byte, header, payload string) string {
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestJWTClaimsAndAttackRejection(t *testing.T) {
	secret := bytes.Repeat([]byte{3}, 32)
	j, err := NewJWT(secret, "trusted-issuer", "publishing-api")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	principal := model.Principal{TenantID: "tenant-a", Subject: "user-a"}
	token, err := j.Mint(principal, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := j.Verify(token, now); err != nil || got != principal {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := j.Verify(token, now.Add(time.Hour)); !errors.Is(err, ErrInvalidJWT) {
		t.Fatal("token accepted at its expiration instant")
	}
	if _, err := j.Verify(token+"\n", now); !errors.Is(err, ErrInvalidJWT) {
		t.Fatal("noncanonical token encoding was accepted")
	}
	claims := map[string]any{
		"iss": "trusted-issuer", "aud": "publishing-api", "exp": now.Unix() + 3600,
		"iat": now.Unix(), "nbf": now.Unix(), "tenant_id": "tenant-a", "sub": "user-a",
	}
	const normalHeader = `{"alg":"HS256","typ":"JWT"}`
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong issuer", func(c map[string]any) { c["iss"] = "untrusted" }},
		{"wrong audience", func(c map[string]any) { c["aud"] = "other-api" }},
		{"audience type", func(c map[string]any) { c["aud"] = []any{"publishing-api", 5} }},
		{"expired", func(c map[string]any) { c["exp"] = now.Unix() }},
		{"future issued", func(c map[string]any) { c["iat"] = now.Unix() + 1 }},
		{"future not before", func(c map[string]any) { c["nbf"] = now.Unix() + 1 }},
		{"missing issued", func(c map[string]any) { delete(c, "iat") }},
		{"missing expiry", func(c map[string]any) { delete(c, "exp") }},
		{"missing not before", func(c map[string]any) { delete(c, "nbf") }},
		{"null issued", func(c map[string]any) { c["iat"] = nil }},
		{"fractional expiry", func(c map[string]any) { c["exp"] = float64(now.Unix()) + 3600.5 }},
		{"long lifetime", func(c map[string]any) { c["exp"] = now.Unix() + 86401 }},
		{"empty tenant", func(c map[string]any) { c["tenant_id"] = "" }},
		{"empty subject", func(c map[string]any) { c["sub"] = "" }},
		{"NUL tenant", func(c map[string]any) { c["tenant_id"] = "tenant\x00owner" }},
		{"oversized subject", func(c map[string]any) { c["sub"] = strings.Repeat("a", 257) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copyClaims := make(map[string]any, len(claims))
			for key, value := range claims {
				copyClaims[key] = value
			}
			tc.change(copyClaims)
			body, _ := json.Marshal(copyClaims)
			if _, err := j.Verify(signedJWT(secret, normalHeader, string(body)), now); !errors.Is(err, ErrInvalidJWT) {
				t.Fatal("invalid claims accepted")
			}
		})
	}
	body, _ := json.Marshal(claims)
	for _, header := range []string{
		`{"alg":"none","typ":"JWT"}`,
		`{"alg":"HS512","typ":"JWT"}`,
		`{"alg":"RS256","typ":"JWT"}`,
		`{"alg":"HS256","alg":"none"}`,
		`{"alg":"HS256","jku":"https://attacker.example/keys"}`,
		`{"alg":"HS256","crit":["b64"],"b64":false}`,
	} {
		if _, err := j.Verify(signedJWT(secret, header, string(body)), now); err == nil {
			t.Fatalf("unsafe JWT header accepted: %s", header)
		}
	}
	duplicateSubject := strings.TrimSuffix(string(body), "}") + `,"sub":"other-user"}`
	if _, err := j.Verify(signedJWT(secret, normalHeader, duplicateSubject), now); err == nil {
		t.Fatal("duplicate identity claim accepted")
	}
	wrongSecret := bytes.Repeat([]byte{4}, 32)
	if _, err := j.Verify(signedJWT(wrongSecret, normalHeader, string(body)), now); err == nil {
		t.Fatal("invalid signature accepted")
	}
	claims["aud"] = []string{"another-api", "publishing-api"}
	body, _ = json.Marshal(claims)
	if got, err := j.Verify(signedJWT(secret, normalHeader, string(body)), now); err != nil || got != principal {
		t.Fatal("valid audience array rejected")
	}
	for _, ttl := range []time.Duration{0, -time.Second, time.Nanosecond, 24*time.Hour + time.Second} {
		if _, err := j.Mint(principal, ttl, now); err == nil {
			t.Fatal("invalid token lifetime accepted")
		}
	}
	secret[0] ^= 1
	if _, err := j.Verify(token, now); err != nil {
		t.Fatal("caller mutation changed JWT signing key")
	}
}

func TestPKCEAndRandomToken(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	if got := Challenge(verifier); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("PKCE S256 known vector mismatch: %s", got)
	}
	first, err := RandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RandomToken(32)
	if err != nil || first == second || len(first) != 43 {
		t.Fatal("random token entropy or format failure")
	}
	if Hash("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("SHA-256 known vector mismatch")
	}
}
