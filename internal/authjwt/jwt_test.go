package authjwt_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/authjwt"
)

const secret = "test-secret"

var t0 = time.Date(2026, time.June, 10, 0, 0, 0, 0, time.UTC)

// at pins the clock so expiry is a value we choose, never something we wait for.
func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// forge assembles a token with a CORRECT signature over arbitrary header and
// payload segments. It is the only way to reach the checks that live behind the
// HMAC — a tampered payload is rejected before it is ever decoded.
func forge(header, payload, sec string) string {
	m := hmac.New(sha256.New, []byte(sec))
	m.Write([]byte(header + "." + payload))
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// ---- sign / verify ---------------------------------------------------------

func TestSignVerifyRoundTrip(t *testing.T) {
	token := authjwt.Sign("anshu", "admin", secret, time.Hour, at(t0))

	claims, err := authjwt.Verify(token, secret, at(t0))
	if err != nil {
		t.Fatalf("a token this server just signed was rejected by this server: %v", err)
	}
	if claims.Sub != "anshu" {
		t.Fatalf("the token identifies %q; what comes out must be what went in", claims.Sub)
	}
	if claims.Role != "admin" {
		t.Fatalf("the token grants the %q role instead of admin", claims.Role)
	}
	if claims.IAT != t0.Unix() {
		t.Fatalf("issued-at is %d, expected the injected clock's %d", claims.IAT, t0.Unix())
	}
	if claims.Exp != t0.Add(time.Hour).Unix() {
		t.Fatalf("expiry is %d, expected one hour after issue (%d)", claims.Exp, t0.Add(time.Hour).Unix())
	}
}

func TestSignProducesTheThreePartHS256Shape(t *testing.T) {
	token := authjwt.Sign("anshu", "admin", secret, time.Hour, at(t0))

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT is header.payload.signature; this token has %d segments", len(parts))
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("the header is not base64url: %v", err)
	}
	var header map[string]string
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		t.Fatalf("the header is not JSON: %v", err)
	}
	if header["alg"] != "HS256" || header["typ"] != "JWT" {
		t.Fatalf("the header advertises %v; any verifier reading it would use the wrong algorithm", header)
	}
	if strings.ContainsAny(token, "=+/") {
		t.Fatalf("the token contains standard-base64 padding or characters, which are not URL-safe: %q", token)
	}
}

func TestSignIsDeterministicForTheSameInputs(t *testing.T) {
	// An HMAC has no nonce; two identical claims signed at the same instant
	// must produce byte-identical tokens. If they ever differ, something
	// non-deterministic has crept into the payload.
	a := authjwt.Sign("anshu", "admin", secret, time.Hour, at(t0))
	b := authjwt.Sign("anshu", "admin", secret, time.Hour, at(t0))
	if a != b {
		t.Fatalf("signing the same claims twice produced different tokens:\n%s\n%s", a, b)
	}
}

func TestTamperingWithThePayloadBreaksTheSignature(t *testing.T) {
	// This is the entire reason the gateway can trust its own token instead of
	// keeping a session database.
	token := authjwt.Sign("viewer-user", "viewer", secret, time.Hour, at(t0))
	parts := strings.Split(token, ".")

	// The forged payload: a viewer promoting themselves to admin, far-future exp.
	forged := b64(`{"sub":"viewer-user","role":"admin","iat":1,"exp":9999999999}`)
	tampered := parts[0] + "." + forged + "." + parts[2]

	claims, err := authjwt.Verify(tampered, secret, at(t0))
	if err == nil {
		t.Fatalf("a viewer promoted themselves to %q by editing the payload and the gateway accepted it", claims.Role)
	}
	if !errors.Is(err, authjwt.ErrBadSignature) {
		t.Fatalf("a tampered payload was rejected as %v; it must be reported as a bad signature", err)
	}
}

func TestTamperingWithTheSignatureIsRejected(t *testing.T) {
	token := authjwt.Sign("anshu", "admin", secret, time.Hour, at(t0))
	flipped := token[:len(token)-1]
	if strings.HasSuffix(token, "A") {
		flipped += "B"
	} else {
		flipped += "A"
	}

	if _, err := authjwt.Verify(flipped, secret, at(t0)); !errors.Is(err, authjwt.ErrBadSignature) {
		t.Fatalf("a token with one signature byte changed was accepted (err=%v)", err)
	}
}

