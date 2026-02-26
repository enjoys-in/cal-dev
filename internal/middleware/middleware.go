package middleware

import (
	"log"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/auth"
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

// RateLimiter provides basic rate limiting.
func RateLimiter(maxRequests int, window time.Duration) fiber.Handler {
	type client struct {
		count    int
		lastSeen time.Time
	}
	clients := make(map[string]*client)

	return func(c *fiber.Ctx) error {
		ip := c.IP()

		cl, exists := clients[ip]
		if !exists {
			clients[ip] = &client{count: 1, lastSeen: time.Now()}
			return c.Next()
		}

		if time.Since(cl.lastSeen) > window {
			cl.count = 1
			cl.lastSeen = time.Now()
			return c.Next()
		}

		cl.count++
		if cl.count > maxRequests {
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

		// No auth header → challenge
		if authHeader == "" {
			c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
			return c.Status(401).SendString("Authentication required")
		}

		// Try Bearer token first (shared JWT from mail/other services)
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			claims, err := authSvc.ValidateToken(token)
			if err != nil {
				c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
				return c.Status(401).SendString("Invalid token")
			}

			// Resolve user from public.users to get canonical tenant_id
			resolved, err := authSvc.ResolveUserFromToken(c.UserContext(), claims)
			if err != nil {
				c.Set("WWW-Authenticate", `Basic realm="CalDAV Server"`)
				return c.Status(401).SendString("User not found")
			}

			c.Locals("user_id", resolved.UserID)
			c.Locals("tenant_id", resolved.TenantID)
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
			c.Locals("tenant_id", tenantID)
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

		if token == "" {
			return c.Status(401).JSON(fiber.Map{"error": "Authentication required"})
		}
		claims, err := authSvc.ValidateToken(token)
		if err != nil {
			return c.Status(401).JSON(fiber.Map{"error": "Invalid token"})
		}

		resolved, err := authSvc.ResolveUserFromToken(c.UserContext(), claims)
		if err != nil {
			return c.Status(401).JSON(fiber.Map{"error": "User not found"})
		}

		c.Locals("user_id", resolved.UserID)
		c.Locals("tenant_id", resolved.TenantID)
		c.Locals("email", resolved.Email)
		return c.Next()
	}
}

// TenantResolver resolves tenant from path and sets locals.
func TenantResolver(s *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		tenantSlug := extractTenantFromPath(c.Path())
		if tenantSlug == "" {
			return c.Next()
		}

		tenant, err := s.GetTenantBySlug(c.UserContext(), tenantSlug)
		if err != nil {
			return c.Status(500).SendString("Internal Server Error")
		}
		if tenant != nil {
			c.Locals("tenant_id", tenant.ID)
			c.Locals("tenant_slug", tenant.Slug)
		}

		return c.Next()
	}
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

// CORSHeaders adds CORS headers for JMAP endpoints.
func CORSHeaders() fiber.Handler {
	allowedOrigins := map[string]bool{
		"http://localhost:9500":       true,
		"https://localhost:9500":      true,
		"https://airsend.in":          true,
		"https://calender.airsend.in": true,
	}

	return func(c *fiber.Ctx) error {
		origin := c.Get("Origin")
		if allowedOrigins[origin] {
			c.Set("Access-Control-Allow-Origin", origin)
		}
		c.Set("Access-Control-Allow-Credentials", "true")
		c.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PROPFIND, PROPPATCH, REPORT, MKCALENDAR, MOVE, COPY, LOCK, UNLOCK")
		c.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Depth, If-Match, If-None-Match, Lock-Token, Destination, Overwrite, Prefer, Brief")
		c.Set("Access-Control-Expose-Headers", "ETag, DAV, Allow, Lock-Token")
		c.Set("Access-Control-Max-Age", "86400")

		if c.Method() == "OPTIONS" {
			return c.SendStatus(204)
		}

		return c.Next()
	}
}
