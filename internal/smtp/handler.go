package smtp

import (
	"log"
	"strings"

	"github.com/enjoys-in/cal-dev/internal/crypto"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/response"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/gofiber/fiber/v2"
)

// Handler handles HTTP requests for Calendar BYO SMTP configuration.
type Handler struct {
	store     *store.Store
	jwtSecret string
}

// NewHandler creates a new SMTP handler.
func NewHandler(s *store.Store, jwtSecret string) *Handler {
	return &Handler{
		store:     s,
		jwtSecret: jwtSecret,
	}
}

// GetSMTP returns the SMTP settings for a calendar (password masked).
// GET /api/calendars/:id/smtp
func (h *Handler) GetSMTP(c *fiber.Ctx) error {
	calendarID := c.Params("id")
	tenantID, _ := c.Locals("tenant_id").(int)

	if calendarID == "" {
		return response.Error(c, 400, "calendar_id is required")
	}

	cfg, err := h.store.GetCalendarSMTP(c.UserContext(), tenantID, calendarID)
	if err != nil {
		log.Printf("smtp: load config for calendar %s: %v", calendarID, err)
		return response.Error(c, 500, "failed to load SMTP config")
	}

	if cfg == nil {
		return response.Success(c, "No SMTP configured for this calendar", fiber.Map{
			"configured":  false,
			"calendar_id": calendarID,
		})
	}

	return response.Success(c, "SMTP configuration loaded", fiber.Map{
		"configured":     true,
		"id":             cfg.ID,
		"calendar_id":    cfg.CalendarID,
		"host":           cfg.Host,
		"port":           cfg.Port,
		"username":       cfg.Username,
		"from_email":     cfg.FromEmail,
		"from_name":      cfg.FromName,
		"reply_to":       cfg.ReplyTo,
		"encryption":     cfg.Encryption,
		"status":         cfg.Status,
		"last_tested_at": cfg.LastTestedAt,
		"last_error":     cfg.LastError,
	})
}

