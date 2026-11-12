package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func newKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSignVerify(t *testing.T) {
	pub, priv := newKeys(t)
	now := time.Unix(1_800_000_000, 0)
	l := License{ID: "id1", Licensee: "Acme", Plan: "pro", Features: []string{FeatureUsers, FeatureAudit}, Issued: now.Unix(), Expires: now.Add(time.Hour).Unix()}
	key, err := Sign(l, priv)
	if err != nil {
		t.Fatal(err)
	}
	p, s, ok := strings.Cut(key, ".")
	if !ok || strings.ContainsAny(key, "+/=") {
		t.Fatalf("key %q", key)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(p)
	if string(payload) != `{"id":"id1","licensee":"Acme","plan":"pro","features":["users","audit"],"issued":1800000000,"expires":1800003600}` {
		t.Fatalf("payload %s", payload)
	}
	got, err := Verify("  "+key+"\n", pub)
	if err != nil || got.Licensee != "Acme" || len(got.Features) != 2 {
		t.Fatalf("verify %+v %v", got, err)
	}

	k := Check(key, pub)
	if k.Status(now) != "valid" || !k.Has(FeatureUsers, now) || k.Has(FeatureConfigEdit, now) {
		t.Fatalf("status %s", k.Status(now))
	}
	later := now.Add(time.Hour)
	if k.Status(later) != "expired" || k.Has(FeatureUsers, later) {
		t.Fatal("expiry")
	}
	if l, ok := k.License(); !ok || l.ID != "id1" {
		t.Fatal("expired licence is still readable")
	}

	// Perpetual.
	l.Expires = 0
	forever, _ := Sign(l, priv)
	if k := Check(forever, pub); k.Status(now.Add(100*365*24*time.Hour)) != "valid" {
		t.Fatal("perpetual licence expired")
	}

	// Tampering, another key, garbage.
	otherPub, otherPriv := newKeys(t)
	forged, _ := Sign(License{Licensee: "Acme", Features: []string{FeatureUsers}}, otherPriv)
	tamperedPayload := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(payload), "Acme", "Evil", 1)))
	for name, bad := range map[string]string{
		"forged":    forged,
		"tampered":  tamperedPayload + "." + s,
		"no dot":    p + s,
		"bad b64":   "!!." + s,
		"short sig": p + ".AAAA",
		"empty sig": p + ".",
		"not json":  "",
	} {
		k := Check(bad, pub)
		want := "invalid"
		if bad == "" {
			want = "none"
		}
		if k.Status(now) != want || k.Has(FeatureUsers, now) {
			t.Errorf("%s: %s %v", name, k.Status(now), k.Err())
		}
	}
	if Check(key, otherPub).Status(now) != "invalid" {
		t.Error("verified with the wrong public key")
	}
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("nope"))
	if k := Check(notJSON+"."+base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte("nope"))), pub); k.Status(now) != "invalid" {
		t.Error("non-JSON payload accepted")
	}
	backwards, _ := Sign(License{Licensee: "x", Issued: 10, Expires: 5}, priv)
	if Check(backwards, pub).Status(now) != "invalid" {
		t.Error("expires before issued accepted")
	}
	if (Key{}).Status(now) != "none" {
		t.Error("zero key")
	}
}

func TestKeyParsing(t *testing.T) {
	pub, priv := newKeys(t)
	if p, err := ParsePublicKey(base64.StdEncoding.EncodeToString(pub) + "\n"); err != nil || !p.Equal(pub) {
		t.Fatal(err)
	}
	if k, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(priv)); err != nil || !k.Equal(priv) {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "AAAA", base64.StdEncoding.EncodeToString(priv)} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("public %q", bad)
		}
	}
	if _, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(pub)); err == nil {
		t.Error("public key as private")
	}
	if _, err := Sign(License{}, priv[:10]); err == nil {
		t.Error("short private key")
	}
	if _, err := Verify("a.b", pub[:3]); err == nil {
		t.Error("short public key")
	}
}

func TestEmbeddedKey(t *testing.T) {
	if len(publicKey) != ed25519.PublicKeySize {
		t.Fatal("embedded public key")
	}
	// A key signed by anything but the embedded key's owner is invalid.
	_, priv := newKeys(t)
	key, _ := Sign(License{Licensee: "x", Features: []string{FeatureUsers}}, priv)
	if Load(key).Status(time.Now()) != "invalid" || Load("").Status(time.Now()) != "none" {
		t.Fatal("Load")
	}
}
