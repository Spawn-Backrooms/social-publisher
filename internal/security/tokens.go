// Package security contains the service's cryptographic and outbound-media
// boundaries. Its errors intentionally omit credentials and signed media URLs.
package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

// RandomToken returns an unpadded, URL-safe encoding of cryptographically random
// bytes. Callers creating authorization secrets should request at least 32 bytes.
func RandomToken(nbytes int) (string, error) {
	if nbytes < 1 || nbytes > 1024 {
		return "", errors.New("invalid random token length")
	}
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("cannot generate random token")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash is suitable for storing an unguessable state or cookie token without
// retaining the original. It is not a password hashing function.
func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Challenge implements the standard PKCE S256 transformation.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
