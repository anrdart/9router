// Package auth verifies 9router dashboard credentials the same way the Node app
// does, so the Go backend can gate native /api/* routes without deferring to the
// Node proxy. It reads the SAME secret files Node writes under DATA_DIR, so a
// browser session (auth_token cookie) or CLI token minted by Node is accepted by
// Go unchanged.
//
// The JWT verifier is a minimal, dependency-free HS256 implementation matching
// jose's SignJWT({alg:"HS256"}) output used by src/lib/auth/dashboardSession.js.
// We only ever VERIFY tokens (never sign), so a full JOSE library is unnecessary.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Errors returned by VerifyJWT. Callers generally only care that err != nil, but
// distinct values aid tests and logging.
var (
	ErrMalformedToken = errors.New("auth: malformed token")
	ErrBadAlg         = errors.New("auth: unexpected JWT alg")
	ErrBadSignature   = errors.New("auth: invalid JWT signature")
	ErrExpired        = errors.New("auth: token expired")
	ErrNoSecret       = errors.New("auth: JWT secret unavailable")
)

// Claims is the decoded, verified JWT payload. Only the fields the gate cares
// about are typed; everything else is discarded. Mirrors the claims set by
// createDashboardAuthToken: {authenticated:true, ...customClaims, iat, exp}.
type Claims struct {
	Authenticated       bool  `json:"authenticated"`
	ForcePasswordChange bool  `json:"force_password_change"`
	Exp                 int64 `json:"exp"`
	Iat                 int64 `json:"iat"`
}

// LoadJWTSecret resolves the HMAC secret exactly like loadJwtSecret() in
// dashboardSession.js: JWT_SECRET env wins, else DATA_DIR/jwt-secret is read.
//
// Unlike Node, Go NEVER generates the secret file — Node owns its lifecycle. If
// neither source is present the secret is empty and VerifyJWT fails closed
// (every token rejected), which is the safe default: without the shared secret
// Go cannot validate any session, so it must not accept one.
func LoadJWTSecret(dataDir string) []byte {
	if v := strings.TrimSpace(os.Getenv("JWT_SECRET")); v != "" {
		return []byte(v)
	}
	if dataDir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "jwt-secret"))
	if err != nil {
		return nil
	}
	return []byte(strings.TrimSpace(string(b)))
}

// VerifyJWT validates an HS256 token against secret and returns its claims. It
// checks the alg header, the HMAC signature (constant-time), and expiry (with
// the same semantics as jose: exp is required-if-present and compared to now).
// now is injected so tests are deterministic; pass time.Now() in production.
func VerifyJWT(token string, secret []byte, now time.Time) (*Claims, error) {
	if len(secret) == 0 {
		return nil, ErrNoSecret
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrMalformedToken
	}

	// Header: assert alg=HS256, typ ignored (jose omits it here).
	headerJSON, err := b64Decode(parts[0])
	if err != nil {
		return nil, ErrMalformedToken
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, ErrMalformedToken
	}
	if header.Alg != "HS256" {
		return nil, fmt.Errorf("%w: %q", ErrBadAlg, header.Alg)
	}

	// Signature is over the ASCII "<header>.<payload>" segments verbatim.
	signingInput := parts[0] + "." + parts[1]
	want := hmacSHA256(signingInput, secret)
	got, err := b64Decode(parts[2])
	if err != nil {
		return nil, ErrMalformedToken
	}
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return nil, ErrBadSignature
	}

	// Payload.
	payloadJSON, err := b64Decode(parts[1])
	if err != nil {
		return nil, ErrMalformedToken
	}
	var claims Claims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, ErrMalformedToken
	}

	// Expiry: jose rejects tokens whose exp <= now. All 9router tokens set exp,
	// so treat a missing exp (0) as invalid rather than eternal.
	if claims.Exp == 0 {
		return nil, ErrExpired
	}
	if now.Unix() >= claims.Exp {
		return nil, ErrExpired
	}

	return &claims, nil
}

// hmacSHA256 returns the raw HMAC-SHA256 of msg keyed by secret.
func hmacSHA256(msg string, secret []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// b64Decode decodes a JWT segment. JOSE uses base64url WITHOUT padding; we accept
// both padded and unpadded via RawURLEncoding after stripping any '=' padding.
func b64Decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
