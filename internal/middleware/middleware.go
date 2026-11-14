package middleware

import (
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/enjoys-in/cal-dev/internal/auth"
	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/gofiber/fiber/v2"
)

// SecurityHeaders adds required security headers (HSTS, etc.).
func SecurityHeaders() fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("X-Frame-Options", "DENY")
		c.Set("X-XSS-Protection", "1; mode=block")
		c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Set("Content-Security-Policy", "default-src 'self'")
		return c.Next()
	}
}

// RequestLogger logs HTTP requests.
func RequestLogger() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		duration := time.Since(start)
		log.Printf("[%s] %s %s %d %s",
			c.Method(), c.Path(), c.IP(), c.Response().StatusCode(), duration)
		return err
	}
}

// RateLimiter provides basic rate limiting with thread-safe client tracking.
func RateLimiter(maxRequests int, window time.Duration) fiber.Handler {
	type client struct {
		count    int
		lastSeen time.Time
	}
	clients := make(map[string]*client)
	var mu sync.Mutex

	return func(c *fiber.Ctx) error {
		ip := c.IP()

		mu.Lock()
		cl, exists := clients[ip]
		if !exists {
			clients[ip] = &client{count: 1, lastSeen: time.Now()}
			mu.Unlock()
			return c.Next()
		}

		if time.Since(cl.lastSeen) > window {
			cl.count = 1
			cl.lastSeen = time.Now()
			mu.Unlock()
			return c.Next()
		}

		cl.count++
		count := cl.count
		mu.Unlock()

		if count > maxRequests {
			return c.Status(429).SendString("Too Many Requests")
		}

		return c.Next()
	}
}

// DAVAuth provides WebDAV authentication (Bearer JWT + Basic Auth with app passwords).
// JWT tokens are verified using the shared secret/issuer from the mail system.
// Basic Auth uses email + app-password (from caldev.app_passwords).
func DAVAuth(authSvc *auth.Service, s *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		authHeader := c.Get("Authorization")
		fromCookie := false

		// Try access_token cookie if no Authorization header
		if authHeader == "" {
			if cookieToken := c.Cookies("access_token"); cookieToken != "" {
				authHeader = "Bearer " + cookieToken
				fromCookie = true
			}
		}

		// No auth at all → Basic challenge (for native CalDAV clients)
		if authHeader == "" {
			c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
			return c.Status(401).SendString("Authentication required")
		}

		// Try Bearer token first (shared JWT from mail/other services)
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			claims, err := authSvc.ValidateToken(token)
			if err != nil {
				code, msg := rejectBearer(c, "validate", err, "")
				// Browser/API request — don't trigger Basic Auth dialog
				if fromCookie || c.Get("Origin") != "" {
					return c.Status(401).JSON(fiber.Map{"error": msg, "code": code})
				}
				c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
				return c.Status(401).SendString("Invalid token")
			}

			// Resolve user from mail_accounts to get canonical IDs
			resolved, err := authSvc.ResolveUserFromToken(c.UserContext(), claims)
			if err != nil {
				code, msg := rejectBearer(c, "resolve", err, claims.Email)
				if fromCookie || c.Get("Origin") != "" {
					return c.Status(401).JSON(fiber.Map{"error": msg, "code": code})
				}
				c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
				return c.Status(401).SendString("User not found")
			}

			c.Locals("user_id", resolved.UserID)
			c.Locals("tenant_id", resolveTenantID(c, authSvc, resolved.TenantID))
			c.Locals("email", resolved.Email)
			return c.Next()
		}

		// Try Basic Auth (email + app-password)
		if strings.HasPrefix(authHeader, "Basic ") {
			result, err := auth.ParseBasicAuth(authHeader)
			if err != nil || !result.Valid {
				c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
				return c.Status(401).SendString("Invalid credentials")
			}

			userID, tenantID, err := authSvc.AuthenticateBasic(c.UserContext(), result.Email, result.Password)
			if err != nil {
				c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
				return c.Status(401).SendString("Invalid credentials")
			}

			c.Locals("user_id", userID)
			c.Locals("tenant_id", resolveTenantID(c, authSvc, tenantID))
			c.Locals("email", result.Email)
			return c.Next()
		}

		c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
		return c.Status(401).SendString("Unsupported authentication method")
	}
}

