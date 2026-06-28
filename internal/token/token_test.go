package token

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mkToken(payload map[string]any) string {
	b, _ := json.Marshal(payload)
	return "dpe_" + base64.RawURLEncoding.EncodeToString(b)
}

// v2: the token carries an opaque handle, which Decode redeems over HTTP(S).
func TestDecode_V2_RedeemsHandle(t *testing.T) {
	var gotHandle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/byod/enroll/resolve" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Handle string `json:"handle"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotHandle = body.Handle
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "dp-1", "tok": "dpt_secret", "addr": "tunnel.example:443", "url": "https://app.example",
		})
	}))
	defer srv.Close()

	got, err := Decode(mkToken(map[string]any{"v": 2, "h": "the-handle", "url": srv.URL}))
	if err != nil {
		t.Fatalf("decode v2: %v", err)
	}
	if gotHandle != "the-handle" {
		t.Fatalf("server received handle %q, want the-handle", gotHandle)
	}
	if got.ID != "dp-1" || got.Tok != "dpt_secret" || got.Addr != "tunnel.example:443" {
		t.Fatalf("bad resolved enrollment: %+v", got)
	}
}

// v1: legacy self-contained token still works (no network).
func TestDecode_V1_Legacy(t *testing.T) {
	got, err := Decode(mkToken(map[string]any{
		"v": 1, "id": "dp-old", "tok": "dpt_old", "addr": "a:443", "url": "https://x",
	}))
	if err != nil {
		t.Fatalf("decode v1: %v", err)
	}
	if got.ID != "dp-old" || got.Tok != "dpt_old" {
		t.Fatalf("bad v1 enrollment: %+v", got)
	}
}

// A tampered token can't downgrade redemption to plaintext HTTP.
func TestDecode_V2_RejectsNonHTTPS(t *testing.T) {
	if _, err := Decode(mkToken(map[string]any{"v": 2, "h": "x", "url": "http://evil.example"})); err == nil {
		t.Fatal("expected non-HTTPS redemption URL to be rejected")
	}
}

// A v2 token with a bad handle surfaces the server's error, not a usable result.
func TestDecode_V2_ServerRejectsHandle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid or already-used enrollment token"})
	}))
	defer srv.Close()

	if _, err := Decode(mkToken(map[string]any{"v": 2, "h": "bad", "url": srv.URL})); err == nil {
		t.Fatal("expected redemption failure to be surfaced")
	}
}
