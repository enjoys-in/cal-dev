package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// Service provides authentication functionality.
// JWT is verified with the same secret and issuer used by the mail/other services.
// CalDAV Basic Auth uses app-passwords stored in caldev_app_passwords.
type Service struct {
	store  *store.Store
	config *config.AuthConfig

	basicMu    sync.Mutex
	basicCache map[[32]byte]basicEntry // recent successful Basic logins, keyed by sha256(email, password)
	touched    map[string]time.Time    // app password id -> last last_used_at update
	fails      map[string]failEntry    // lower-cased email -> recent failed Basic logins

	userMu    sync.Mutex
	userCache map[string]userEntry // lower-cased, trimmed email -> active account (JWT logins)
}

// userEntry is a cached JWT account lookup.
type userEntry struct {
	resolved ResolvedAuth
	expires  time.Time
}

const (
	// JWT logins look up mail_accounts by email on every request; this in-process cache skips that
	// query. Only active accounts are cached, so a suspended or deleted account is refused at most
	// this long after the change.
	userCacheTTL = 60 * time.Second
	userCacheMax = 10000 // entries; the cache is reset beyond this
)

// failEntry counts failed Basic logins for one email.
type failEntry struct {
	count  int
	first  time.Time // start of the counting window
	locked time.Time // refuse logins until then (zero: not locked)
}

// basicEntry is a cached successful Basic login.
type basicEntry struct {
	userID    int
	appPassID string
	expires   time.Time
}

const (
	basicCacheTTL = 5 * time.Minute  // a revoked app password keeps working at most this long
	basicCacheMax = 10000            // entries; the cache is reset beyond this
	touchInterval = 10 * time.Minute // last_used_at is written at most this often per password

	// Failed-login lockout (per email, in memory, per server instance: with N instances an
	// attacker gets at most N*loginMaxFails guesses per window). Not keyed by IP on purpose:
	// users share proxies/NAT.
	loginMaxFails = 10               // failures within loginWindow that trigger a lockout
	loginWindow   = 15 * time.Minute // counting window, starting at the first failure
	loginLockout  = 15 * time.Minute // how long a locked email is refused without checking
	loginTrackMax = 10000            // max tracked emails
)

// TooManyAttemptsError is returned by AuthenticateBasic while an email is locked out.
// errors.Is(err, ErrTooManyAttempts) is true for it; RetryAfter is the remaining lock time.
type TooManyAttemptsError struct{ RetryAfter time.Duration }

func (e *TooManyAttemptsError) Error() string {
	return fmt.Sprintf("auth: too many failed login attempts, retry in %s", e.RetryAfter.Round(time.Second))
}

func (e *TooManyAttemptsError) Is(target error) bool { return target == ErrTooManyAttempts }

// loginLocked returns how long email is still locked out (0 when it is not).
func (s *Service) loginLocked(email string, now time.Time) time.Duration {
	s.basicMu.Lock()
	defer s.basicMu.Unlock()
	if f, ok := s.fails[email]; ok && now.Before(f.locked) {
		return f.locked.Sub(now)
	}
	return 0
}

// loginFailed records a failed login for email and locks it after loginMaxFails in loginWindow.
func (s *Service) loginFailed(email string, now time.Time) {
	s.basicMu.Lock()
	defer s.basicMu.Unlock()
	f, ok := s.fails[email]
	if !ok || failExpired(f, now) {
		if s.fails == nil {
			s.fails = make(map[string]failEntry)
		}
		if len(s.fails) >= loginTrackMax {
			s.evictFails(now)
		}
		f = failEntry{first: now}
	}
	f.count++
	if f.count >= loginMaxFails {
		f.locked = now.Add(loginLockout)
	}
	s.fails[email] = f
}

// loginSucceeded clears the failure count for email.
func (s *Service) loginSucceeded(email string) {
	s.basicMu.Lock()
	delete(s.fails, email)
	s.basicMu.Unlock()
}

