package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are the argon2id cost parameters. Memory is in KiB.
type Argon2Params struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultArgon2Params: 64 MiB, 3 passes, 2 lanes (OWASP-compatible, sized for small pods).
var DefaultArgon2Params = Argon2Params{Memory: 64 * 1024, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32}

// Bounds applied when hashing and when parsing a hash, so a hostile or mistyped hash cannot make
// one verification allocate more than the api pod can hold or spin for minutes.
const (
	// MaxArgon2Memory is 256 MiB in KiB; the api runs up to two verifications at once.
	MaxArgon2Memory     = 1 << 18
	maxArgon2Iterations = 20
	minSaltLength       = 8
	maxSaltLength       = 64
	minKeyLength        = 16
	maxKeyLength        = 64
)

// ErrInvalidHash means a string is not a supported argon2id PHC hash.
var ErrInvalidHash = errors.New("crypto: invalid argon2id hash")

var b64 = base64.RawStdEncoding

// HashPassword returns an argon2id hash in PHC string format:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash> (unpadded standard base64).
func HashPassword(password string, p Argon2Params) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// ParsedHash is a decoded PHC argon2id hash.
type ParsedHash struct {
	Params Argon2Params
	Salt   []byte
	Key    []byte
}

// ParseArgon2idHash decodes and bounds-checks a PHC argon2id string.
func ParseArgon2idHash(encoded string) (*ParsedHash, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, ErrInvalidHash
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return nil, fmt.Errorf("%w: unsupported version %q", ErrInvalidHash, parts[2])
	}
	var p Argon2Params
	seen := map[string]bool{}
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || seen[k] {
			return nil, ErrInvalidHash
		}
		seen[k] = true
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, ErrInvalidHash
		}
		switch k {
		case "m":
			p.Memory = uint32(n) //nolint:gosec // n < 2^32 (ParseUint bitSize 32)
		case "t":
			p.Iterations = uint32(n) //nolint:gosec // n < 2^32 (ParseUint bitSize 32)
		case "p":
			if n > 255 {
				return nil, ErrInvalidHash
			}
			p.Parallelism = uint8(n) //nolint:gosec // checked <= 255 above
		default:
			return nil, fmt.Errorf("%w: unknown parameter %q", ErrInvalidHash, k)
		}
	}
	if !seen["m"] || !seen["t"] || !seen["p"] {
		return nil, ErrInvalidHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return nil, ErrInvalidHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return nil, ErrInvalidHash
	}
	p.SaltLength = uint32(len(salt)) //nolint:gosec // bounded by the input length; validate() caps it
	p.KeyLength = uint32(len(key))   //nolint:gosec // as above
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}
	return &ParsedHash{Params: p, Salt: salt, Key: key}, nil
}

// VerifyPassword reports whether password matches the PHC hash. The cost parameters are taken
// from the hash, so hashes made with older parameters keep working.
func VerifyPassword(password, encoded string) (bool, error) {
	h, err := ParseArgon2idHash(encoded)
	if err != nil {
		return false, err
	}
	return h.Verify(password), nil
}

// Verify compares password against the parsed hash in constant time.
func (h *ParsedHash) Verify(password string) bool {
	key := argon2.IDKey([]byte(password), h.Salt, h.Params.Iterations, h.Params.Memory, h.Params.Parallelism, h.Params.KeyLength)
	return subtle.ConstantTimeCompare(key, h.Key) == 1
}

func (p Argon2Params) validate() error {
	switch {
	case p.Parallelism < 1:
		return errors.New("crypto: argon2 parallelism must be >= 1")
	case p.Memory < 8*uint32(p.Parallelism) || p.Memory > MaxArgon2Memory:
		return fmt.Errorf("crypto: argon2 memory must be between %d and %d KiB", 8*uint32(p.Parallelism), MaxArgon2Memory)
	case p.Iterations < 1 || p.Iterations > maxArgon2Iterations:
		return fmt.Errorf("crypto: argon2 iterations must be between 1 and %d", maxArgon2Iterations)
	case p.SaltLength < minSaltLength || p.SaltLength > maxSaltLength:
		return fmt.Errorf("crypto: argon2 salt length must be between %d and %d", minSaltLength, maxSaltLength)
	case p.KeyLength < minKeyLength || p.KeyLength > maxKeyLength:
		return fmt.Errorf("crypto: argon2 key length must be between %d and %d", minKeyLength, maxKeyLength)
	}
	return nil
}
