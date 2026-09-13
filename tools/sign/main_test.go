package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// The signature must be the raw 64-byte PureEdDSA output — exactly what
// `openssl pkeyutl -sign -rawin` emits and what install.sh verifies.
func TestSignRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	pubDER, _ := x509.MarshalPKIXPublicKey(pub)
	keyPath := filepath.Join(dir, "key.pem")
	pubPath := filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := []byte("abc  origamy_linux_amd64\n")
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, msg, 0o644); err != nil {
		t.Fatal(err)
	}

	gotPriv, err := loadPrivate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(gotPriv, msg)
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	gotPub, err := loadPublic(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(gotPub, msg, sig) {
		t.Fatal("round trip failed")
	}
	msg[0] ^= 1
	if ed25519.Verify(gotPub, msg, sig) {
		t.Fatal("tampered message must not verify")
	}
}

func TestRejectsNonEd25519(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rsa.pem")
	// A PKCS#8 header that is not Ed25519 (RSA OID) must be refused, not misused.
	if err := os.WriteFile(p, []byte("-----BEGIN PRIVATE KEY-----\nMIIBVQIBADANBgkqhkiG9w0BAQEFAASCAT8=\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPrivate(p); err == nil {
		t.Fatal("expected an error for a non-Ed25519 key")
	}
}
