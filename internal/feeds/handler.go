package feeds

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/response"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/gofiber/fiber/v2"
)

// Handler provides ICS feed endpoints for Outlook and other clients
// that subscribe to .ics URLs but don't support CalDAV.
type Handler struct {
	store *store.Store
}

// NewHandler creates a new feeds handler.
func NewHandler(s *store.Store) *Handler {
	return &Handler{store: s}
}

// CalendarFeed returns a full .ics feed for a single calendar.
// GET /feeds/{tenant}/{user}/{calendar}.ics
//
// Outlook: File → Account Settings → Internet Calendars → New → paste URL.
// Google Calendar: Other calendars → From URL → paste URL.
func (h *Handler) CalendarFeed(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	calendarParam := c.Params("calendar")

	// Strip .ics extension if present
	calendarID := strings.TrimSuffix(calendarParam, ".ics")

	// Get calendar metadata
	cal, err := h.store.GetCalendar(c.UserContext(), tenantID, calendarID)
	if err != nil || cal == nil {
		return c.Status(404).SendString("Calendar not found")
	}

	// Get all events in the calendar
	events, err := h.store.GetEventsByCalendar(c.UserContext(), tenantID, calendarID)
	if err != nil {
		return c.Status(500).SendString("Internal server error")
	}

	ics := buildCalendarICS(cal, events)

	// Caching headers — Outlook polls every few hours
	etag := computeFeedETag(cal.CTag)
	c.Set("Content-Type", "text/calendar; charset=utf-8")
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.ics"`, sanitizeFilename(cal.Name)))
	c.Set("ETag", fmt.Sprintf(`"%s"`, etag))
	c.Set("Cache-Control", "no-cache, must-revalidate")
	c.Set("X-WR-CALNAME", cal.Name)
	c.Set("X-WR-CALDESC", cal.Description)

	// Conditional GET support — if client sends If-None-Match
	if match := c.Get("If-None-Match"); match != "" {
		if strings.Trim(match, `"`) == etag {
			return c.SendStatus(304)
		}
	}

	return c.SendString(ics)
}

// AllCalendarsFeed returns a merged .ics feed of ALL calendars for a user.
// GET /feeds/{tenant}/{user}/all.ics
func (h *Handler) AllCalendarsFeed(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)

	// Get all user calendars
	calendars, err := h.store.GetCalendarsByUser(c.UserContext(), tenantID, userID)
	if err != nil {
		return c.Status(500).SendString("Internal server error")
	}

	// Collect all events across calendars
	var allEvents []*models.Event
	var ctags []string
	for _, cal := range calendars {
		events, err := h.store.GetEventsByCalendar(c.UserContext(), tenantID, cal.ID)
		if err != nil {
			continue
		}
		allEvents = append(allEvents, events...)
		ctags = append(ctags, cal.CTag)
	}

	ics := buildMergedICS(calendars, allEvents)

	etag := computeFeedETag(strings.Join(ctags, ":"))
	c.Set("Content-Type", "text/calendar; charset=utf-8")
	c.Set("Content-Disposition", `attachment; filename="all-calendars.ics"`)
	c.Set("ETag", fmt.Sprintf(`"%s"`, etag))
	c.Set("Cache-Control", "no-cache, must-revalidate")
	c.Set("X-WR-CALNAME", "All Calendars")

	if match := c.Get("If-None-Match"); match != "" {
		if strings.Trim(match, `"`) == etag {
			return c.SendStatus(304)
		}
	}

	return c.SendString(ics)
}

// FeedIndex returns a JSON list of available feeds for the user.
// GET /feeds/{tenant}/{user}/
func (h *Handler) FeedIndex(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)

	calendars, err := h.store.GetCalendarsByUser(c.UserContext(), tenantID, userID)
	if err != nil {
		return response.Error(c, 500, "failed to list calendars")
	}

	type feedInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Color       string `json:"color"`
		FeedURL     string `json:"feed_url"`
	}

	basePath := fmt.Sprintf("/feeds/%d/%d", tenantID, userID)
	feeds := []feedInfo{
		{
			Name:        "All Calendars (merged)",
			Description: "Combined feed of all your calendars",
			FeedURL:     basePath + "/all.ics",
		},
	}

	for _, cal := range calendars {
		feeds = append(feeds, feedInfo{
			Name:        cal.Name,
			Description: cal.Description,
			Color:       cal.Color,
			FeedURL:     fmt.Sprintf("%s/%s.ics", basePath, cal.ID),
		})
	}

	return response.Success(c, "feeds retrieved", fiber.Map{
		"feeds": feeds,
		"help": fiber.Map{
			"outlook": "File → Account Settings → Internet Calendars → New → paste the feed_url",
			"google":  "Other calendars (+) → From URL → paste the feed_url",
			"apple":   "File → New Calendar Subscription → paste the feed_url",
		},
	})
}

