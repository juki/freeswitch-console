package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/juki/fsdeck/internal/store"
)

// pbkdf2Iterations is the work factor of new hashes. Tests lower it; a
// stored hash carries its own count, so raising it later keeps old hashes
// valid.
var pbkdf2Iterations = 600000

const hashScheme = "pbkdf2-sha256"

var b64 = base64.RawStdEncoding

// hashPassword returns pbkdf2-sha256$<iterations>$<salt>$<key> with a 16-byte
// random salt and a 32-byte key, both unpadded standard base64.
func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, pbkdf2Iterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword compares in constant time. A malformed hash never matches.
func verifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false
	}
	// The upper bound keeps a hand-edited file from stalling every sign-in.
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err := b64.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func checkPassword(password string) error {
	if n := utf8.RuneCountInString(password); n < 10 || n > 256 {
		return errors.New("password must be 10 to 256 characters")
	}
	return nil
}

// randomPassword is 24 characters from 18 random bytes.
func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Bootstrap creates the account admin with a random password when the store
// has no accounts and no admin password comes from the environment. It
// returns that password, to be shown once, or "" when nothing was created.
func Bootstrap(st *store.Store, envAdmin bool) (string, error) {
	if envAdmin || len(st.Users()) > 0 {
		return "", nil
	}
	password, err := randomPassword()
	if err != nil {
		return "", err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return "", err
	}
	if _, err := st.AddUser(store.User{Username: "admin", Role: store.RoleAdmin, PasswordHash: hash}); err != nil {
		return "", err
	}
	return password, nil
}