func TestAWrongSecretIsRejected(t *testing.T) {
	token := authjwt.Sign("anshu", "admin", secret, time.Hour, at(t0))

	for _, wrong := range []string{"other-secret", "", "test-secre", "test-secrett", "TEST-SECRET"} {
		t.Run("secret="+wrong, func(t *testing.T) {
			if _, err := authjwt.Verify(token, wrong, at(t0)); !errors.Is(err, authjwt.ErrBadSignature) {
				t.Fatalf("a token signed with a different secret verified (err=%v) — anybody could mint admin tokens", err)
			}
		})
	}
}

func TestExpiryIsCheckedAtTheExactSecond(t *testing.T) {
	const ttl = 100 * time.Second
	token := authjwt.Sign("anshu", "admin", secret, ttl, at(t0))

	cases := []struct {
		name      string
		when      time.Time
		wantValid bool
	}{
		{"at the moment of issue", t0, true},
		{"one second before expiry", t0.Add(99 * time.Second), true},
		{"at the exact expiry second", t0.Add(ttl), false},
		{"one second after expiry", t0.Add(101 * time.Second), false},
		{"a stolen token replayed a year later", t0.Add(365 * 24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := authjwt.Verify(token, secret, at(tc.when))
			if tc.wantValid && err != nil {
				t.Fatalf("a token still inside its lifetime was rejected: %v", err)
			}
			if !tc.wantValid {
				if err == nil {
					t.Fatal("an expired token verified — a stolen old token must be useless")
				}
				if !errors.Is(err, authjwt.ErrExpired) {
					t.Fatalf("an expired token was rejected as %v; the caller cannot tell it apart from a forgery", err)
				}
			}
		})
	}
}

func TestMalformedTokensAreRejected(t *testing.T) {
	valid := authjwt.Sign("anshu", "admin", secret, time.Hour, at(t0))
	parts := strings.Split(valid, ".")

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"empty string", "", authjwt.ErrMalformed},
		{"no dots at all", "not-a-jwt", authjwt.ErrMalformed},
		{"only two segments", "header.payload", authjwt.ErrMalformed},
		{"four segments", valid + ".extra", authjwt.ErrMalformed},
		{"nothing but dots", "..", authjwt.ErrBadSignature},
		{"three segments of junk", "a.b.c", authjwt.ErrBadSignature},
		{"the signature stripped off", parts[0] + "." + parts[1] + ".", authjwt.ErrBadSignature},
		{
			"a payload that is not base64url, correctly signed",
			forge(parts[0], "not*valid*base64!", secret),
			authjwt.ErrMalformed,
		},
		{
			"base64 padding, which RawURLEncoding rejects",
			forge(parts[0], b64(`{"sub":"x"}`)+"=", secret),
			authjwt.ErrMalformed,
		},
		{
			"a payload that decodes but is not JSON, correctly signed",
			forge(parts[0], b64("this is not json"), secret),
			authjwt.ErrMalformed,
		},
		{
			"a payload that is JSON but not an object, correctly signed",
			forge(parts[0], b64(`["not","an","object"]`), secret),
			authjwt.ErrMalformed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := authjwt.Verify(tc.token, secret, at(t0))
			if err == nil {
				t.Fatalf("malformed input verified and produced claims %+v", claims)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("rejected as %v, expected %v", err, tc.want)
			}
		})
	}
}

func TestAValidlySignedPayloadWithNoExpiryIsAccepted(t *testing.T) {
	// Documented behaviour: a zero exp means "no expiry", not "expired at the
	// epoch". Only this server can produce such a token, since forging one
	// requires the secret.
	parts := strings.Split(authjwt.Sign("anshu", "admin", secret, time.Hour, at(t0)), ".")
	token := forge(parts[0], b64(`{"sub":"anshu","role":"admin","iat":0,"exp":0}`), secret)

	claims, err := authjwt.Verify(token, secret, at(t0.Add(100*365*24*time.Hour)))
	if err != nil {
		t.Fatalf("a correctly signed token with no expiry claim was rejected: %v", err)
	}
	if claims.Role != "admin" {
		t.Fatalf("claims came back as %+v", claims)
	}
}

func TestNilClockFallsBackToWallTime(t *testing.T) {
	token := authjwt.Sign("anshu", "admin", secret, time.Hour, nil)
	if _, err := authjwt.Verify(token, secret, nil); err != nil {
		t.Fatalf("a wall-clock token failed a wall-clock verification: %v", err)
	}
	expired := authjwt.Sign("anshu", "admin", secret, -time.Hour, nil)
	if _, err := authjwt.Verify(expired, secret, nil); !errors.Is(err, authjwt.ErrExpired) {
		t.Fatalf("a token issued already expired was accepted (err=%v)", err)
	}
}

