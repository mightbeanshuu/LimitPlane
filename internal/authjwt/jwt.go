// Package authjwt implements JSON Web Tokens and role-based access from
// scratch, with nothing but crypto/hmac.
//
// A JWT is three base64url strings glued with dots:
//
//	header.payload.signature
//	header    {"alg":"HS256","typ":"JWT"}
//	payload   {"sub":"...","role":"admin","exp":...}
//	signature HMAC-SHA256(header + "." + payload, secret)
//
// The server hands one out at login and then trusts its own signature instead
// of keeping a session database: if the HMAC verifies, the claims inside must
// be exactly what we signed, because any tampering changes the digest. That is
// the entire trick, and it is why this file has no dependencies.
//
// Roles: "admin" can do everything, "viewer" is read-only, "user" is an org
// member whose visibility is resolved per request from the org directory.
package authjwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrMalformed    = errors.New("jwt: malformed")
	ErrBadSignature = errors.New("jwt: bad signature")
	ErrExpired      = errors.New("jwt: expired")
)

// Claims is the payload we sign. Extra fields are not carried: everything the
// gateway authorises on is here.
type Claims struct {
	Sub  string `json:"sub"`
	Role string `json:"role"`
	IAT  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
}

func b64(b []byte) string            { return base64.RawURLEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// Sign mints a token valid for expiresIn.
func Sign(sub, role, secret string, expiresIn time.Duration, now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	t := now()
	header := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(Claims{
		Sub:  sub,
		Role: role,
		IAT:  t.Unix(),
		Exp:  t.Add(expiresIn).Unix(),
	})
	body := b64(payload)
	return header + "." + body + "." + sign(header+"."+body, secret)
}

func sign(signingInput, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(signingInput))
	return b64(m.Sum(nil))
}

// Verify checks the signature and the expiry, in that order.
func Verify(token, secret string, now func() time.Time) (Claims, error) {
	if now == nil {
		now = time.Now
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}
	expected := sign(parts[0]+"."+parts[1], secret)

	// Constant-time compare: a byte-by-byte early exit leaks, through timing,
	// how much of a forged signature was correct.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[2])) != 1 {
		return Claims{}, ErrBadSignature
	}

	raw, err := unb64(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	if c.Exp != 0 && c.Exp <= now().Unix() {
		return Claims{}, ErrExpired // a stolen old token is useless
	}
	return c, nil
}

// ---- the login + gate pair a server actually uses --------------------------

type User struct {
	Password string
	Role     string
}

type Auth struct {
	users     map[string]User
	secret    string
	expiresIn time.Duration
	now       func() time.Time
}

type Session struct {
	Token        string `json:"token"`
	Role         string `json:"role"`
	ExpiresInSec int    `json:"expiresInSec"`
}

func New(users map[string]User, secret string, expiresIn time.Duration, now func() time.Time) (*Auth, error) {
	if secret == "" {
		return nil, errors.New("auth needs a secret") // no silent insecurity
	}
	if len(users) == 0 {
		return nil, errors.New("auth needs users")
	}
	if expiresIn == 0 {
		expiresIn = 2 * time.Hour
	}
	return &Auth{users: users, secret: secret, expiresIn: expiresIn, now: now}, nil
}

// Login exchanges credentials for a signed token. Wrong email and wrong
// password fail identically, so an attacker cannot enumerate accounts.
func (a *Auth) Login(email, password string) *Session {
	u, ok := a.users[email]
	if !ok || subtle.ConstantTimeCompare([]byte(u.Password), []byte(password)) != 1 {
		return nil
	}
	return &Session{
		Token:        Sign(email, u.Role, a.secret, a.expiresIn, a.now),
		Role:         u.Role,
		ExpiresInSec: int(a.expiresIn.Seconds()),
	}
}

// Guard reads "Authorization: Bearer <jwt>", verifies it, and checks the role.
// It returns the claims when allowed and nil when not; the caller sends the 401
// so it can shape the error body.
func (a *Auth) Guard(authHeader string, allowedRoles ...string) *Claims {
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == authHeader { // prefix was absent
		return nil
	}
	return a.GuardToken(token, allowedRoles...)
}

// GuardToken is the same check for wires that cannot set headers — the SSE
// stream and the WebSocket upgrade both carry the token in the query string.
func (a *Auth) GuardToken(token string, allowedRoles ...string) *Claims {
	c, err := Verify(token, a.secret, a.now)
	if err != nil {
		return nil
	}
	if len(allowedRoles) > 0 {
		for _, r := range allowedRoles {
			if c.Role == r {
				return &c
			}
		}
		return nil // wrong role
	}
	return &c
}

func (a *Auth) Secret() string           { return a.secret }
func (a *Auth) ExpiresIn() time.Duration { return a.expiresIn }
