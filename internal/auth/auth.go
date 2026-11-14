package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
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
}

// NewService creates a new auth service.
func NewService(s *store.Store, cfg *config.AuthConfig) *Service {
	return &Service{store: s, config: cfg}
}

// Claims represents JWT claims (compatible with the shared issuer token format).
// The external mail system JWT uses "mid" (mail_accounts.id) as the tenant identifier.
type Claims struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
	MID      string `json:"mid"` // mail_accounts.id — used as tenant ID
	Email    string `json:"email"`
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
	// Look up user in mail_accounts by email
	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return 0, 0, fmt.Errorf("auth: lookup error: %w", err)
	}
	if user == nil {
		return 0, 0, fmt.Errorf("auth: user not found")
	}
	if strings.ToUpper(user.Status) != "ACTIVE" {
		return 0, 0, fmt.Errorf("auth: account is %s", user.Status)
	}

	// Check app passwords for this user
	appPasswords, err := s.store.GetAppPasswordsByUser(ctx, user.ID)
	if err != nil {
		return 0, 0, fmt.Errorf("auth: app password lookup error: %w", err)
	}

	for _, ap := range appPasswords {
		if CheckPassword(password, ap.PasswordHash) {
			// Update last_used timestamp asynchronously
			go func(id string) {
				_ = s.store.TouchAppPassword(context.Background(), id)
			}(ap.ID)
			// tenant_id == user.ID (mail_accounts has no tenant_id)
			return user.ID, user.ID, nil
		}
	}

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

	// 1. Try by email first (most common identifier in tokens)
	if claims.Email != "" {
		user, err = s.store.GetUserByEmail(ctx, claims.Email)
		if err != nil {
			return nil, fmt.Errorf("auth: user resolve by email error: %w", err)
		}
	}

	// 2. Fall back to user ID / subject
	if user == nil && claims.UserID != "" {
		uid, parseErr := strconv.Atoi(claims.UserID)
		if parseErr == nil {
			user, err = s.store.GetUserByID(ctx, uid)
			if err != nil {
				return nil, fmt.Errorf("auth: user resolve by id error: %w", err)
			}
		}
	}

	if user == nil {
		return nil, fmt.Errorf("auth: user not found in mail_accounts")
	}
	if strings.ToUpper(user.Status) != "ACTIVE" {
		return nil, fmt.Errorf("auth: account is %s", user.Status)
	}

	// Always use canonical values from mail_accounts
	// tenant_id == user.ID (mail_accounts has no tenant_id)
	return &ResolvedAuth{
		UserID:   user.ID,
		TenantID: user.ID,
		Email:    user.Email,
	}, nil
}

// ConstantTimeCompare performs constant-time string comparison.
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
