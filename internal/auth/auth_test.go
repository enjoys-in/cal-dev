package auth

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/golang-jwt/jwt/v5"
)

func svc(requireExp bool) *Service {
	return &Service{config: &config.AuthConfig{JWTSecret: "unit-test-secret-0123456789", JWTIssuer: "ENJOYS", RequireExp: requireExp}}
}

func sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("unit-test-secret-0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTokenWithoutExpRejectedWhenRequired(t *testing.T) {
	tok := sign(t, jwt.MapClaims{"email": "a@x.com", "iss": "ENJOYS"})
	if _, err := svc(true).ValidateToken(tok); err == nil {
		t.Fatal("token without exp must be rejected when AUTH_REQUIRE_EXP is on")
	}
	if _, err := svc(false).ValidateToken(tok); err != nil {
		t.Fatalf("token without exp must still work when the requirement is off: %v", err)
	}
}

func TestValidTokenAccepted(t *testing.T) {
	tok := sign(t, jwt.MapClaims{"email": "a@x.com", "iss": "ENJOYS", "exp": time.Now().Add(time.Hour).Unix()})
	c, err := svc(true).ValidateToken(tok)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if c.Email != "a@x.com" {
		t.Fatalf("email claim lost: %q", c.Email)
	}
}

func TestExpiredAndWrongIssuerRejected(t *testing.T) {
	expired := sign(t, jwt.MapClaims{"email": "a@x.com", "iss": "ENJOYS", "exp": time.Now().Add(-time.Hour).Unix()})
	if _, err := svc(true).ValidateToken(expired); err == nil {
		t.Fatal("expired token accepted")
	}
	wrongIss := sign(t, jwt.MapClaims{"email": "a@x.com", "iss": "SOMEONE-ELSE", "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := svc(true).ValidateToken(wrongIss); err == nil {
		t.Fatal("token from another issuer accepted")
	}
}

func TestBasicCache(t *testing.T) {
	s := svc(true)
	k := basicKey(" A@x.com ", "pw")
	if _, ok := s.cachedBasic(k); ok {
		t.Fatal("empty cache must miss")
	}
	s.rememberBasic(k, basicEntry{userID: 7, expires: time.Now().Add(time.Minute)})
	if e, ok := s.cachedBasic(basicKey("a@x.com", "pw")); !ok || e.userID != 7 {
		t.Fatal("cached login must hit (email is case-insensitive)")
	}
	if _, ok := s.cachedBasic(basicKey("a@x.com", "other")); ok {
		t.Fatal("a different password must miss")
	}
	s.rememberBasic(k, basicEntry{userID: 7, expires: time.Now().Add(-time.Second)})
	if _, ok := s.cachedBasic(k); ok {
		t.Fatal("expired entry must miss")
	}
}

func TestLoginLockout(t *testing.T) {
	s := svc(true)
	now := time.Now()
	for i := 0; i < loginMaxFails-1; i++ {
		s.loginFailed("a@x.com", now)
	}
	if s.loginLocked("a@x.com", now) != 0 {
		t.Fatal("locked before reaching the limit")
	}
	s.loginFailed("a@x.com", now)
	if w := s.loginLocked("a@x.com", now); w != loginLockout {
		t.Fatalf("expected lockout of %s, got %s", loginLockout, w)
	}
	if s.loginLocked("b@x.com", now) != 0 {
		t.Fatal("lockout must be per email")
	}
	if s.loginLocked("a@x.com", now.Add(loginLockout)) != 0 {
		t.Fatal("lockout must end")
	}
	// An expired lock starts a fresh window.
	s.loginFailed("a@x.com", now.Add(loginLockout))
	if s.loginLocked("a@x.com", now.Add(loginLockout)) != 0 {
		t.Fatal("one failure after the lock ended must not relock")
	}

	// Failures outside the window do not add up; a success clears the count.
	for i := 0; i < loginMaxFails-1; i++ {
		s.loginFailed("c@x.com", now)
	}
	s.loginFailed("c@x.com", now.Add(loginWindow+time.Second))
	if s.loginLocked("c@x.com", now.Add(loginWindow+time.Second)) != 0 {
		t.Fatal("failures from an old window must not count")
	}
	for i := 0; i < loginMaxFails-1; i++ {
		s.loginFailed("d@x.com", now)
	}
	s.loginSucceeded("d@x.com")
	s.loginFailed("d@x.com", now)
	if s.loginLocked("d@x.com", now) != 0 {
		t.Fatal("success must clear the count")
	}
}

func TestLoginLockoutBounded(t *testing.T) {
	s := svc(true)
	now := time.Now()
	for i := 0; i < loginMaxFails; i++ {
		s.loginFailed("victim@x.com", now)
	}
	for i := 0; i < 2*loginTrackMax; i++ {
		s.loginFailed(strconv.Itoa(i)+"@x.com", now)
	}
	if len(s.fails) > loginTrackMax {
		t.Fatalf("map grew to %d", len(s.fails))
	}
	if s.loginLocked("victim@x.com", now) == 0 {
		t.Fatal("eviction must keep locked entries while unlocked ones exist")
	}
}

func TestTooManyAttemptsError(t *testing.T) {
	var err error = &TooManyAttemptsError{RetryAfter: time.Minute}
	if !errors.Is(fmt.Errorf("wrapped: %w", err), ErrTooManyAttempts) {
		t.Fatal("errors.Is must match ErrTooManyAttempts")
	}
}
