package sync

import (
	"fmt"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/response"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// Handler provides REST API endpoints for managing calendar subscriptions.
type Handler struct {
	store   *store.Store
	service *Service
}

// NewHandler creates a new subscription API handler.
func NewHandler(s *store.Store, svc *Service) *Handler {
	return &Handler{store: s, service: svc}
}

// List returns all subscriptions for the authenticated user.
// GET /api/subscriptions/
func (h *Handler) List(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)

	subs, err := h.store.GetSubscriptionsByUser(c.UserContext(), tenantID, userID)
	if err != nil {
		return response.Error(c, 500, "failed to list subscriptions")
	}
	if subs == nil {
		subs = []*models.CalendarSubscription{}
	}

	return response.Success(c, "subscriptions retrieved", fiber.Map{
		"subscriptions": subs,
		"help": fiber.Map{
			"google_calendar": "Settings → calendar → Integrate → Secret address in iCal format",
			"outlook":         "Settings → Calendar → Shared calendars → Publish a calendar → ICS link",
			"any_ics":         "Any public or authenticated .ics feed URL",
		},
	})
}

// Create creates a new external calendar subscription.
// POST /api/subscriptions/
func (h *Handler) Create(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)

	var req struct {
		SourceURL       string `json:"source_url"`
		DisplayName     string `json:"display_name"`
		Color           string `json:"color"`
		SourceType      string `json:"source_type"`
		AuthType        string `json:"auth_type"`
		AuthCredentials string `json:"auth_credentials"`
		SyncInterval    int    `json:"sync_interval"`
		CalendarID      string `json:"calendar_id"` // optional — if empty, auto-create
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request body")
	}
	if req.SourceURL == "" {
		return response.Error(c, 400, "source_url is required")
	}

	// Defaults
	if req.SourceType == "" {
		req.SourceType = "ics_url"
	}
	if req.AuthType == "" {
		req.AuthType = "none"
	}
	if req.SyncInterval <= 0 {
		req.SyncInterval = 3600 // 1 hour
	}
	if req.SyncInterval < 300 {
		req.SyncInterval = 300 // minimum 5 minutes
	}
	if req.Color == "" {
		req.Color = "#6B7280"
	}
	if req.DisplayName == "" {
		req.DisplayName = "Imported Calendar"
	}

	// Auto-detect source type from URL
	if req.SourceType == "ics_url" {
		if isGoogleCalendarURL(req.SourceURL) {
			req.SourceType = "google"
		} else if isOutlookURL(req.SourceURL) {
			req.SourceType = "outlook"
		}
	}

	// Create a dedicated calendar for this subscription if not specified
	calendarID := req.CalendarID
	if calendarID == "" {
		cal := &models.Calendar{
			ID:                  uuid.New().String(),
			TenantID:            tenantID,
			UserID:              userID,
			Name:                req.DisplayName,
			Description:         fmt.Sprintf("Imported from %s", req.SourceType),
			Color:               req.Color,
			Timezone:            "UTC",
			IsReadOnly:          true, // external calendars are read-only locally
			SupportedComponents: []string{"VEVENT", "VTODO"},
			MaxResourceSize:     10485760,
			Properties:          []byte("{}"),
			CTag:                "",
			SyncToken:           "",
		}
		if err := h.store.CreateCalendar(c.UserContext(), cal); err != nil {
			return response.Error(c, 500, "failed to create calendar")
		}
		calendarID = cal.ID
	}

	sub := &models.CalendarSubscription{
		ID:              uuid.New().String(),
		TenantID:        tenantID,
		UserID:          userID,
		CalendarID:      calendarID,
		SourceType:      req.SourceType,
		SourceURL:       req.SourceURL,
		DisplayName:     req.DisplayName,
		Color:           req.Color,
		AuthType:        req.AuthType,
		AuthCredentials: req.AuthCredentials,
		IsEnabled:       true,
		SyncInterval:    req.SyncInterval,
	}

	if err := h.store.CreateSubscription(c.UserContext(), sub); err != nil {
		return response.Error(c, 500, "failed to create subscription")
	}

	// Trigger immediate first sync in background
	go func() {
		if err := h.service.SyncSubscription(c.UserContext(), sub); err != nil {
			// Error is already recorded in the sync state
			_ = err
		}
	}()

	return response.Created(c, "Subscription created. First sync is running in the background.", fiber.Map{
		"subscription": sub,
		"calendar_id":  calendarID,
	})
}