// ---- construction ----------------------------------------------------------

func users() map[string]authjwt.User {
	return map[string]authjwt.User{
		"admin@x.dev": {Password: "a-pass", Role: "admin"},
		"view@x.dev":  {Password: "v-pass", Role: "viewer"},
	}
}

func newAuth(t *testing.T, now func() time.Time) *authjwt.Auth {
	t.Helper()
	a, err := authjwt.New(users(), secret, 2*time.Hour, now)
	if err != nil {
		t.Fatalf("the standard test auth could not be built: %v", err)
	}
	return a
}

func TestNewRefusesToStartInsecurely(t *testing.T) {
	if _, err := authjwt.New(users(), "", time.Hour, at(t0)); err == nil {
		t.Fatal("auth was constructed with no signing secret — silent insecurity is the one thing this package must not allow")
	}
	if _, err := authjwt.New(nil, secret, time.Hour, at(t0)); err == nil {
		t.Fatal("auth was constructed with no user directory, so nobody could ever log in")
	}
	if _, err := authjwt.New(map[string]authjwt.User{}, secret, time.Hour, at(t0)); err == nil {
		t.Fatal("auth was constructed with an empty user directory")
	}
}

func TestNewAppliesADefaultLifetime(t *testing.T) {
	a, err := authjwt.New(users(), secret, 0, at(t0))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.ExpiresIn(); got != 2*time.Hour {
		t.Fatalf("with no lifetime configured tokens live for %v; the documented default is 2h", got)
	}
	if got := a.Secret(); got != secret {
		t.Fatalf("the configured signing secret came back as %q", got)
	}

	custom, err := authjwt.New(users(), secret, 45*time.Minute, at(t0))
	if err != nil {
		t.Fatal(err)
	}
	if got := custom.ExpiresIn(); got != 45*time.Minute {
		t.Fatalf("an explicit lifetime of 45m was overridden with %v", got)
	}
}

// ---- login -----------------------------------------------------------------

func TestLoginIssuesARoleToken(t *testing.T) {
	a := newAuth(t, at(t0))

	sess := a.Login("view@x.dev", "v-pass")
	if sess == nil {
		t.Fatal("a correct email and password were refused")
	}
	if sess.Role != "viewer" {
		t.Fatalf("the session says role %q; the directory says viewer", sess.Role)
	}
	if sess.ExpiresInSec != 7200 {
		t.Fatalf("the client is told the session lasts %ds; it lasts 7200", sess.ExpiresInSec)
	}

	claims, err := authjwt.Verify(sess.Token, a.Secret(), at(t0))
	if err != nil {
		t.Fatalf("the token handed out at login does not verify: %v", err)
	}
	if claims.Sub != "view@x.dev" || claims.Role != "viewer" {
		t.Fatalf("the issued token carries %+v, not the logged-in user's identity and role", claims)
	}
}

func TestLoginFailsIdenticallyForWrongUserAndWrongPassword(t *testing.T) {
	// Distinguishable failures let an attacker enumerate which accounts exist.
	a := newAuth(t, at(t0))

	cases := []struct{ name, email, password string }{
		{"wrong password", "admin@x.dev", "WRONG"},
		{"unknown account", "ghost@x.dev", "a-pass"},
		{"empty password against a real account", "admin@x.dev", ""},
		{"empty email", "", "a-pass"},
		{"another user's password", "admin@x.dev", "v-pass"},
		{"password with the right prefix", "admin@x.dev", "a-pas"},
		{"password with an extra character", "admin@x.dev", "a-passs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if sess := a.Login(tc.email, tc.password); sess != nil {
				t.Fatalf("login succeeded and issued a %q token", sess.Role)
			}
		})
	}
}

func TestLoginIsCaseSensitiveOnCredentials(t *testing.T) {
	a := newAuth(t, at(t0))
	if a.Login("ADMIN@X.DEV", "a-pass") != nil {
		t.Fatal("an email in a different case was accepted; the directory is an exact-match map")
	}
	if a.Login("admin@x.dev", "A-PASS") != nil {
		t.Fatal("a password in a different case was accepted")
	}
}

