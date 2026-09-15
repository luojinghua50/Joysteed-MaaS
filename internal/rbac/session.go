package rbac

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// SessionClaims are the server-side identity fields that must be fixed when a
// session is created. They are not derived from request parameters.
type SessionClaims struct {
	SessionID     string
	Principal     Principal
	CSRFTokenHash string
	ExpiresAt     time.Time
}

func NewSessionClaims(sessionID string, principal Principal, lifetime time.Duration) (SessionClaims, string, error) {
	if strings.TrimSpace(sessionID) == "" || !principal.Valid() {
		return SessionClaims{}, "", ErrInvalidPrincipal
	}
	if lifetime <= 0 {
		return SessionClaims{}, "", errors.New("rbac: session lifetime must be positive")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return SessionClaims{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	return SessionClaims{SessionID: sessionID, Principal: principal, CSRFTokenHash: hashToken(token), ExpiresAt: time.Now().UTC().Add(lifetime)}, token, nil
}

func (s SessionClaims) Valid(now time.Time) bool {
	return s.SessionID != "" && s.Principal.Valid() && s.ExpiresAt.After(now)
}

func (s SessionClaims) VerifyCSRF(token string) bool {
	if token == "" || s.CSRFTokenHash == "" {
		return false
	}
	got := hashToken(token)
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.CSRFTokenHash)) == 1
}

func hashToken(token string) string {
	// A session store should persist only a digest, never the bearer token.
	// SHA-256 is sufficient here because the token is generated with 256 bits
	// of entropy and is never password-derived.
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