// Get returns a single subscription.
// GET /api/subscriptions/:id
func (h *Handler) Get(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	subID := c.Params("id")

	sub, err := h.store.GetSubscription(c.UserContext(), tenantID, subID)
	if err != nil {
		return response.Error(c, 500, "failed to get subscription")
	}
	if sub == nil {
		return response.Error(c, 404, "subscription not found")
	}

	return response.Success(c, "subscription retrieved", fiber.Map{"subscription": sub})
}

// Update modifies a subscription.
// PUT /api/subscriptions/:id
func (h *Handler) Update(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	subID := c.Params("id")

	existing, err := h.store.GetSubscription(c.UserContext(), tenantID, subID)
	if err != nil || existing == nil {
		return response.Error(c, 404, "subscription not found")
	}

	var req struct {
		DisplayName     *string `json:"display_name"`
		Color           *string `json:"color"`
		SourceURL       *string `json:"source_url"`
		AuthType        *string `json:"auth_type"`
		AuthCredentials *string `json:"auth_credentials"`
		IsEnabled       *bool   `json:"is_enabled"`
		SyncInterval    *int    `json:"sync_interval"`
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request body")
	}

	if req.DisplayName != nil {
		existing.DisplayName = *req.DisplayName
	}
	if req.Color != nil {
		existing.Color = *req.Color
	}
	if req.SourceURL != nil {
		existing.SourceURL = *req.SourceURL
	}
	if req.AuthType != nil {
		existing.AuthType = *req.AuthType
	}
	if req.AuthCredentials != nil {
		existing.AuthCredentials = *req.AuthCredentials
	}
	if req.IsEnabled != nil {
		existing.IsEnabled = *req.IsEnabled
	}
	if req.SyncInterval != nil {
		if *req.SyncInterval < 300 {
			*req.SyncInterval = 300
		}
		existing.SyncInterval = *req.SyncInterval
	}

	if err := h.store.UpdateSubscription(c.UserContext(), existing); err != nil {
		return response.Error(c, 500, "failed to update subscription")
	}

	return response.Success(c, "subscription updated", fiber.Map{"subscription": existing})
}

// Delete removes a subscription and optionally its calendar.
// DELETE /api/subscriptions/:id?delete_calendar=true
func (h *Handler) Delete(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	subID := c.Params("id")
	deleteCalendar := c.Query("delete_calendar") == "true"

	sub, err := h.store.GetSubscription(c.UserContext(), tenantID, subID)
	if err != nil || sub == nil {
		return response.Error(c, 404, "subscription not found")
	}

	if err := h.store.DeleteSubscription(c.UserContext(), tenantID, subID); err != nil {
		return response.Error(c, 500, "failed to delete subscription")
	}

	if deleteCalendar {
		// Also delete the linked calendar and all its events
		_ = h.store.DeleteCalendar(c.UserContext(), tenantID, sub.CalendarID)
	}

	return c.SendStatus(204)
}

// SyncNow triggers an immediate sync for a subscription.
// POST /api/subscriptions/:id/sync
func (h *Handler) SyncNow(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	subID := c.Params("id")

	sub, err := h.store.GetSubscription(c.UserContext(), tenantID, subID)
	if err != nil || sub == nil {
		return response.Error(c, 404, "subscription not found")
	}

	// Run sync synchronously so the user gets immediate feedback
	if err := h.service.SyncSubscription(c.UserContext(), sub); err != nil {
		return response.Error(c, 500, "sync failed", err.Error())
	}

	// Refresh subscription state
	updated, _ := h.store.GetSubscription(c.UserContext(), tenantID, subID)
	if updated == nil {
		updated = sub
	}

	return response.Success(c, "Sync completed successfully", fiber.Map{
		"subscription": updated,
	})
}

// ── Helpers ──────────────────────────────────────────────────

func isGoogleCalendarURL(url string) bool {
	return contains(url,
		"calendar.google.com",
		"googleapis.com/calendar",
		"google.com/calendar",
	)
}

func isOutlookURL(url string) bool {
	return contains(url,
		"outlook.live.com",
		"outlook.office365.com",
		"outlook.office.com",
		"live.com/calendar",
	)
}

func contains(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
