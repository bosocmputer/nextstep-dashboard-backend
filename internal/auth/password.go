package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	minimumArgonMemory      = 8 * 1024
	maximumArgonMemory      = 1024 * 1024
	maximumArgonIterations  = 10
	maximumArgonParallelism = 16
)

func HashArgon2ID(password string, entropy io.Reader) (string, error) {
	if len(password) < 14 || len(password) > 1024 {
		return "", errors.New("password length must be between 14 and 1024 bytes")
	}
	if entropy == nil {
		return "", errors.New("password hashing entropy source is required")
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(entropy, salt); err != nil {
		return "", errors.New("generate password salt")
	}
	const (
		memory      = uint32(64 * 1024)
		iterations  = uint32(3)
		parallelism = uint8(2)
		keyLength   = uint32(32)
	)
	digest := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, keyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		memory,
		iterations,
		parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

func VerifyArgon2ID(encodedHash, password string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("password hash is not a valid Argon2id encoding")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("password hash uses an unsupported Argon2id version")
	}
	var memory, iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false, errors.New("password hash has invalid Argon2id parameters")
	}
	if memory < minimumArgonMemory || memory > maximumArgonMemory || iterations == 0 || iterations > maximumArgonIterations || parallelism == 0 || parallelism > maximumArgonParallelism {
		return false, errors.New("password hash Argon2id parameters are outside safe bounds")
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false, errors.New("password hash has an invalid Argon2id salt")
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return false, errors.New("password hash has an invalid Argon2id digest")
	}

	actual := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}
