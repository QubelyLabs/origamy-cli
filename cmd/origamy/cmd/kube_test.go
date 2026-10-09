package cmd

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSecretManifest(t *testing.T) {
	tok := "dpt_secret-value"
	key := "-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----\n"
	m := secretManifest("origamy-dp", "origamy-byod-identity", map[string]string{
		"tls.key":    key,
		"auth-token": tok,
	})
	// Plaintext never appears: everything is under data: as base64.
	if strings.Contains(m, tok) || strings.Contains(m, "BEGIN EC") {
		t.Fatalf("plaintext secret in manifest:\n%s", m)
	}
	for _, want := range []string{
		"kind: Secret", "type: Opaque", "name: origamy-byod-identity", "namespace: origamy-dp",
		"  auth-token: " + base64.StdEncoding.EncodeToString([]byte(tok)),
		"  tls.key: " + base64.StdEncoding.EncodeToString([]byte(key)),
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	// Keys are emitted sorted, so the manifest is stable across runs.
	if strings.Index(m, "auth-token:") > strings.Index(m, "tls.key:") {
		t.Fatalf("keys not sorted:\n%s", m)
	}
}

func TestNamespaceManifest(t *testing.T) {
	m := namespaceManifest("origamy-dp")
	if !strings.Contains(m, "kind: Namespace") || !strings.Contains(m, "name: origamy-dp") {
		t.Fatalf("bad namespace manifest:\n%s", m)
	}
}
