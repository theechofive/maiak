package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

const tokenBytes = 32

// newToken returns a random 32-byte token, URL-safe base64 without padding.
func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken returns the SHA-256 of a session token. Only the hash is stored, so a database
// leak does not yield usable sessions. A slow hash is unnecessary: the token has 256 bits.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// validTokenFormat rejects obviously malformed cookies before touching the database.
func validTokenFormat(token string) bool {
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == tokenBytes
}

// CSRFValid compares the session's CSRF token with the one sent by the client in constant time.
// An empty expected token never matches.
func CSRFValid(expected, got string) bool {
	if expected == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}
