package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestEncryptDecryptRoundtrip(t *testing.T) {
	kr, err := NewKeyring(map[string][]byte{"k1": key(1)}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range [][]byte{nil, []byte(""), []byte("https://hooks.slack.com/services/T000/B000/XXXX")} {
		ct, err := kr.Encrypt(pt, []byte("aad"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := kr.Decrypt(ct, []byte("aad"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("roundtrip: got %q want %q", got, pt)
		}
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	kr, _ := NewKeyring(map[string][]byte{"k1": key(1)}, "k1")
	a, _ := kr.Encrypt([]byte("same"), nil)
	b, _ := kr.Encrypt([]byte("same"), nil)
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext are identical")
	}
}

func TestRotation(t *testing.T) {
	old, _ := NewKeyring(map[string][]byte{"k1": key(1)}, "k1")
	ct, _ := old.Encrypt([]byte("secret"), nil)

	rotated, err := NewKeyring(map[string][]byte{"k1": key(1), "k2": key(2)}, "k2")
	if err != nil {
		t.Fatal(err)
	}
	got, err := rotated.Decrypt(ct, nil)
	if err != nil || string(got) != "secret" {
		t.Fatalf("decrypt with rotated keyring: %q, %v", got, err)
	}
	if needs, _ := rotated.NeedsRotation(ct); !needs {
		t.Fatal("old ciphertext should need rotation")
	}
	ct2, _ := rotated.Encrypt(got, nil)
	if id, _ := KeyID(ct2); id != "k2" {
		t.Fatalf("new ciphertext key id = %q, want k2", id)
	}
	if needs, _ := rotated.NeedsRotation(ct2); needs {
		t.Fatal("new ciphertext should not need rotation")
	}

	retired, _ := NewKeyring(map[string][]byte{"k2": key(2)}, "k2")
	if _, err := retired.Decrypt(ct, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("decrypt with retired key: err = %v, want ErrUnknownKey", err)
	}
}

func TestDecryptRejectsTamperingAndWrongAAD(t *testing.T) {
	kr, _ := NewKeyring(map[string][]byte{"k1": key(1), "k2": key(2)}, "k1")
	ct, _ := kr.Encrypt([]byte("secret"), []byte("row-1"))

	if _, err := kr.Decrypt(ct, []byte("row-2")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong aad: err = %v", err)
	}
	flipped := bytes.Clone(ct)
	flipped[len(flipped)-1] ^= 1
	if _, err := kr.Decrypt(flipped, []byte("row-1")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered: err = %v", err)
	}
	// Swapping the key id in the header must fail authentication, not decrypt.
	swapped := bytes.Clone(ct)
	copy(swapped[2:4], "k2")
	if _, err := kr.Decrypt(swapped, []byte("row-1")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("swapped key id: err = %v", err)
	}
	for _, bad := range [][]byte{nil, {0x02, 1, 'k'}, {0x01}, {0x01, 5, 'k'}, {0x01, 2, 'k', '1', 0}} {
		if _, err := kr.Decrypt(bad, nil); !errors.Is(err, ErrMalformed) {
			t.Fatalf("malformed %v: err = %v", bad, err)
		}
	}
}

func TestNewKeyringValidation(t *testing.T) {
	cases := map[string]struct {
		keys   map[string][]byte
		active string
	}{
		"empty":          {map[string][]byte{}, "k1"},
		"short key":      {map[string][]byte{"k1": make([]byte, 16)}, "k1"},
		"missing active": {map[string][]byte{"k1": key(1)}, "k2"},
		"bad key id":     {map[string][]byte{"k 1": key(1)}, "k 1"},
	}
	for name, c := range cases {
		if _, err := NewKeyring(c.keys, c.active); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
