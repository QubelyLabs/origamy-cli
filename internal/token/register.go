package token

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// errMTLSDisabled means the control plane has no BYOD CA configured, so
// /byod/register is unavailable — callers fall back to the bearer-only path.
var errMTLSDisabled = errors.New("mTLS enrollment not configured on the control plane")

// GenerateIdentity creates a fresh EC P-256 keypair and a CSR. The private key
// (PEM) never leaves the holder; only the CSR is sent to /byod/register. The CSR
// subject is cosmetic — the control plane assigns the SPIFFE identity.
func GenerateIdentity() (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: "origamy-data-plane"}}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), nil
}

// Enroll turns a raw dpe_ token into install params, requesting an mTLS identity
// when possible:
//   - v2 token + a CA-configured control plane → /byod/register signs the CSR and
//     returns cert + bearer + addresses in one round-trip.
//   - v2 token, mTLS not configured → falls back to /enroll/resolve (bearer only).
//   - v1 legacy token → the embedded params (bearer only).
//
// csrPEM is from GenerateIdentity; pass nil to skip mTLS and resolve only.
func Enroll(raw string, csrPEM []byte) (*Enrollment, error) {
	tok, err := decodeRaw(raw)
	if err != nil {
		return nil, err
	}
	if tok.Handle == "" {
		// v1 legacy self-contained token.
		if tok.ID == "" || tok.Tok == "" {
			return nil, fmt.Errorf("token is missing required fields — generate a new one from your dashboard")
		}
		return tok, nil
	}
	if len(csrPEM) > 0 {
		enr, regErr := register(tok.URL, tok.Handle, csrPEM)
		if regErr == nil {
			return enr, nil
		}
		if !errors.Is(regErr, errMTLSDisabled) {
			return nil, regErr
		}
		// mTLS not configured on the control plane — fall back to bearer-only.
	}
	return redeem(tok.URL, tok.Handle)
}

// register redeems a one-time handle + CSR for a signed mTLS cert (and, for
// dual-mode, the bearer + addresses) at /v1/byod/register. HTTPS-only except
// localhost, like redeem — a tampered token can't downgrade to plaintext.
func register(baseURL, handle string, csrPEM []byte) (*Enrollment, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, fmt.Errorf("enrollment token is missing its control-plane URL — generate a new one")
	}
	if !strings.HasPrefix(baseURL, "https://") && !isLocal(baseURL) {
		return nil, fmt.Errorf("refusing to enroll over a non-HTTPS URL (%s)", baseURL)
	}

	body, _ := json.Marshal(map[string]string{"handle": handle, "csr": string(csrPEM)})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/byod/register", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the control plane to enroll: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, errMTLSDisabled
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("could not enroll: %s", e.Error)
	}

	var r struct {
		DataPlaneID string `json:"data_plane_id"`
		AuthToken   string `json:"auth_token"`
		Certificate string `json:"certificate"`
		CAChain     string `json:"ca_chain"`
		TunnelAddr  string `json:"tunnel_addr"`
		HTTPURL     string `json:"http_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("could not parse enrollment response: %w", err)
	}
	if r.DataPlaneID == "" || r.AuthToken == "" || r.Certificate == "" {
		return nil, fmt.Errorf("enrollment response is missing required fields")
	}
	return &Enrollment{
		ID:      r.DataPlaneID,
		Tok:     r.AuthToken,
		Addr:    r.TunnelAddr,
		URL:     r.HTTPURL,
		Cert:    r.Certificate,
		CAChain: r.CAChain,
	}, nil
}
