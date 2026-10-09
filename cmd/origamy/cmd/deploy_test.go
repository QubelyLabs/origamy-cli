package cmd

import (
	"errors"
	"strings"
	"testing"
)

func TestEnrollHint(t *testing.T) {
	newToken := "generate a new one"
	connectivity := "reach your control plane"
	cases := map[string]string{
		// The control plane's own phrases (byod_handlers.go) → a new token is the fix.
		"could not enroll: invalid or already-used enrollment token":                      newToken,
		"could not redeem enrollment token: enrollment token expired; generate a new one": newToken,
		"token is missing required fields — generate a new one from your dashboard":       newToken,
		"could not decode token: illegal base64 data at input byte 4":                     newToken,
		// Anything else is connectivity: a new token would not help.
		"could not reach the control plane to enroll: x509: certificate has expired or is not yet valid": connectivity,
		"could not enroll: HTTP 502":                                  connectivity,
		"refusing to enroll over a non-HTTPS URL (http://cp.example)": connectivity,
	}
	for msg, want := range cases {
		got := enrollHint(errors.New(msg))
		if !strings.HasPrefix(got, msg) {
			t.Errorf("hint must start with the server's message: %q", got)
		}
		if !strings.Contains(got, want) {
			t.Errorf("enrollHint(%q) = %q, want it to mention %q", msg, got, want)
		}
	}
}
