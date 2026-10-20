package dbbootstrap

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
)

const passwordBytes = 32

// GeneratePassword returns 32 random bytes as URL-safe base64 without padding.
func GeneratePassword() (string, error) {
	b := make([]byte, passwordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("dbbootstrap: random password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// RoleURL builds the connection URL stored in the Secret. User and password are
// percent-escaped by net/url, so any password survives the round trip.
func RoleURL(role, password, host string, port int, dbName, sslMode string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + dbName,
		RawQuery: url.Values{"sslmode": {sslMode}}.Encode(),
	}
	return u.String()
}

// scramIterations matches the PostgreSQL default (scram_iterations).
const scramIterations = 4096

// scramVerifier returns the SCRAM-SHA-256 verifier PostgreSQL stores for password (the format
// of pg_authid.rolpassword). Sending the verifier instead of the password keeps the plaintext
// out of server logs and pg_stat_statements. SASLprep is the identity for printable ASCII, so
// only such passwords are accepted.
func scramVerifier(password string, salt []byte) (string, error) {
	if password == "" {
		return "", errors.New("dbbootstrap: empty password")
	}
	for _, c := range []byte(password) {
		if c < 0x21 || c > 0x7e {
			return "", errors.New("dbbootstrap: password must be printable ASCII")
		}
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("dbbootstrap: pbkdf2: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	enc := base64.StdEncoding
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		scramIterations, enc.EncodeToString(salt), enc.EncodeToString(storedKey[:]), enc.EncodeToString(serverKey)), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func newSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("dbbootstrap: salt: %w", err)
	}
	return salt, nil
}
