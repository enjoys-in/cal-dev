package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
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
func (s *Service) GenerateToken(userID, tenantID string, email string) (string, error) {
	issuer := s.config.JWTIssuer
	if issuer == "" {
		issuer = "caldev"
	}

	claims := &Claims{
		UserID:   userID,
		TenantID: tenantID,
		Email:    email,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.config.TokenExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    issuer,
			Subject:   userID,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.config.JWTSecret))
}

// ValidateToken validates a JWT token using the shared secret.
// Accepts tokens from any service that uses the same secret+issuer.
func (s *Service) ValidateToken(tokenString string) (*Claims, error) {
	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}),
	}

	// If issuer is configured, enforce it
	if s.config.JWTIssuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(s.config.JWTIssuer))
	}

	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(s.config.JWTSecret), nil
	}, parserOpts...)

	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
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
func (s *Service) AuthenticateBasic(ctx context.Context, email, password string) (string, string, error) {
	// Look up user in mail_accounts by email
	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return "", "", fmt.Errorf("auth: lookup error: %w", err)
	}
	if user == nil {
		return "", "", fmt.Errorf("auth: user not found")
	}

	// Check app passwords for this user
	appPasswords, err := s.store.GetAppPasswordsByUser(ctx, user.ID)
	if err != nil {
		return "", "", fmt.Errorf("auth: app password lookup error: %w", err)
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

	return "", "", fmt.Errorf("auth: invalid credentials")
}

// ResolveUserFromToken looks up the user in mail_accounts by the JWT claims.
// Tries email first (most reliable), then falls back to user_id/subject.
// This ensures the user still exists and sets tenant_id = user.ID.
func (s *Service) ResolveUserFromToken(ctx context.Context, claims *Claims) (*Claims, error) {
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
		user, err = s.store.GetUserByID(ctx, claims.UserID)
		if err != nil {
			return nil, fmt.Errorf("auth: user resolve by id error: %w", err)
		}
	}

	if user == nil {
		return nil, fmt.Errorf("auth: user not found in mail_accounts")
	}

	// Always use canonical values from mail_accounts
	// tenant_id == user.ID (mail_accounts has no tenant_id)
	claims.UserID = user.ID
	claims.TenantID = user.ID
	claims.Email = user.Email
	return claims, nil
}

// ConstantTimeCompare performs constant-time string comparison.
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
