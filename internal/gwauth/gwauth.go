// Package gwauth implements the gateway's browser authentication (WS10): a
// single shared secret (the login password / bearer token), stateless
// HMAC-signed session cookies, and a same-origin check for the WebSocket
// upgrade.
//
// The model is deliberately minimal because herdr is single-user: there is one
// secret, no user table. A browser exchanges the secret once at /login for a
// session cookie; a headless client (wsprobe, scripts) presents the secret
// directly as an Authorization: Bearer token. Session cookies are signed with a
// per-process random key, so restarting the gateway invalidates outstanding
// sessions (re-login required) and no secret is ever written to disk.
package gwauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CookieName is the session cookie the gateway sets after a successful login.
const CookieName = "hsess"

// bearerPrefix is the scheme in an Authorization header value.
const bearerPrefix = "Bearer "

// Authenticator validates the shared secret and issues/verifies session
// cookies. All fields are read-only after New, so it is safe for concurrent
// use by every request handler.
type Authenticator struct {
	secret  []byte        // shared login password / bearer token
	signKey []byte        // per-process key signing session cookies
	ttl     time.Duration // session lifetime
}

// New builds an Authenticator around a shared secret with the given session
// TTL, generating a random cookie-signing key. secret must be non-empty and
// ttl positive.
func New(secret string, ttl time.Duration) (*Authenticator, error) {
	if secret == "" {
		return nil, errors.New("gwauth: empty secret")
	}
	if ttl <= 0 {
		return nil, errors.New("gwauth: non-positive session ttl")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("gwauth: generate signing key: %w", err)
	}
	return &Authenticator{secret: []byte(secret), signKey: key, ttl: ttl}, nil
}

// TTL is the configured session lifetime (used to set the cookie's MaxAge).
func (a *Authenticator) TTL() time.Duration { return a.ttl }

// CheckSecret reports, in constant time, whether provided equals the shared
// secret. Used by the login POST and the bearer-token path.
func (a *Authenticator) CheckSecret(provided string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), a.secret) == 1
}

// CheckBearer parses an "Authorization: Bearer <token>" header value and
// reports whether the token matches the shared secret. Empty or malformed
// values return false.
func (a *Authenticator) CheckBearer(authorization string) bool {
	if len(authorization) <= len(bearerPrefix) ||
		!strings.EqualFold(authorization[:len(bearerPrefix)], bearerPrefix) {
		return false
	}
	return a.CheckSecret(strings.TrimSpace(authorization[len(bearerPrefix):]))
}

// IssueSession returns a cookie value binding an expiry (now+TTL) to an HMAC
// over that expiry: "<expiryUnix>.<hex(mac)>". It carries no identity — only
// proof the server minted it and when it lapses.
func (a *Authenticator) IssueSession(now time.Time) string {
	payload := strconv.FormatInt(now.Add(a.ttl).Unix(), 10)
	return payload + "." + hex.EncodeToString(a.mac(payload))
}

// ValidSession reports whether a value produced by IssueSession is well-formed,
// correctly signed, and unexpired at now. The MAC comparison is constant time;
// a bad signature and an expired-but-valid signature are both rejected.
func (a *Authenticator) ValidSession(cookie string, now time.Time) bool {
	dot := strings.IndexByte(cookie, '.')
	if dot <= 0 {
		return false
	}
	payload, sig := cookie[:dot], cookie[dot+1:]
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	if !hmac.Equal(sigBytes, a.mac(payload)) {
		return false
	}
	return now.Unix() < exp
}

func (a *Authenticator) mac(payload string) []byte {
	h := hmac.New(sha256.New, a.signKey)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// GenerateSecret returns a random URL-safe secret suitable as a login password
// when the operator did not supply one (~24 chars, 18 bytes of entropy).
func GenerateSecret() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("gwauth: generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// OriginOK reports whether a WebSocket upgrade's Origin header is same-origin
// with the request Host. A browser always sends Origin on a cross-document WS
// handshake, so a mismatch is a cross-site attempt and is rejected. A
// non-browser client sends no Origin and is allowed here — the same-origin
// policy does not apply to it, and it must still pass the secret/cookie check.
func OriginOK(origin, host string) bool {
	if origin == "" {
		return true // non-browser client (e.g. wsprobe); auth is still enforced
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == host
}
