// Package license signs and verifies fsdeck licence keys. A key is
// base64url(payload) + "." + base64url(signature), where the payload is JSON
// and the signature is Ed25519 over the payload bytes. Verification is
// offline, against the public key built into fsdeck.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Paid features a licence can unlock.
const (
	FeatureUsers      = "users"
	FeatureAudit      = "audit"
	FeatureConfigEdit = "config-edit"
)

// Features lists every feature name.
var Features = []string{FeatureUsers, FeatureAudit, FeatureConfigEdit}

// License is the signed payload.
type License struct {
	ID       string   `json:"id"`
	Licensee string   `json:"licensee"`
	Plan     string   `json:"plan"`
	Features []string `json:"features"`
	Issued   int64    `json:"issued"`  // unix seconds
	Expires  int64    `json:"expires"` // unix seconds; 0 never expires
}

// Expired reports whether l has expired at now.
func (l License) Expired(now time.Time) bool {
	return l.Expires != 0 && !now.Before(time.Unix(l.Expires, 0))
}

var b64 = base64.RawURLEncoding

// Sign returns the key for l.
func Sign(l License, priv ed25519.PrivateKey) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("license: the private key must be 64 bytes")
	}
	payload, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(ed25519.Sign(priv, payload)), nil
}

// Verify checks the signature of key against pub and decodes the payload.
// It does not look at the dates.
func Verify(key string, pub ed25519.PublicKey) (License, error) {
	if len(pub) != ed25519.PublicKeySize {
		return License{}, errors.New("license: the public key must be 32 bytes")
	}
	p, s, ok := strings.Cut(strings.TrimSpace(key), ".")
	if !ok {
		return License{}, errors.New("license: malformed key")
	}
	payload, err := b64.DecodeString(p)
	if err != nil {
		return License{}, errors.New("license: malformed key")
	}
	sig, err := b64.DecodeString(s)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return License{}, errors.New("license: malformed key")
	}
	if !ed25519.Verify(pub, payload, sig) {
		return License{}, errors.New("license: the signature does not match")
	}
	var l License
	if err := json.Unmarshal(payload, &l); err != nil {
		return License{}, fmt.Errorf("license: bad payload: %v", err)
	}
	if l.Expires != 0 && l.Expires < l.Issued {
		return License{}, errors.New("license: expires before it was issued")
	}
	if l.Features == nil {
		l.Features = []string{}
	}
	return l, nil
}

// ParsePublicKey decodes a standard base64 Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("license: the public key must be 32 bytes of standard base64")
	}
	return ed25519.PublicKey(b), nil
}

// ParsePrivateKey decodes a standard base64 Ed25519 private key (64 bytes).
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("license: the private key must be 64 bytes of standard base64")
	}
	return ed25519.PrivateKey(b), nil
}

// Key is the licence fsdeck runs with: none, one that failed verification,
// or a verified one that may since have expired.
type Key struct {
	lic   *License
	given bool
	err   error
}

// Load verifies key against the public key built into fsdeck. An empty key
// is no licence.
func Load(key string) Key { return Check(key, publicKey) }

// Check verifies key against pub.
func Check(key string, pub ed25519.PublicKey) Key {
	if strings.TrimSpace(key) == "" {
		return Key{}
	}
	l, err := Verify(key, pub)
	if err != nil {
		return Key{given: true, err: err}
	}
	return Key{given: true, lic: &l}
}

// Invalid is a key that could not be read; err says why.
func Invalid(err error) Key { return Key{given: true, err: err} }

// Status is "none", "invalid", "expired" or "valid".
func (k Key) Status(now time.Time) string {
	switch {
	case !k.given:
		return "none"
	case k.lic == nil:
		return "invalid"
	case k.lic.Expired(now):
		return "expired"
	}
	return "valid"
}

// Has reports whether a valid licence grants feature at now.
func (k Key) Has(feature string, now time.Time) bool {
	return k.Status(now) == "valid" && slices.Contains(k.lic.Features, feature)
}

// License returns the verified payload, also when it has expired.
func (k Key) License() (License, bool) {
	if k.lic == nil {
		return License{}, false
	}
	return *k.lic, true
}

// Err says why a given key is invalid.
func (k Key) Err() error { return k.err }
