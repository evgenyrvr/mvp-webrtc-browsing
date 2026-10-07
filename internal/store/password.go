package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"golang.org/x/crypto/argon2"
)

// OWASP minimum Argon2id profile; each password has a fresh 128-bit salt.
const passwordPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"

var passwordSlots = make(chan struct{}, 2)

func passwordKey(ctx context.Context, password string, salt []byte) ([]byte, error) {
	select {
	case passwordSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-passwordSlots }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	return key, ctx.Err()
}
func hashPassword(ctx context.Context, password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := passwordKey(ctx, password, salt)
	if err != nil {
		return "", err
	}
	return passwordPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func verifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	// Accept only our bounded profile: malformed DB data cannot request arbitrary
	// memory or iteration counts. Invalid/missing users still pay one KDF cost.
	parts := strings.Split(strings.TrimPrefix(encoded, passwordPrefix), "$")
	salt, expected := make([]byte, 16), make([]byte, 32)
	valid := strings.HasPrefix(encoded, passwordPrefix) && len(parts) == 2
	if valid {
		var err error
		salt, err = base64.RawStdEncoding.DecodeString(parts[0])
		valid = err == nil && len(salt) == 16
		expected, err = base64.RawStdEncoding.DecodeString(parts[1])
		valid = valid && err == nil && len(expected) == 32
	}
	if !valid {
		salt = make([]byte, 16)
		expected = make([]byte, 32)
	}
	key, err := passwordKey(ctx, password, salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(key, expected) == 1 && valid, nil
}
