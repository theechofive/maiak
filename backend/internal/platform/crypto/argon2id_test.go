package crypto

import (
	"errors"
	"strings"
	"testing"
)

// Cheap parameters keep the tests fast; production defaults are exercised once.
var testParams = Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("unexpected PHC prefix: %s", h)
	}
	if ok, err := VerifyPassword("correct horse", h); err != nil || !ok {
		t.Fatalf("verify correct: ok=%v err=%v", ok, err)
	}
	if ok, err := VerifyPassword("wrong", h); err != nil || ok {
		t.Fatalf("verify wrong: ok=%v err=%v", ok, err)
	}
}

func TestDefaultParamsEncodedInHash(t *testing.T) {
	h, err := HashPassword("pw", DefaultArgon2Params)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseArgon2idHash(h)
	if err != nil {
		t.Fatal(err)
	}
	if p.Params != DefaultArgon2Params {
		t.Fatalf("params = %+v, want %+v", p.Params, DefaultArgon2Params)
	}
	if !strings.Contains(h, "$m=65536,t=3,p=2$") {
		t.Fatalf("hash does not carry default params: %s", h)
	}
}

func TestVerifyUsesParamsFromHash(t *testing.T) {
	// A hash produced with other parameters must still verify.
	h, _ := HashPassword("pw", Argon2Params{Memory: 128, Iterations: 2, Parallelism: 2, SaltLength: 8, KeyLength: 16})
	if ok, err := VerifyPassword("pw", h); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestParseRejectsInvalidHashes(t *testing.T) {
	valid, _ := HashPassword("pw", testParams)
	parts := strings.Split(valid, "$")
	salt, key := parts[4], parts[5]
	cases := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=16$m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=1,x=2$" + salt + "$" + key,
		"$argon2id$v=19$m=64,m=64,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=abc,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=4294967295,t=1,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=262145,t=1,p=1$" + salt + "$" + key, // 256 MiB + 1 KiB
		"$argon2id$v=19$m=64,t=1000,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=0$" + salt + "$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$!!!$" + key,
		"$argon2id$v=19$m=64,t=1,p=1$" + salt + "$" + key[:4],
		"$argon2id$v=19$m=64,t=1,p=1$" + salt,
	}
	for _, c := range cases {
		if _, err := ParseArgon2idHash(c); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("ParseArgon2idHash(%q) err = %v, want ErrInvalidHash", c, err)
		}
	}
}

func TestMemoryCap(t *testing.T) {
	p := testParams
	p.Memory = MaxArgon2Memory + 1
	if _, err := HashPassword("pw", p); err == nil {
		t.Fatal("HashPassword accepted memory above 256 MiB")
	}
	valid, _ := HashPassword("pw", testParams)
	parts := strings.Split(valid, "$")
	atCap := "$argon2id$v=19$m=262144,t=1,p=1$" + parts[4] + "$" + parts[5]
	if _, err := ParseArgon2idHash(atCap); err != nil {
		t.Fatalf("256 MiB exactly must parse: %v", err)
	}
}
