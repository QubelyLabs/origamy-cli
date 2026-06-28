package token

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerateIdentity(t *testing.T) {
	keyPEM, csrPEM, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if kb, _ := pem.Decode(keyPEM); kb == nil || kb.Type != "EC PRIVATE KEY" {
		t.Fatal("private key is not a usable EC PRIVATE KEY PEM")
	}
	cb, _ := pem.Decode(csrPEM)
	if cb == nil || cb.Type != "CERTIFICATE REQUEST" {
		t.Fatal("CSR is not a usable PEM")
	}
	csr, err := x509.ParseCertificateRequest(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}
}

// v2 token + a CA-configured control plane: Enroll registers the CSR and returns
// the cert + bearer + addresses.
func TestEnrollRegistersWithCert(t *testing.T) {
	var gotHandle, gotCSR string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/byod/register" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct{ Handle, CSR string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotHandle, gotCSR = body.Handle, body.CSR
		_ = json.NewEncoder(w).Encode(map[string]string{
			"data_plane_id": "dp-9", "auth_token": "dpt_bearer", "certificate": "CERTPEM",
			"ca_chain": "CAPEM", "tunnel_addr": "tunnel.example:443", "http_url": "https://app.example",
		})
	}))
	defer srv.Close()

	_, csrPEM, _ := GenerateIdentity()
	enr, err := Enroll(mkToken(map[string]any{"v": 2, "h": "the-handle", "url": srv.URL}), csrPEM)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if gotHandle != "the-handle" || gotCSR == "" {
		t.Fatalf("server received handle=%q csr-empty=%v", gotHandle, gotCSR == "")
	}
	if enr.ID != "dp-9" || enr.Tok != "dpt_bearer" || enr.Cert != "CERTPEM" || enr.CAChain != "CAPEM" || enr.Addr != "tunnel.example:443" {
		t.Fatalf("bad enrollment: %+v", enr)
	}
}

// v2 token but mTLS disabled (503): Enroll falls back to bearer-only resolve.
func TestEnrollFallsBackWhenMTLSDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/byod/register":
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "mTLS enrollment is not configured"})
		case "/v1/byod/enroll/resolve":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "dp-1", "tok": "dpt_legacy", "addr": "a:443", "url": "https://x"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, csrPEM, _ := GenerateIdentity()
	enr, err := Enroll(mkToken(map[string]any{"v": 2, "h": "h", "url": srv.URL}), csrPEM)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if enr.Tok != "dpt_legacy" || enr.Cert != "" {
		t.Fatalf("expected bearer-only fallback, got %+v", enr)
	}
}

// v1 legacy token: no network, returns the embedded params.
func TestEnrollLegacyV1(t *testing.T) {
	enr, err := Enroll(mkToken(map[string]any{"v": 1, "id": "dp", "tok": "dpt_old", "addr": "a", "url": "https://x"}), nil)
	if err != nil {
		t.Fatalf("enroll v1: %v", err)
	}
	if enr.Tok != "dpt_old" || enr.Cert != "" {
		t.Fatalf("bad v1 enrollment: %+v", enr)
	}
}
