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

// JMAPAuth provides JMAP authentication (Bearer + Basic).
func JMAPAuth(authSvc *auth.Service, s *store.Store) fiber.Handler {
	return DAVAuth(authSvc, s) // Same behavior for now
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

// CORSHeaders adds CORS headers for JMAP endpoints.
func CORSHeaders() fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Set("Access-Control-Allow-Origin", "*")
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