// ── ICS Builders ──────────────────────────────────────────────

func buildCalendarICS(cal *models.Calendar, events []*models.Event) string {
	var b strings.Builder

	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//CalDev//Calendar//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("METHOD:PUBLISH\r\n")
	b.WriteString(fmt.Sprintf("X-WR-CALNAME:%s\r\n", escapeICSValue(cal.Name)))
	if cal.Description != "" {
		b.WriteString(fmt.Sprintf("X-WR-CALDESC:%s\r\n", escapeICSValue(cal.Description)))
	}
	if cal.Timezone != "" {
		b.WriteString(fmt.Sprintf("X-WR-TIMEZONE:%s\r\n", cal.Timezone))
	}
	if cal.Color != "" {
		b.WriteString(fmt.Sprintf("X-APPLE-CALENDAR-COLOR:%s\r\n", cal.Color))
	}

	// Refresh interval hint for clients (1 hour)
	b.WriteString("REFRESH-INTERVAL;VALUE=DURATION:PT1H\r\n")
	b.WriteString("X-PUBLISHED-TTL:PT1H\r\n")

	for _, event := range events {
		writeEventComponent(&b, event)
	}

	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

func buildMergedICS(calendars []*models.Calendar, events []*models.Event) string {
	var b strings.Builder

	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//CalDev//AllCalendars//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("METHOD:PUBLISH\r\n")
	b.WriteString("X-WR-CALNAME:All Calendars\r\n")
	b.WriteString("REFRESH-INTERVAL;VALUE=DURATION:PT1H\r\n")
	b.WriteString("X-PUBLISHED-TTL:PT1H\r\n")

	for _, event := range events {
		writeEventComponent(&b, event)
	}

	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

func writeEventComponent(b *strings.Builder, e *models.Event) {
	// If we have raw ICS and it contains VEVENT, extract just the event block
	if e.RawICS != "" {
		startIdx := strings.Index(e.RawICS, "BEGIN:VEVENT")
		endIdx := strings.Index(e.RawICS, "END:VEVENT")
		if startIdx >= 0 && endIdx >= 0 {
			b.WriteString(e.RawICS[startIdx : endIdx+len("END:VEVENT")])
			b.WriteString("\r\n")
			return
		}
	}

	// Fallback: generate from structured data
	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString(fmt.Sprintf("UID:%s\r\n", e.UID))
	b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", formatDT(e.DTStamp)))
	b.WriteString(fmt.Sprintf("DTSTART:%s\r\n", formatDT(e.DTStart)))
	if e.DTEnd != nil {
		b.WriteString(fmt.Sprintf("DTEND:%s\r\n", formatDT(*e.DTEnd)))
	}
	if e.Duration != "" {
		b.WriteString(fmt.Sprintf("DURATION:%s\r\n", e.Duration))
	}
	if e.Summary != "" {
		b.WriteString(fmt.Sprintf("SUMMARY:%s\r\n", escapeICSValue(e.Summary)))
	}
	if e.Description != "" {
		b.WriteString(fmt.Sprintf("DESCRIPTION:%s\r\n", escapeICSValue(e.Description)))
	}
	if e.Location != "" {
		b.WriteString(fmt.Sprintf("LOCATION:%s\r\n", escapeICSValue(e.Location)))
	}
	if e.Status != "" {
		b.WriteString(fmt.Sprintf("STATUS:%s\r\n", e.Status))
	}
	if e.RecurrenceRule != "" {
		b.WriteString(fmt.Sprintf("RRULE:%s\r\n", e.RecurrenceRule))
	}
	if e.Organizer != "" {
		b.WriteString(fmt.Sprintf("ORGANIZER:mailto:%s\r\n", e.Organizer))
	}
	if e.URL != "" {
		b.WriteString(fmt.Sprintf("URL:%s\r\n", e.URL))
	}
	b.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", e.Sequence))
	b.WriteString(fmt.Sprintf("CREATED:%s\r\n", formatDT(e.CreatedAt)))
	b.WriteString(fmt.Sprintf("LAST-MODIFIED:%s\r\n", formatDT(e.UpdatedAt)))
	b.WriteString("END:VEVENT\r\n")
}

// ── Helpers ───────────────────────────────────────────────────

func formatDT(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

func escapeICSValue(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

func computeFeedETag(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h[:8])
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, `"`, "")
	if name == "" {
		name = "calendar"
	}
	return name
}