// failExpired reports whether f no longer counts: its lock is over, or (unlocked) its window passed.
func failExpired(f failEntry, now time.Time) bool {
	if !f.locked.IsZero() {
		return !now.Before(f.locked)
	}
	return now.Sub(f.first) > loginWindow
}

// evictFails makes room in s.fails (caller holds basicMu): drops expired entries, then, down to
// 90% of the cap (so this full scan is not repeated on every insert), arbitrary unlocked ones,
// and only as a last resort locked ones.
func (s *Service) evictFails(now time.Time) {
	target := loginTrackMax * 9 / 10
	for k, f := range s.fails {
		if failExpired(f, now) {
			delete(s.fails, k)
		}
	}
	for k, f := range s.fails {
		if len(s.fails) <= target {
			return
		}
		if f.locked.IsZero() {
			delete(s.fails, k)
		}
	}
	for k := range s.fails {
		if len(s.fails) <= target {
			return
		}
		delete(s.fails, k)
	}
}

// dummyHash is compared against for unknown/inactive accounts so they take as long as real ones.
var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("caldev-timing-equaliser"), bcrypt.DefaultCost)
	return h
})

// BasicAuthEnabled reports whether CalDAV Basic Auth is enabled (AUTH_BASIC_ENABLED).
func (s *Service) BasicAuthEnabled() bool {
	return s.config != nil && s.config.BasicAuthEnabled
}

func basicKey(email, password string) [32]byte {
	return sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + "\x00" + password))
}

func (s *Service) cachedBasic(key [32]byte) (basicEntry, bool) {
	s.basicMu.Lock()
	defer s.basicMu.Unlock()
	e, ok := s.basicCache[key]
	if !ok || time.Now().After(e.expires) {
		return basicEntry{}, false
	}
	return e, true
}

func (s *Service) rememberBasic(key [32]byte, e basicEntry) {
	s.basicMu.Lock()
	defer s.basicMu.Unlock()
	if s.basicCache == nil || len(s.basicCache) >= basicCacheMax {
		s.basicCache = make(map[[32]byte]basicEntry)
	}
	s.basicCache[key] = e
}

// touchAppPassword updates last_used_at in the background, at most once per touchInterval.
func (s *Service) touchAppPassword(id string) {
	s.basicMu.Lock()
	if s.touched == nil || len(s.touched) >= basicCacheMax {
		s.touched = make(map[string]time.Time)
	}
	if last, ok := s.touched[id]; ok && time.Since(last) < touchInterval {
		s.basicMu.Unlock()
		return
	}
	s.touched[id] = time.Now()
	s.basicMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.store.TouchAppPassword(ctx, id)
	}()
}

// NewService creates a new auth service.
func NewService(s *store.Store, cfg *config.AuthConfig) *Service {
	return &Service{store: s, config: cfg}
}

// Sentinel errors so callers (and logs) can tell why a valid token was still refused.
var (
	ErrUserNotFound    = errors.New("no account with this email/id in mail_accounts")
	ErrAccountInactive = errors.New("account is not active")
	// ErrTooManyAttempts: the email is locked out after repeated failed Basic logins (answer 429).
	ErrTooManyAttempts = errors.New("too many failed login attempts")
)

// Claims represents JWT claims (compatible with the shared issuer token format).
// The external mail system JWT uses "mid" (mail_accounts.id) as the tenant identifier.
type Claims struct {
	UserID      string `json:"user_id"`
	TenantID    string `json:"tenant_id"`
	MID         string `json:"mid"` // mail_accounts.id — used as tenant ID
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	jwt.RegisteredClaims
}

// HashPassword hashes a password using bcrypt (for app passwords).
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

// CheckPassword compares a password with its hash.
func CheckPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

// GenerateToken creates a JWT token using the shared secret and issuer.
func (s *Service) GenerateToken(userID, tenantID int, email string) (string, error) {
	issuer := s.config.JWTIssuer
	if issuer == "" {
		issuer = "caldev"
	}

	userIDStr := strconv.Itoa(userID)
	tenantIDStr := strconv.Itoa(tenantID)

	claims := &Claims{
		UserID:   userIDStr,
		TenantID: tenantIDStr,
		Email:    email,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.config.TokenExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    issuer,
			Subject:   userIDStr,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.config.JWTSecret))
}