// JMAPAuth provides JMAP authentication (Bearer JWT only, no Basic Auth).
// Accepts token from: Authorization: Bearer <token> header OR access_token cookie.
func JMAPAuth(authSvc *auth.Service, s *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var token string

		// 1. Try Authorization header first
		if authHeader := c.Get("Authorization"); authHeader != "" {
			if !strings.HasPrefix(authHeader, "Bearer ") {
				return c.Status(401).JSON(fiber.Map{"error": "Bearer token required"})
			}
			token = strings.TrimPrefix(authHeader, "Bearer ")
		}

		// 2. Fall back to access_token cookie
		if token == "" {
			token = c.Cookies("access_token")
		}

		// 3. Fall back to query parameter (for browser EventSource / SSE)
		if token == "" {
			token = c.Query("access_token")
			if token == "" {
				token = c.Query("token")
			}
		}

		if token == "" {
			return c.Status(401).JSON(fiber.Map{"error": "Authentication required"})
		}
		claims, err := authSvc.ValidateToken(token)
		if err != nil {
			code, msg := rejectBearer(c, "validate", err, "")
			return c.Status(401).JSON(fiber.Map{"error": msg, "code": code})
		}

		resolved, err := authSvc.ResolveUserFromToken(c.UserContext(), claims)
		if err != nil {
			code, msg := rejectBearer(c, "resolve", err, claims.Email)
			return c.Status(401).JSON(fiber.Map{"error": msg, "code": code})
		}

		c.Locals("user_id", resolved.UserID)
		c.Locals("tenant_id", resolveTenantID(c, authSvc, resolved.TenantID))
		c.Locals("email", resolved.Email)
		return c.Next()
	}
}

// rejectBearer logs why a bearer token was refused (never the token itself) and returns a
// stable machine-readable code plus the message clients already receive. The log line is what
// distinguishes "token invalid" from "user has no mail_accounts row" from "account suspended".
func rejectBearer(c *fiber.Ctx, stage string, err error, email string) (code, msg string) {
	code, msg = "invalid_token", "Invalid token"
	switch {
	case errors.Is(err, auth.ErrAccountInactive):
		code, msg = "account_inactive", "User not found"
	case errors.Is(err, auth.ErrUserNotFound):
		code, msg = "user_not_found", "User not found"
	case stage == "resolve":
		code, msg = "user_lookup_failed", "User not found"
	}
	log.Printf("auth: 401 %s %s ip=%s stage=%s code=%s email=%q cause=%v",
		c.Method(), c.Path(), c.IP(), stage, code, email, err)
	return code, msg
}

// XMLSizeLimit limits XML request body size.
func XMLSizeLimit(maxSize int) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if len(c.Body()) > maxSize {
			return c.Status(413).SendString("Request body too large")
		}
		return c.Next()
	}
}

// resolveTenantID determines the tenant ID.
// Authenticated user identity (fallback) is authoritative and cannot be overridden by untrusted request headers.
func resolveTenantID(c *fiber.Ctx, authSvc *auth.Service, fallback int) int {
	// If already authenticated from token/user, use the authenticated tenant ID
	if fallback > 0 {
		return fallback
	}

	// 1. Explicit header (only if not authenticated via user token)
	if tid := c.Get("X-Tenant-ID"); tid != "" {
		if n, err := strconv.Atoi(tid); err == nil {
			return n
		}
	}

	// 2. access_token cookie → decode JWT → mid claim
	if cookieToken := c.Cookies("access_token"); cookieToken != "" {
		if claims, err := authSvc.ValidateToken(cookieToken); err == nil {
			if claims.MID != "" {
				if n, err := strconv.Atoi(claims.MID); err == nil {
					return n
				}
			}
			if claims.TenantID != "" {
				if n, err := strconv.Atoi(claims.TenantID); err == nil {
					return n
				}
			}
		}
	}

	return fallback
}

// extractTenantFromPath extracts tenant ID/slug from URL path.
func extractTenantFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// Pattern: /dav/{tenant}/{user}/...
	if len(parts) >= 2 && parts[0] == "dav" {
		return parts[1]
	}
	return ""
}

// APIKeyAuth protects admin endpoints with a static API key.
// Key is sent via X-API-Key header.
func APIKeyAuth(apiKey string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if apiKey == "" {
			return c.Status(503).JSON(fiber.Map{"error": "Admin API not configured"})
		}
		key := c.Get("X-API-Key")
		if key == "" {
			return c.Status(401).JSON(fiber.Map{"error": "API key required"})
		}
		if key != apiKey {
			return c.Status(403).JSON(fiber.Map{"error": "Invalid API key"})
		}
		return c.Next()
	}
}

