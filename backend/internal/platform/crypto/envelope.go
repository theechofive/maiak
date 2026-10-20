// Package crypto holds the application's cryptographic primitives: AES-256-GCM envelope
// encryption for secrets stored in the database, and argon2id password hashing.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// Ciphertext layout (version 1):
//
//	0x01 | len(keyID) (1 byte) | keyID | nonce (12 bytes) | AES-256-GCM ciphertext+tag
//
// The key id is stored in clear so a value can be decrypted after the active key changes, and
// so rotation jobs can find values that still use an old key.
const (
	envelopeV1 = 0x01
	keySize    = 32
	nonceSize  = 12
	// maxKeyIDLen bounds key ids so their length always fits the one-byte header field.
	maxKeyIDLen = 32
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,` + strconv.Itoa(maxKeyIDLen) + `}$`)

var (
	// ErrUnknownKey means the ciphertext names a key id the keyring does not have.
	ErrUnknownKey = errors.New("crypto: unknown key id")
	// ErrMalformed means the ciphertext is not a valid envelope.
	ErrMalformed = errors.New("crypto: malformed ciphertext")
	// ErrDecrypt means authentication failed (wrong key, wrong associated data or tampering).
	ErrDecrypt = errors.New("crypto: decryption failed")
)

// Keyring holds all known data keys and the id of the key used for new encryptions.
type Keyring struct {
	active string
	aeads  map[string]cipher.AEAD
}

// NewKeyring builds a keyring. Every key must be 32 bytes; activeKeyID must be one of them.
func NewKeyring(keys map[string][]byte, activeKeyID string) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("crypto: no keys")
	}
	aeads := make(map[string]cipher.AEAD, len(keys))
	for id, key := range keys {
		if err := ValidateKeyID(id); err != nil {
			return nil, err
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("crypto: key %q must be %d bytes, got %d", id, keySize, len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("crypto: key %q: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("crypto: key %q: %w", id, err)
		}
		aeads[id] = aead
	}
	if _, ok := aeads[activeKeyID]; !ok {
		return nil, fmt.Errorf("crypto: active key id %q is not in the keyring", activeKeyID)
	}
	return &Keyring{active: activeKeyID, aeads: aeads}, nil
}

// ValidateKeyID checks that a key id is short and URL-safe.
func ValidateKeyID(id string) error {
	if !keyIDPattern.MatchString(id) {
		return fmt.Errorf("crypto: invalid key id %q (want 1-%d of [A-Za-z0-9_-])", id, maxKeyIDLen)
	}
	return nil
}

// ActiveKeyID returns the id of the key used by Encrypt.
func (k *Keyring) ActiveKeyID() string { return k.active }

// Encrypt seals plaintext with the active key. aad (associated data, may be nil) binds the
// ciphertext to its context, for example "notification_channels.config:<id>", so a value
// cannot be copied to another row; the same aad must be passed to Decrypt.
func (k *Keyring) Encrypt(plaintext, aad []byte) ([]byte, error) {
	idLen := len(k.active)
	if idLen == 0 || idLen > maxKeyIDLen {
		// Unreachable through NewKeyring, which validates every id.
		return nil, fmt.Errorf("crypto: active key id length %d out of range", idLen)
	}
	aead := k.aeads[k.active]
	out := make([]byte, 0, 2+idLen+nonceSize+len(plaintext)+aead.Overhead())
	out = append(out, envelopeV1, byte(idLen))
	out = append(out, k.active...)
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, headerAAD(out[:2+len(k.active)], aad)), nil
}

// Decrypt opens a ciphertext produced by Encrypt with any key in the keyring.
func (k *Keyring) Decrypt(ciphertext, aad []byte) ([]byte, error) {
	keyID, header, rest, err := parseEnvelope(ciphertext)
	if err != nil {
		return nil, err
	}
	aead, ok := k.aeads[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, keyID)
	}
	if len(rest) < nonceSize+aead.Overhead() {
		return nil, ErrMalformed
	}
	nonce, sealed := rest[:nonceSize], rest[nonceSize:]
	plaintext, err := aead.Open(nil, nonce, sealed, headerAAD(header, aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// NeedsRotation reports whether ciphertext was sealed with a key other than the active one.
func (k *Keyring) NeedsRotation(ciphertext []byte) (bool, error) {
	keyID, err := KeyID(ciphertext)
	if err != nil {
		return false, err
	}
	return keyID != k.active, nil
}

// KeyID returns the key id recorded in a ciphertext.
func KeyID(ciphertext []byte) (string, error) {
	keyID, _, _, err := parseEnvelope(ciphertext)
	return keyID, err
}

// headerAAD authenticates the envelope header together with the caller's aad, so the key id
// cannot be swapped without detection.
func headerAAD(header, aad []byte) []byte {
	out := make([]byte, 0, len(header)+len(aad))
	out = append(out, header...)
	return append(out, aad...)
}

func parseEnvelope(c []byte) (keyID string, header, rest []byte, err error) {
	if len(c) < 2 || c[0] != envelopeV1 {
		return "", nil, nil, ErrMalformed
	}
	n := int(c[1])
	if n == 0 || n > maxKeyIDLen || len(c) < 2+n {
		return "", nil, nil, ErrMalformed
	}
	return string(c[2 : 2+n]), c[:2+n], c[2+n:], nil
}