func TestIssuedTokensExpireOnTheConfiguredSchedule(t *testing.T) {
	a := newAuth(t, at(t0))
	sess := a.Login("admin@x.dev", "a-pass")
	if sess == nil {
		t.Fatal("login failed")
	}

	later, err := authjwt.New(users(), secret, 2*time.Hour, at(t0.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if later.GuardToken(sess.Token, "admin") != nil {
		t.Fatal("a 2h session was still accepted exactly 2h after it was issued")
	}
}

// ---- the guard -------------------------------------------------------------

func TestGuardEnforcesRoles(t *testing.T) {
	a := newAuth(t, at(t0))
	admin := a.Login("admin@x.dev", "a-pass")
	viewer := a.Login("view@x.dev", "v-pass")
	if admin == nil || viewer == nil {
		t.Fatal("test setup: login failed")
	}

	cases := []struct {
		name    string
		header  string
		roles   []string
		wantOK  bool
		wantSub string
	}{
		{"admin through an admin-only gate", "Bearer " + admin.Token, []string{"admin"}, true, "admin@x.dev"},
		{"admin through a shared gate", "Bearer " + admin.Token, []string{"admin", "viewer"}, true, "admin@x.dev"},
		{"viewer through a shared gate", "Bearer " + viewer.Token, []string{"admin", "viewer"}, true, "view@x.dev"},
		{"viewer through an admin-only gate", "Bearer " + viewer.Token, []string{"admin"}, false, ""},
		{"viewer through a gate for a role nobody has", "Bearer " + viewer.Token, []string{"superuser"}, false, ""},
		{"any valid token through a gate with no role requirement", "Bearer " + viewer.Token, nil, true, "view@x.dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := a.Guard(tc.header, tc.roles...)
			if tc.wantOK {
				if got == nil {
					t.Fatal("an authorised caller was turned away")
				}
				if got.Sub != tc.wantSub {
					t.Fatalf("the gate identified the caller as %q, expected %q", got.Sub, tc.wantSub)
				}
				return
			}
			if got != nil {
				t.Fatalf("a caller with the %q role passed a gate that does not allow it", got.Role)
			}
		})
	}
}

func TestGuardRejectsAnythingThatIsNotABearerHeader(t *testing.T) {
	a := newAuth(t, at(t0))
	sess := a.Login("admin@x.dev", "a-pass")
	if sess == nil {
		t.Fatal("test setup: login failed")
	}

	cases := []struct{ name, header string }{
		{"no header at all", ""},
		{"the raw token with no scheme", sess.Token},
		{"a lower-cased scheme", "bearer " + sess.Token},
		{"an upper-cased scheme", "BEARER " + sess.Token},
		{"the scheme with no space", "Bearer" + sess.Token},
		{"the scheme with nothing after it", "Bearer "},
		{"the scheme alone", "Bearer"},
		{"a different auth scheme", "Basic YWRtaW46cGFzcw=="},
		{"a doubled scheme", "Bearer Bearer " + sess.Token},
		{"leading whitespace before the scheme", " Bearer " + sess.Token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Guard(tc.header, "admin"); got != nil {
				t.Fatalf("the gate accepted %q and identified the caller as %q", tc.header, got.Sub)
			}
		})
	}
}

