package token

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Enrollment struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	Tok  string `json:"tok"`
	Addr string `json:"addr"`
	URL  string `json:"url"`
	Exp  int64  `json:"exp"`

	// Handle is set on v2 tokens: the dpe_ token carries only this opaque handle
	// + URL, never the credential. It's redeemed for the real params at
	// <URL>/v1/byod/enroll/resolve.
	Handle string `json:"h"`
}

// Decode parses a dpe_ enrollment token into install parameters.
//
//   - v2 (current): the token carries an opaque handle; Decode redeems it over
//     HTTPS for the real {id, tok, addr, url}. The credential is never in the
//     token, so a leaked token is useless.
//   - v1 (legacy): the token embedded the credential directly. Still accepted so
//     tokens minted just before the upgrade keep working.
func Decode(raw string) (*Enrollment, error) {
	b64 := strings.TrimPrefix(raw, "dpe_")
	b64 = strings.NewReplacer("-", "+", "_", "/").Replace(b64)
	switch len(b64) % 4 {
	case 2:
		b64 += "=="
	case 3:
		b64 += "="
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("could not decode token: %w", err)
	}
	var tok Enrollment
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("could not parse token: %w", err)
	}

	if tok.Handle != "" {
		return redeem(tok.URL, tok.Handle)
	}

	// Legacy self-contained token.
	if tok.ID == "" || tok.Tok == "" {
		return nil, fmt.Errorf("token is missing required fields — generate a new one from your dashboard")
	}
	return &tok, nil
}

// redeem exchanges a one-time handle for the real install params at the control
// plane. The endpoint is required to be HTTPS (except localhost) so a tampered
// token can't downgrade the redemption to a MITM-able plaintext request.
func redeem(baseURL, handle string) (*Enrollment, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, fmt.Errorf("enrollment token is missing its control-plane URL — generate a new one")
	}
	if !strings.HasPrefix(baseURL, "https://") && !isLocal(baseURL) {
		return nil, fmt.Errorf("refusing to redeem enrollment token over a non-HTTPS URL (%s)", baseURL)
	}

	body, _ := json.Marshal(map[string]string{"handle": handle})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/byod/enroll/resolve", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the control plane to redeem your token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("could not redeem enrollment token: %s", e.Error)
	}

	var out Enrollment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("could not parse redemption response: %w", err)
	}
	if out.ID == "" || out.Tok == "" {
		return nil, fmt.Errorf("redemption response is missing required fields — generate a new token")
	}
	return &out, nil
}

func isLocal(url string) bool {
	return strings.HasPrefix(url, "http://localhost") || strings.HasPrefix(url, "http://127.0.0.1")
}
