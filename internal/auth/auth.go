// Package auth provides local-account primitives: bcrypt password hashing and stateless,
// HMAC-signed session tokens carried in an HttpOnly cookie.
//
// The token is self-contained (userID + expiry, signed) rather than a server-side session id, so
// it needs no sessions table and works identically under the in-memory and Postgres stores. The
// signing key is derived from KAAS_SECRET_KEY - the same env-derived-key shortcut used for
// at-rest secrets (see app.loadKey) and the shell exec token (see app.shellToken). This is a
// deliberate demo shortcut (see CLAUDE.md fidelity stance): production would use a real session
// store with server-side revocation and a KMS-managed signing key. Because the token is stateless
// it can't be revoked before expiry; deleting a user is still effective because every request
// re-loads the user from the store, so a token for a removed user resolves to nobody.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword returns a bcrypt hash suitable for storing.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword reports whether password matches the stored bcrypt hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// Signer issues and verifies session tokens with an HMAC key derived from the platform secret.
type Signer struct {
	key []byte
}

// NewSigner derives a per-purpose HMAC key from the platform secret (empty is tolerated but yields
// a weak, well-known key - callers should ensure KAAS_SECRET_KEY is set in real deployments).
func NewSigner(secret string) *Signer {
	sum := sha256.Sum256([]byte("kaas-session:" + secret))
	return &Signer{key: sum[:]}
}

// Issue returns a signed token binding userID to an expiry ttl from now.
func (s *Signer) Issue(userID string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	payload := userID + "|" + strconv.FormatInt(exp, 10)
	return b64(payload) + "." + b64(string(s.mac(payload)))
}

var (
	errMalformed = errors.New("auth: malformed token")
	errSignature = errors.New("auth: bad signature")
	errExpired   = errors.New("auth: token expired")
)

// Verify checks the signature and expiry and returns the userID the token was issued for.
func (s *Signer) Verify(token string) (string, error) {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return "", errMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return "", errMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return "", errMalformed
	}
	if !hmac.Equal(sig, s.mac(string(payload))) {
		return "", errSignature
	}
	sep := strings.LastIndexByte(string(payload), '|')
	if sep < 0 {
		return "", errMalformed
	}
	exp, err := strconv.ParseInt(string(payload[sep+1:]), 10, 64)
	if err != nil {
		return "", errMalformed
	}
	if time.Now().Unix() > exp {
		return "", errExpired
	}
	return string(payload[:sep]), nil
}

func (s *Signer) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