// UpsertSMTP creates or updates the SMTP settings for a calendar.
// PUT /api/calendars/:id/smtp
func (h *Handler) UpsertSMTP(c *fiber.Ctx) error {
	calendarID := c.Params("id")
	tenantID, _ := c.Locals("tenant_id").(int)
	userID, _ := c.Locals("user_id").(int)

	if calendarID == "" {
		return response.Error(c, 400, "calendar_id is required")
	}

	// Verify calendar exists and belongs to tenant
	cal, err := h.store.GetCalendar(c.UserContext(), tenantID, calendarID)
	if err != nil || cal == nil {
		return response.Error(c, 404, "calendar not found")
	}

	var req struct {
		Host       string `json:"host"`
		Port       int    `json:"port"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		FromEmail  string `json:"from_email"`
		FromName   string `json:"from_name"`
		ReplyTo    string `json:"reply_to"`
		Encryption string `json:"encryption"`
	}

	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request body", err.Error())
	}

	req.Host = strings.TrimSpace(req.Host)
	req.Username = strings.TrimSpace(req.Username)
	req.FromEmail = strings.TrimSpace(req.FromEmail)

	if req.Host == "" {
		return response.Error(c, 400, "host is required")
	}
	if req.Username == "" {
		return response.Error(c, 400, "username is required")
	}
	if req.FromEmail == "" {
		return response.Error(c, 400, "from_email is required")
	}
	if req.Port == 0 {
		req.Port = 587
	}
	if req.Port < 1 || req.Port > 65535 {
		return response.Error(c, 400, "port must be between 1 and 65535")
	}
	req.Encryption = strings.ToLower(strings.TrimSpace(req.Encryption))
	if req.Encryption == "" {
		req.Encryption = "starttls"
	}
	if req.Encryption != "none" && req.Encryption != "ssl" && req.Encryption != "starttls" {
		return response.Error(c, 400, "encryption must be one of: none, ssl, starttls")
	}

	key := crypto.DeriveKey(h.jwtSecret)
	var passwordEncrypted string

	if req.Password != "" {
		encrypted, err := crypto.Encrypt(req.Password, key)
		if err != nil {
			log.Printf("smtp: encrypt password for calendar %s: %v", calendarID, err)
			return response.Error(c, 500, "failed to encrypt password")
		}
		passwordEncrypted = encrypted
	} else {
		// If password wasn't sent, keep existing encrypted password
		existing, _ := h.store.GetCalendarSMTP(c.UserContext(), tenantID, calendarID)
		if existing == nil {
			return response.Error(c, 400, "password is required for initial configuration")
		}
		// Keep the saved password only for the same server and account; otherwise the next send
		// would hand it to whatever new host was entered.
		if !strings.EqualFold(existing.Host, req.Host) || existing.Port != req.Port || existing.Username != req.Username {
			return response.Error(c, 400, "password is required when host, port or username changes")
		}
		passwordEncrypted = existing.PasswordEncrypted
	}

	cfg := &models.CalendarSMTP{
		CalendarID:        calendarID,
		TenantID:          tenantID,
		UserID:            userID,
		Host:              req.Host,
		Port:              req.Port,
		Username:          req.Username,
		PasswordEncrypted: passwordEncrypted,
		FromEmail:         req.FromEmail,
		FromName:          strings.TrimSpace(req.FromName),
		ReplyTo:           strings.TrimSpace(req.ReplyTo),
		Encryption:        strings.ToLower(req.Encryption),
		Status:            "active",
	}

	if err := h.store.UpsertCalendarSMTP(c.UserContext(), cfg); err != nil {
		log.Printf("smtp: save config for calendar %s: %v", calendarID, err)
		return response.Error(c, 500, "failed to save SMTP settings")
	}

	return response.Success(c, "SMTP configuration saved", fiber.Map{
		"configured":  true,
		"id":          cfg.ID,
		"calendar_id": cfg.CalendarID,
		"host":        cfg.Host,
		"port":        cfg.Port,
		"username":    cfg.Username,
		"from_email":  cfg.FromEmail,
		"from_name":   cfg.FromName,
		"encryption":  cfg.Encryption,
		"status":      cfg.Status,
	})
}

// TestSMTP tests live connectivity and authentication with the SMTP server.
// POST /api/calendars/:id/smtp/test
func (h *Handler) TestSMTP(c *fiber.Ctx) error {
	calendarID := c.Params("id")
	tenantID, _ := c.Locals("tenant_id").(int)

	var req struct {
		Host       string `json:"host"`
		Port       int    `json:"port"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		FromEmail  string `json:"from_email"`
		Encryption string `json:"encryption"`
	}

	// Try to parse body if provided (testing unsaved values from modal)
	_ = c.BodyParser(&req)
	req.Host = strings.TrimSpace(req.Host)
	req.Username = strings.TrimSpace(req.Username)

	var clientCfg Config
	// Only a test of the saved configuration may update its stored status.
	testingSaved := false

	if req.Host != "" && req.Username != "" {
		port := req.Port
		if port <= 0 {
			port = 587
		}
		clientCfg = Config{
			Host:       req.Host,
			Port:       port,
			Username:   req.Username,
			Password:   req.Password,
			FromEmail:  req.FromEmail,
			Encryption: req.Encryption,
		}
		// If password omitted, reuse the saved one only for the same host/port/username,
		// so the stored credential can never be sent to a different server.
		if clientCfg.Password == "" {
			existing, err := h.store.GetCalendarSMTP(c.UserContext(), tenantID, calendarID)
			if err != nil {
				log.Printf("smtp: load config for calendar %s: %v", calendarID, err)
				return response.Error(c, 500, "failed to load SMTP config")
			}
			if existing == nil || !strings.EqualFold(existing.Host, req.Host) || existing.Port != port || existing.Username != req.Username {
				return response.Error(c, 400, "password is required to test settings that differ from the saved configuration")
			}
			decrypted, decErr := crypto.DecryptWithFallback(existing.PasswordEncrypted, h.jwtSecret)
			if decErr != nil {
				log.Printf("smtp: decrypt saved password for calendar %s: %v", calendarID, decErr)
				return response.Error(c, 500, "failed to decrypt saved password")
			}
			clientCfg.Password = decrypted
			testingSaved = strings.EqualFold(strings.TrimSpace(req.Encryption), existing.Encryption)
		}
	} else if calendarID != "" {
		// Use saved configuration
		saved, err := h.store.GetCalendarSMTP(c.UserContext(), tenantID, calendarID)
		if err != nil || saved == nil {
			if err != nil {
				log.Printf("smtp: load config for calendar %s: %v", calendarID, err)
			}
			return response.Error(c, 404, "no saved SMTP configuration found to test")
		}
		decrypted, decErr := crypto.DecryptWithFallback(saved.PasswordEncrypted, h.jwtSecret)
		if decErr != nil {
			log.Printf("smtp: decrypt saved password for calendar %s: %v", calendarID, decErr)
			return response.Error(c, 500, "failed to decrypt saved password")
		}
		testingSaved = true
		clientCfg = Config{
			Host:       saved.Host,
			Port:       saved.Port,
			Username:   saved.Username,
			Password:   decrypted,
			FromEmail:  saved.FromEmail,
			Encryption: saved.Encryption,
		}
	} else {
		return response.Error(c, 400, "host, username, and password required to test SMTP")
	}

	// Attempt connection & handshake
	testErr := TestConnection(&clientCfg)
	if testErr != nil {
		log.Printf("smtp: connection test for calendar %s failed: %v", calendarID, testErr)
		if testingSaved {
			_ = h.store.UpdateCalendarSMTPTestResult(c.UserContext(), tenantID, calendarID, "failing", testErr.Error())
		}
		return response.Error(c, 400, "SMTP connection test failed. Check the host, port, encryption and credentials.")
	}

	if testingSaved {
		_ = h.store.UpdateCalendarSMTPTestResult(c.UserContext(), tenantID, calendarID, "active", "")
	}

	return response.Success(c, "SMTP connection verified successfully", fiber.Map{
		"success": true,
		"host":    clientCfg.Host,
		"port":    clientCfg.Port,
	})
}

// DeleteSMTP removes the SMTP settings for a calendar.
// DELETE /api/calendars/:id/smtp
func (h *Handler) DeleteSMTP(c *fiber.Ctx) error {
	calendarID := c.Params("id")
	tenantID, _ := c.Locals("tenant_id").(int)

	if calendarID == "" {
		return response.Error(c, 400, "calendar_id is required")
	}

	if err := h.store.DeleteCalendarSMTP(c.UserContext(), tenantID, calendarID); err != nil {
		log.Printf("smtp: delete config for calendar %s: %v", calendarID, err)
		return response.Error(c, 500, "failed to delete SMTP settings")
	}

	return c.SendStatus(fiber.StatusNoContent)
}