// ValidateToken validates a JWT token using the shared secret.
// Accepts tokens from any service that uses the same secret+issuer.
// Uses MapClaims internally to handle both numeric and string claim values.
func (s *Service) ValidateToken(tokenString string) (*Claims, error) {
	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}),
	}

	// If issuer is configured, enforce it
	if s.config.JWTIssuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(s.config.JWTIssuer))
	}

	// A token without exp would be valid forever (and keep working after the user is removed).
	if s.config.RequireExp {
		parserOpts = append(parserOpts, jwt.WithExpirationRequired())
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(s.config.JWTSecret), nil
	}, parserOpts...)

	if err != nil {
		return nil, err
	}

	mc, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	claims := &Claims{
		UserID:   claimStr(mc, "user_id"),
		TenantID: claimStr(mc, "tenant_id"),
		MID:      claimStr(mc, "mid"),
		Email:    claimStr(mc, "email"),
	}

	// Extract standard registered claims
	if sub, _ := mc.GetSubject(); sub != "" {
		claims.Subject = sub
	}
	if iss, _ := mc.GetIssuer(); iss != "" {
		claims.Issuer = iss
	}

	// Populate UserID from Subject if not set in custom claim
	if claims.UserID == "" && claims.Subject != "" {
		claims.UserID = claims.Subject
	}

	// Use mid (mail_accounts.id) as tenant ID when present
	if claims.TenantID == "" && claims.MID != "" {
		claims.TenantID = claims.MID
	}

	// Populate Email from Subject if subject looks like an email
	if claims.Email == "" && claims.Subject != "" && strings.Contains(claims.Subject, "@") {
		claims.Email = claims.Subject
	}

	return claims, nil
}

// claimStr extracts a claim value as a string, handling both JSON string and number types.
func claimStr(mc jwt.MapClaims, key string) string {
	v, ok := mc[key]
	if !ok {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		if val == float64(int(val)) {
			return strconv.Itoa(int(val))
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// BasicAuthResult holds the result of basic auth parsing.
type BasicAuthResult struct {
	Email    string
	Password string
	Valid    bool
}

// ParseBasicAuth parses an Authorization header for Basic auth.
// Expects email:app-password format.
func ParseBasicAuth(authHeader string) (*BasicAuthResult, error) {
	if !strings.HasPrefix(authHeader, "Basic ") {
		return &BasicAuthResult{Valid: false}, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authHeader, "Basic "))
	if err != nil {
		return nil, fmt.Errorf("invalid base64 in auth header")
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return &BasicAuthResult{Valid: false}, nil
	}

	return &BasicAuthResult{
		Email:    parts[0],
		Password: parts[1],
		Valid:    true,
	}, nil
}

// AuthenticateBasic authenticates a user via email + app password.
// 1. Looks up user by email in mail_accounts
// 2. Checks the password against caldev_app_passwords
// Returns (userID, tenantID, error). tenantID == userID since mail_accounts has no tenant_id.
func (s *Service) AuthenticateBasic(ctx context.Context, email, password string) (int, int, error) {
	// Clients send Basic credentials on every request: skip bcrypt for recently verified ones.
	key := basicKey(email, password)
	if e, ok := s.cachedBasic(key); ok {
		s.touchAppPassword(e.appPassID)
		return e.userID, e.userID, nil
	}

	// Lockout is checked after the cache, so a client that already proved the password keeps
	// working; everything else is refused without running bcrypt.
	failKey := strings.ToLower(strings.TrimSpace(email))
	if wait := s.loginLocked(failKey, time.Now()); wait > 0 {
		return 0, 0, &TooManyAttemptsError{RetryAfter: wait}
	}

	// Look up user in mail_accounts by email
	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return 0, 0, fmt.Errorf("auth: lookup error: %w", err)
	}
	if user == nil {
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(password)) // same timing as a real account
		s.loginFailed(failKey, time.Now())
		return 0, 0, fmt.Errorf("auth: user not found")
	}
	if strings.ToUpper(user.Status) != "ACTIVE" {
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		s.loginFailed(failKey, time.Now())
		return 0, 0, fmt.Errorf("auth: account is %s", user.Status)
	}

	// Check app passwords for this user
	appPasswords, err := s.store.GetAppPasswordsByUser(ctx, user.ID)
	if err != nil {
		return 0, 0, fmt.Errorf("auth: app password lookup error: %w", err)
	}

	for _, ap := range appPasswords {
		if CheckPassword(password, ap.PasswordHash) {
			s.loginSucceeded(failKey)
			s.rememberBasic(key, basicEntry{userID: user.ID, appPassID: ap.ID, expires: time.Now().Add(basicCacheTTL)})
			// Update last_used timestamp asynchronously (throttled)
			s.touchAppPassword(ap.ID)
			// tenant_id == user.ID (mail_accounts has no tenant_id)
			return user.ID, user.ID, nil
		}
	}

	s.loginFailed(failKey, time.Now())
	return 0, 0, fmt.Errorf("auth: invalid credentials")
}

