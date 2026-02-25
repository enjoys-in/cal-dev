package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// Service provides authentication functionality.
// JWT is verified with the same secret and issuer used by the mail/other services.
// CalDAV Basic Auth uses app-passwords stored in caldev.app_passwords.
type Service struct {
	store  *store.Store
	config *config.AuthConfig
}

// NewService creates a new auth service.
func NewService(s *store.Store, cfg *config.AuthConfig) *Service {
	return &Service{store: s, config: cfg}
}

// Claims represents JWT claims (compatible with the shared issuer token format).
type Claims struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
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
func (s *Service) GenerateToken(userID, tenantID, email string) (string, error) {
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
// 1. Looks up user by email in public.users
// 2. Checks the password against caldev.app_passwords
// Returns (userID, tenantID, error).
func (s *Service) AuthenticateBasic(ctx context.Context, email, password string) (string, string, error) {
	// Look up user in public.users by email
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
			return user.ID, user.TenantID, nil
		}
	}

	return "", "", fmt.Errorf("auth: invalid credentials")
}

// ResolveUserFromToken looks up the user in public.users by the JWT claims.
// This ensures the user still exists and fetches current tenant_id.
func (s *Service) ResolveUserFromToken(ctx context.Context, claims *Claims) (*Claims, error) {
	user, err := s.store.GetUserByID(ctx, claims.UserID)
	if err != nil {
		return nil, fmt.Errorf("auth: user resolve error: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("auth: user not found in public.users")
	}

	// Always use the canonical tenant_id from public.users
	claims.TenantID = user.TenantID
	claims.Email = user.Email
	return claims, nil
}

// ConstantTimeCompare performs constant-time string comparison.
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
