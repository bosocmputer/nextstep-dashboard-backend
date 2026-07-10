package auth

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashArgon2IDProducesVerifiableBoundedHash(t *testing.T) {
	hash, err := HashArgon2ID("a-production-password", bytes.NewReader(bytes.Repeat([]byte{0x42}, 16)))
	if err != nil {
		t.Fatalf("HashArgon2ID() error = %v", err)
	}
	valid, err := VerifyArgon2ID(hash, "a-production-password")
	if err != nil || !valid {
		t.Fatalf("generated hash: valid=%v err=%v", valid, err)
	}
	if _, err := HashArgon2ID("too-short", bytes.NewReader(bytes.Repeat([]byte{0x42}, 16))); err == nil {
		t.Fatal("expected short password to fail")
	}
}

func TestVerifyArgon2ID(t *testing.T) {
	hash := encodedHash("correct horse battery staple", 64*1024, 3, 2)

	valid, err := VerifyArgon2ID(hash, "correct horse battery staple")
	if err != nil || !valid {
		t.Fatalf("correct password: valid=%v err=%v", valid, err)
	}
	valid, err = VerifyArgon2ID(hash, "wrong password")
	if err != nil {
		t.Fatalf("wrong password returned error: %v", err)
	}
	if valid {
		t.Fatal("wrong password was accepted")
	}
}

func TestVerifyArgon2IDRejectsMalformedAndUnboundedHashes(t *testing.T) {
	for _, hash := range []string{
		"not-a-password-hash",
		"$argon2id$v=19$m=999999999,t=3,p=2$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=65536,t=0,p=2$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if valid, err := VerifyArgon2ID(hash, "irrelevant"); err == nil || valid {
			t.Fatalf("hash %q: valid=%v err=%v", hash, valid, err)
		}
	}
}

func encodedHash(password string, memory uint32, iterations uint32, parallelism uint8) string {
	salt := []byte("0123456789abcdef")
	digest := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		memory,
		iterations,
		parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	)
}