// AppHeaders validates the required application headers:
//   - X-App-Version  (must match configured version)
//   - X-App-Name     (must match configured app name)
//   - x-api-key      (must match configured API key)
//
// Requests missing or mismatching any of these headers are rejected.
func AppHeaders(appCfg *config.AppConfig) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Skip health-check, well-known discovery, EventSource, onboarding, and CalDAV/feed paths from header validation
		path := c.Path()
		if path == "/health" || strings.HasPrefix(path, "/.well-known/") ||
			strings.HasPrefix(path, "/dav/") || strings.HasPrefix(path, "/feeds/") ||
			path == "/jmap/eventsource" ||
			strings.HasPrefix(path, "/api/calendar/onboard") ||
			strings.HasPrefix(path, "/api/users/onboard") {
			return c.Next()
		}

		// Validate x-api-key
		if appCfg.APIKey != "" {
			if key := c.Get("x-api-key"); key != appCfg.APIKey {
				return c.Status(401).JSON(fiber.Map{"error": "Invalid or missing x-api-key"})
			}
		}

		// Validate X-App-Name
		if appCfg.Name != "" {
			if name := c.Get("X-App-Name"); !strings.EqualFold(name, appCfg.Name) {
				return c.Status(400).JSON(fiber.Map{"error": "Invalid or missing X-App-Name"})
			}
		}

		// Validate X-App-Version
		if appCfg.Version != "" {
			if ver := c.Get("X-App-Version"); ver != appCfg.Version {
				return c.Status(400).JSON(fiber.Map{"error": "Invalid or missing X-App-Version"})
			}
		}

		// Validate x-signature
		if appCfg.Signature != "" {
			if sig := c.Get("x-signature"); sig != appCfg.Signature {
				return c.Status(401).JSON(fiber.Map{"error": "Invalid or missing x-signature"})
			}
		}

		return c.Next()
	}
}

// isAllowedOrigin checks if the origin is permitted for CORS.
func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}

	allowed := map[string]bool{
		"http://localhost":            true,
		"https://localhost":           true,
		"http://127.0.0.1":            true,
		"https://127.0.0.1":           true,
		"http://localhost:3000":       true,
		"https://localhost:3000":      true,
		"http://localhost:3001":       true,
		"https://localhost:3001":      true,
		"http://localhost:9500":       true,
		"https://localhost:9500":      true,
		"http://localhost:9503":       true,
		"https://localhost:9503":      true,
		"http://127.0.0.1:3000":       true,
		"http://127.0.0.1:3001":       true,
		"http://127.0.0.1:9500":       true,
		"http://127.0.0.1:9503":       true,
		"https://airsend.in":          true,
		"https://calender.airsend.in": true,
	}
	if allowed[origin] {
		return true
	}

	// Allow any localhost and 127.0.0.1 port in dev
	if strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "https://localhost:") ||
		strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasPrefix(origin, "https://127.0.0.1:") {
		return true
	}

	// Custom allowed origins from environment variable
	if custom := os.Getenv("CORS_ALLOWED_ORIGINS"); custom != "" {
		for _, o := range strings.Split(custom, ",") {
			if strings.TrimSpace(o) == origin {
				return true
			}
		}
	}

	return false
}

// CORSHeaders adds CORS headers for JMAP endpoints.
func CORSHeaders() fiber.Handler {
	return func(c *fiber.Ctx) error {
		origin := c.Get("Origin")

		// Always set Vary so caches distinguish responses by origin.
		c.Set("Vary", "Origin")

		if isAllowedOrigin(origin) {
			c.Set("Access-Control-Allow-Origin", origin)
			c.Set("Access-Control-Allow-Credentials", "true")
			c.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PROPFIND, PROPPATCH, REPORT, MKCALENDAR, MOVE, COPY, LOCK, UNLOCK")
			allowHeaders := "Authorization, Content-Type, Depth, If-Match, If-None-Match, Lock-Token, Destination, Overwrite, Prefer, Brief, X-App-Version, X-App-Name, x-api-key, x-signature, X-Tenant-ID, Accept, Origin, X-Requested-With"
			if reqHeaders := c.Get("Access-Control-Request-Headers"); reqHeaders != "" {
				c.Set("Access-Control-Allow-Headers", allowHeaders+", "+reqHeaders)
			} else {
				c.Set("Access-Control-Allow-Headers", allowHeaders)
			}
			c.Set("Access-Control-Expose-Headers", "ETag, DAV, Allow, Lock-Token")
			c.Set("Access-Control-Max-Age", "86400")
		}

		if c.Method() == "OPTIONS" {
			// A real WebDAV capability probe (no CORS preflight header) must reach the DAV handler so
			// CalDAV clients receive the DAV/Allow headers. Everything else is a browser preflight.
			if strings.HasPrefix(c.Path(), "/dav/") && c.Get("Access-Control-Request-Method") == "" {
				return c.Next()
			}
			return c.SendStatus(204)
		}

		return c.Next()
	}
}
