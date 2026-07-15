package security

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"strings"
)

type TokenAuth struct {
	hash        [sha256.Size]byte
	fingerprint string
}

func LoadToken(path string) (*TokenAuth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if len(token) < 43 {
		return nil, errors.New("token must contain at least 256 bits of encoded random data")
	}
	h := sha256.Sum256([]byte(token))
	return &TokenAuth{hash: h, fingerprint: fmt.Sprintf("%x", h[:8])}, nil
}

func (a *TokenAuth) Authenticate(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	candidate := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	h := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(a.hash[:], h[:]) == 1
}

func (a *TokenAuth) Fingerprint() string { return a.fingerprint }