// ResolvedAuth holds the resolved user identity with integer IDs.
type ResolvedAuth struct {
	UserID   int
	TenantID int
	Email    string
}

// ResolveUserFromToken looks up the user in mail_accounts by the JWT claims.
// Tries email first (most reliable), then falls back to user_id/subject.
// This ensures the user still exists and sets tenant_id = user.ID.
func (s *Service) ResolveUserFromToken(ctx context.Context, claims *Claims) (*ResolvedAuth, error) {
	var user *models.User
	var err error
	email := strings.ToLower(strings.TrimSpace(claims.Email))

	// 1. Try by email first (most common identifier in tokens), from the cache when possible
	if email != "" {
		if r, ok := s.cachedUser(email); ok {
			return &r, nil
		}
		user, err = s.store.GetUserByEmail(ctx, email)
		if err != nil {
			return nil, fmt.Errorf("auth: user resolve by email error: %w", err)
		}
	}

	// 2. Fall back to user ID / subject, only for tokens without an email: an unknown email must
	// not be mapped onto whatever local account happens to have the same numeric id.
	if user == nil && email == "" && claims.UserID != "" {
		uid, parseErr := strconv.Atoi(claims.UserID)
		if parseErr == nil {
			user, err = s.store.GetUserByID(ctx, uid)
			if err != nil {
				return nil, fmt.Errorf("auth: user resolve by id error: %w", err)
			}
		}
	}

	if user == nil {
		return nil, fmt.Errorf("auth: %w", ErrUserNotFound)
	}
	if strings.ToUpper(user.Status) != "ACTIVE" {
		return nil, fmt.Errorf("auth: %w (status %s)", ErrAccountInactive, user.Status)
	}

	// Always use canonical values from mail_accounts
	// tenant_id == user.ID (mail_accounts has no tenant_id)
	resolved := ResolvedAuth{
		UserID:   user.ID,
		TenantID: user.ID,
		Email:    user.Email,
	}
	if email != "" {
		s.rememberUser(email, resolved)
	}
	return &resolved, nil
}

// cachedUser returns a still-valid cached JWT account lookup for a normalized email.
func (s *Service) cachedUser(email string) (ResolvedAuth, bool) {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	e, ok := s.userCache[email]
	if !ok || time.Now().After(e.expires) {
		return ResolvedAuth{}, false
	}
	return e.resolved, true
}

// rememberUser caches an active account for userCacheTTL.
func (s *Service) rememberUser(email string, r ResolvedAuth) {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if s.userCache == nil || len(s.userCache) >= userCacheMax {
		s.userCache = make(map[string]userEntry)
	}
	s.userCache[email] = userEntry{resolved: r, expires: time.Now().Add(userCacheTTL)}
}

// ConstantTimeCompare performs constant-time string comparison.
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