func TestGuardRejectsExpiredAndForgedTokens(t *testing.T) {
	issuer := newAuth(t, at(t0))
	sess := issuer.Login("admin@x.dev", "a-pass")
	if sess == nil {
		t.Fatal("test setup: login failed")
	}

	t.Run("expired", func(t *testing.T) {
		later, err := authjwt.New(users(), secret, 2*time.Hour, at(t0.Add(3*time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		if later.Guard("Bearer "+sess.Token, "admin") != nil {
			t.Fatal("an expired session still opened an admin gate")
		}
	})

	t.Run("signed by somebody else", func(t *testing.T) {
		other, err := authjwt.New(users(), "a-different-deployment-secret", 2*time.Hour, at(t0))
		if err != nil {
			t.Fatal(err)
		}
		stranger := other.Login("admin@x.dev", "a-pass")
		if stranger == nil {
			t.Fatal("test setup: login failed")
		}
		if issuer.Guard("Bearer "+stranger.Token, "admin") != nil {
			t.Fatal("a token minted by a different deployment opened this deployment's admin gate")
		}
	})

	t.Run("role escalated in the payload", func(t *testing.T) {
		viewer := issuer.Login("view@x.dev", "v-pass")
		parts := strings.Split(viewer.Token, ".")
		forged := parts[0] + "." + b64(`{"sub":"view@x.dev","role":"admin","exp":9999999999}`) + "." + parts[2]
		if issuer.Guard("Bearer "+forged, "admin") != nil {
			t.Fatal("a viewer edited their own role to admin and the gate let them through")
		}
	})
}

func TestGuardTokenIsTheSameCheckWithoutAHeader(t *testing.T) {
	// SSE and the WebSocket upgrade cannot set headers, so they pass the token
	// in the query string; the two paths must not drift apart.
	a := newAuth(t, at(t0))
	sess := a.Login("view@x.dev", "v-pass")
	if sess == nil {
		t.Fatal("test setup: login failed")
	}

	viaHeader := a.Guard("Bearer "+sess.Token, "viewer")
	viaQuery := a.GuardToken(sess.Token, "viewer")
	if viaHeader == nil || viaQuery == nil {
		t.Fatalf("the two gate paths disagree: header=%v query=%v", viaHeader, viaQuery)
	}
	if *viaHeader != *viaQuery {
		t.Fatalf("the header path produced %+v and the query path %+v", *viaHeader, *viaQuery)
	}

	if a.GuardToken(sess.Token, "admin") != nil {
		t.Fatal("the query-string path skipped the role check")
	}
	if a.GuardToken("", "viewer") != nil {
		t.Fatal("an empty token passed the query-string gate")
	}
	if a.GuardToken("garbage", "viewer") != nil {
		t.Fatal("a junk token passed the query-string gate")
	}
}

func TestGuardReturnsACopyOfTheClaims(t *testing.T) {
	a := newAuth(t, at(t0))
	sess := a.Login("view@x.dev", "v-pass")
	if sess == nil {
		t.Fatal("test setup: login failed")
	}

	first := a.Guard("Bearer "+sess.Token, "viewer")
	first.Role = "admin" // a handler scribbling on what it was handed

	second := a.Guard("Bearer "+sess.Token, "viewer")
	if second.Role != "viewer" {
		t.Fatalf("a handler that edited its own claims changed the role every later request sees to %q", second.Role)
	}
	if a.Guard("Bearer "+sess.Token, "admin") != nil {
		t.Fatal("mutating a previous request's claims escalated this token to admin")
	}
}

// ---- a hardening note, asserted so it cannot change silently ---------------

func TestAnEmptyConfiguredPasswordMatchesAnEmptySubmission(t *testing.T) {
	// Documented, deliberately asserted: the demo directory compares passwords
	// in constant time, and two empty strings are equal. A user record with no
	// password is therefore an open door — a deployment hazard, not a code bug,
	// but it must not change without somebody noticing.
	a, err := authjwt.New(map[string]authjwt.User{"nopass@x.dev": {Password: "", Role: "admin"}}, secret, time.Hour, at(t0))
	if err != nil {
		t.Fatal(err)
	}
	if a.Login("nopass@x.dev", "") == nil {
		t.Fatal("behaviour changed: an empty configured password no longer matches an empty submission")
	}
	if a.Login("nopass@x.dev", "anything") != nil {
		t.Fatal("a user with an empty password accepted a non-empty one")
	}
}

// ---- concurrency -----------------------------------------------------------

func TestAuthIsSafeForConcurrentUse(t *testing.T) {
	// Every admin request goes through Guard on its own goroutine. Auth is
	// immutable after New, so this must need no locking at all — run with -race
	// to prove nothing is being written behind the scenes.
	a := newAuth(t, at(t0))
	admin := a.Login("admin@x.dev", "a-pass")
	viewer := a.Login("view@x.dev", "v-pass")
	if admin == nil || viewer == nil {
		t.Fatal("test setup: login failed")
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	fail := make(chan string, 64)

	for g := 0; g < 24; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 300; i++ {
				switch g % 4 {
				case 0:
					if c := a.Guard("Bearer "+admin.Token, "admin"); c == nil || c.Role != "admin" {
						fail <- "an admin token was rejected under concurrent load"
						return
					}
				case 1:
					if a.Guard("Bearer "+viewer.Token, "admin") != nil {
						fail <- "a viewer token passed an admin gate under concurrent load"
						return
					}
				case 2:
					if s := a.Login("admin@x.dev", "a-pass"); s == nil || s.Token != admin.Token {
						fail <- "concurrent logins produced inconsistent tokens"
						return
					}
				case 3:
					if _, err := authjwt.Verify(viewer.Token, a.Secret(), at(t0)); err != nil {
						fail <- "a valid token failed verification under concurrent load"
						return
					}
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(fail)

	for msg := range fail {
		t.Fatal(msg)
	}
}
