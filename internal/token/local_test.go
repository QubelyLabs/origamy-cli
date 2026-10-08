package token

import "testing"

func TestIsLocal(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost":              true,
		"http://localhost:8080":         true,
		"http://localhost:8080/":        true,
		"http://127.0.0.1:3000":         true,
		"http://[::1]:3000":             true,
		"https://localhost":             false, // https needs no exemption
		"http://localhost.evil.example": false, // prefix lookalikes
		"http://127.0.0.1.nip.io":       false,
		"http://localhost@evil.example": false, // userinfo trick: host is evil.example
		"http://v1.origamy.io":          false,
		"://bad":                        false,
		"":                              false,
	} {
		if got := isLocal(raw); got != want {
			t.Errorf("isLocal(%q) = %v, want %v", raw, got, want)
		}
	}
}
