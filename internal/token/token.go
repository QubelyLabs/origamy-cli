package token

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type Enrollment struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	Tok  string `json:"tok"`
	Addr string `json:"addr"`
	URL  string `json:"url"`
	Exp  int64  `json:"exp"`
}

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
	if tok.ID == "" || tok.Tok == "" {
		return nil, fmt.Errorf("token is missing required fields — generate a new one from your dashboard")
	}
	return &tok, nil
}
