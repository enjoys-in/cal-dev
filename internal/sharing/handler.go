// Package sharing exposes team availability (free/busy) and read-only calendar sharing over REST.
package sharing

import (
	"strconv"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/availability"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/recurrence"
	"github.com/enjoys-in/cal-dev/internal/response"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/gofiber/fiber/v2"
)

const (
	maxEmailsPerLookup = 50
	maxAvailWindow     = 62 * 24 * time.Hour
	maxEventsWindow    = 366 * 24 * time.Hour
	maxEventsReturned  = 2000
)

// Handler serves availability and sharing endpoints.
type Handler struct {
	store      *store.Store
	avail      *availability.Service
	orgVisible bool
}

// NewHandler creates the handler. orgVisible mirrors the availability service setting and is
// only reported to clients so they can explain why teammates appear without a share.
func NewHandler(s *store.Store, a *availability.Service, orgVisible bool) *Handler {
	return &Handler{store: s, avail: a, orgVisible: orgVisible}
}

func (h *Handler) me(c *fiber.Ctx) (*models.User, error) {
	userID, _ := c.Locals("user_id").(int)
	return h.store.GetUserByID(c.UserContext(), userID)
}

func parseWindow(c *fiber.Ctx, defaultSpan, maxSpan time.Duration) (time.Time, time.Time, string) {
	from := time.Now().UTC()
	if v := c.Query("start"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return from, from, "start must be RFC 3339"
		}
		from = t
	}
	to := from.Add(defaultSpan)
	if v := c.Query("end"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return from, to, "end must be RFC 3339"
		}
		to = t
	}
	if !to.After(from) {
		return from, to, "end must be after start"
	}
	if to.Sub(from) > maxSpan {
		return from, to, "time window too large"
	}
	return from, to, ""
}

// ============================================================
// GET /api/availability?emails=a@x,b@x&start=...&end=...
// ============================================================

type availabilityResult struct {
	Email   string                  `json:"email"`
	Allowed bool                    `json:"allowed"`
	UserID  int                     `json:"user_id,omitempty"`
	Name    string                  `json:"name,omitempty"`
	Current *availability.Current   `json:"current,omitempty"`
	Busy    []availability.Interval `json:"busy"`
}

// Lookup returns busy/free for the requested users. A user that does not exist and one that
// has not shared with the caller are indistinguishable (allowed=false) to prevent account discovery.
func (h *Handler) Lookup(c *fiber.Ctx) error {
	viewer, err := h.me(c)
	if err != nil || viewer == nil {
		return response.Error(c, 401, "user not found")
	}
	from, to, msg := parseWindow(c, 7*24*time.Hour, maxAvailWindow)
	if msg != "" {
		return response.Error(c, 400, msg)
	}

	var emails []string
	for _, e := range strings.Split(c.Query("emails"), ",") {
		if e = strings.TrimSpace(e); e != "" {
			emails = append(emails, e)
		}
	}
	if len(emails) == 0 {
		return response.Error(c, 400, "emails is required (comma separated)")
	}
	if len(emails) > maxEmailsPerLookup {
		return response.Error(c, 400, "too many emails (max "+strconv.Itoa(maxEmailsPerLookup)+")")
	}

	now := time.Now()
	results := make([]availabilityResult, 0, len(emails))
	for _, email := range emails {
		res := availabilityResult{Email: email, Busy: []availability.Interval{}}
		owner, err := h.store.GetUserByEmail(c.UserContext(), email)
		if err != nil {
			return response.Error(c, 500, "lookup failed")
		}
		ok := false
		if owner != nil {
			if ok, err = h.avail.CanView(c.UserContext(), viewer, owner); err != nil {
				return response.Error(c, 500, "lookup failed")
			}
		}
		if ok {
			busy, err := h.avail.Busy(c.UserContext(), owner, from, to)
			if err != nil {
				return response.Error(c, 500, "lookup failed")
			}
			cur := availability.StatusAt(busy, now)
			res.Allowed = true
			res.UserID = owner.ID
			res.Name = owner.Name
			res.Current = &cur
			if busy != nil {
				res.Busy = busy
			}
		}
		results = append(results, res)
	}
	return response.Success(c, "availability retrieved", fiber.Map{
		"start":   from.UTC().Format(time.RFC3339),
		"end":     to.UTC().Format(time.RFC3339),
		"results": results,
	})
}

// ============================================================
// Availability shares
// ============================================================

// ListAvailabilityShares returns who the caller shares with and who shares with the caller.
func (h *Handler) ListAvailabilityShares(c *fiber.Ctx) error {
	me, err := h.me(c)
	if err != nil || me == nil {
		return response.Error(c, 401, "user not found")
	}
	byMe, err := h.store.ListAvailabilitySharedByOwner(c.UserContext(), me.ID)
	if err != nil {
		return response.Error(c, 500, "failed to list shares")
	}
	withMe, err := h.store.ListAvailabilitySharedWith(c.UserContext(), me.ID)
	if err != nil {
		return response.Error(c, 500, "failed to list shares")
	}
	return response.Success(c, "shares retrieved", fiber.Map{
		"shared_by_me":   nonNil(byMe),
		"shared_with_me": nonNil(withMe),
		"org_visible":    h.orgVisible && me.TenantName != "",
	})
}

// CreateAvailabilityShare lets another user see the caller's free/busy.
func (h *Handler) CreateAvailabilityShare(c *fiber.Ctx) error {
	me, err := h.me(c)
	if err != nil || me == nil {
		return response.Error(c, 401, "user not found")
	}
	grantee, ok := h.granteeFromBody(c, me)
	if !ok {
		return nil
	}
	if err := h.store.CreateAvailabilityShare(c.UserContext(), me.ID, grantee.ID); err != nil {
		return response.Error(c, 500, "failed to share availability")
	}
	return response.Created(c, "availability shared", fiber.Map{"user_id": grantee.ID, "email": grantee.Email})
}

// DeleteAvailabilityShare stops sharing the caller's free/busy with a user.
func (h *Handler) DeleteAvailabilityShare(c *fiber.Ctx) error {
	me, err := h.me(c)
	if err != nil || me == nil {
		return response.Error(c, 401, "user not found")
	}
	granteeID, err := strconv.Atoi(c.Params("user_id"))
	if err != nil {
		return response.Error(c, 400, "invalid user_id")
	}
	if _, err := h.store.DeleteAvailabilityShare(c.UserContext(), me.ID, granteeID); err != nil {
		return response.Error(c, 500, "failed to remove share")
	}
	return c.SendStatus(204)
}

// ============================================================
// Calendar (read-only) shares
// ============================================================

func (h *Handler) ownedCalendar(c *fiber.Ctx, me *models.User) (*models.Calendar, bool) {
	cal, err := h.store.GetCalendar(c.UserContext(), me.ID, c.Params("id"))
	if err != nil || cal == nil || cal.UserID != me.ID {
		_ = response.Error(c, 404, "calendar not found")
		return nil, false
	}
	return cal, true
}

// ListCalendarShares lists who the caller's calendar is shared with.
func (h *Handler) ListCalendarShares(c *fiber.Ctx) error {
	me, err := h.me(c)
	if err != nil || me == nil {
		return response.Error(c, 401, "user not found")
	}
	cal, ok := h.ownedCalendar(c, me)
	if !ok {
		return nil
	}
	shares, err := h.store.ListCalendarShares(c.UserContext(), cal.ID, me.ID)
	if err != nil {
		return response.Error(c, 500, "failed to list shares")
	}
	return response.Success(c, "shares retrieved", fiber.Map{"calendar_id": cal.ID, "shares": nonNil(shares)})
}

// CreateCalendarShare shares one of the caller's calendars read-only with another user.
func (h *Handler) CreateCalendarShare(c *fiber.Ctx) error {
	me, err := h.me(c)
	if err != nil || me == nil {
		return response.Error(c, 401, "user not found")
	}
	cal, ok := h.ownedCalendar(c, me)
	if !ok {
		return nil
	}
	grantee, ok := h.granteeFromBody(c, me)
	if !ok {
		return nil
	}
	if err := h.store.CreateCalendarShare(c.UserContext(), cal.ID, me.ID, grantee.ID); err != nil {
		return response.Error(c, 500, "failed to share calendar")
	}
	return response.Created(c, "calendar shared (read-only)", fiber.Map{
		"calendar_id": cal.ID, "user_id": grantee.ID, "email": grantee.Email, "privilege": "read",
	})
}

// DeleteCalendarShare revokes a calendar share.
func (h *Handler) DeleteCalendarShare(c *fiber.Ctx) error {
	me, err := h.me(c)
	if err != nil || me == nil {
		return response.Error(c, 401, "user not found")
	}
	cal, ok := h.ownedCalendar(c, me)
	if !ok {
		return nil
	}
	granteeID, err := strconv.Atoi(c.Params("user_id"))
	if err != nil {
		return response.Error(c, 400, "invalid user_id")
	}
	if _, err := h.store.DeleteCalendarShare(c.UserContext(), cal.ID, me.ID, granteeID); err != nil {
		return response.Error(c, 500, "failed to remove share")
	}
	return c.SendStatus(204)
}

// ListSharedCalendars lists calendars other users shared with the caller.
func (h *Handler) ListSharedCalendars(c *fiber.Ctx) error {
	me, err := h.me(c)
	if err != nil || me == nil {
		return response.Error(c, 401, "user not found")
	}
	cals, err := h.store.ListCalendarsSharedWith(c.UserContext(), me.ID)
	if err != nil {
		return response.Error(c, 500, "failed to list shared calendars")
	}
	if cals == nil {
		cals = []*store.SharedCalendar{}
	}
	return response.Success(c, "shared calendars retrieved", fiber.Map{"calendars": cals})
}

type sharedEvent struct {
	ID        string    `json:"id"`
	UID       string    `json:"uid"`
	Title     string    `json:"title"`
	Location  string    `json:"location,omitempty"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	AllDay    bool      `json:"is_all_day"`
	Status    string    `json:"status"`
	Recurring bool      `json:"recurring"`
	Private   bool      `json:"private,omitempty"`
}

// SharedCalendarEvents returns expanded occurrences of a calendar shared with the caller.
// PRIVATE/CONFIDENTIAL events are shown as plain "Busy" blocks without details.
func (h *Handler) SharedCalendarEvents(c *fiber.Ctx) error {
	me, err := h.me(c)
	if err != nil || me == nil {
		return response.Error(c, 401, "user not found")
	}
	cal, err := h.store.GetCalendarSharedWith(c.UserContext(), me.ID, c.Params("id"))
	if err != nil {
		return response.Error(c, 500, "failed to load calendar")
	}
	if cal == nil {
		return response.Error(c, 404, "calendar not found")
	}
	from, to, msg := parseWindow(c, 31*24*time.Hour, maxEventsWindow)
	if msg != "" {
		return response.Error(c, 400, msg)
	}

	events, err := h.store.QueryEventsWithOverrides(c.UserContext(), &models.EventFilter{
		TenantID:       cal.TenantID,
		CalendarID:     cal.ID,
		ComponentType:  "VEVENT",
		TimeRangeStart: &from,
		TimeRangeEnd:   &to,
	})
	if err != nil {
		return response.Error(c, 500, "failed to load events")
	}

	out := make([]sharedEvent, 0, len(events))
	for _, series := range recurrence.Group(events) {
		for _, occ := range series.Expand(from, to, 500) {
			// Each instance is described by the component that defines it: a moved or renamed
			// occurrence shows its own title/time, and privacy follows that component's CLASS.
			e := occ.Event
			private := strings.EqualFold(e.Classification, "PRIVATE") || strings.EqualFold(e.Classification, "CONFIDENTIAL")
			se := sharedEvent{
				ID: series.Master.ID, UID: e.UID, Start: occ.Start.UTC(), End: occ.End.UTC(), AllDay: e.AllDay,
				Status: e.Status, Recurring: series.Master.RecurrenceRule != "",
			}
			if private {
				se.Title, se.Private = "Busy", true
			} else {
				se.Title, se.Location = e.Summary, e.Location
			}
			out = append(out, se)
			if len(out) >= maxEventsReturned {
				break
			}
		}
		if len(out) >= maxEventsReturned {
			break
		}
	}
	return response.Success(c, "shared events retrieved", fiber.Map{
		"calendar": cal, "events": out,
		"start": from.UTC().Format(time.RFC3339), "end": to.UTC().Format(time.RFC3339),
	})
}

// ============================================================
// helpers
// ============================================================

// granteeFromBody resolves {"email": "..."} to an active user other than the caller.
// It writes the error response itself and returns ok=false on failure.
func (h *Handler) granteeFromBody(c *fiber.Ctx, me *models.User) (*models.User, bool) {
	var req struct {
		Email string `json:"email"`
	}
	if err := c.BodyParser(&req); err != nil || strings.TrimSpace(req.Email) == "" {
		_ = response.Error(c, 400, "email is required")
		return nil, false
	}
	grantee, err := h.store.GetUserByEmail(c.UserContext(), req.Email)
	if err != nil {
		_ = response.Error(c, 500, "lookup failed")
		return nil, false
	}
	if grantee == nil || !strings.EqualFold(grantee.Status, "ACTIVE") {
		_ = response.Error(c, 404, "user not found")
		return nil, false
	}
	if grantee.ID == me.ID {
		_ = response.Error(c, 400, "cannot share with yourself")
		return nil, false
	}
	return grantee, true
}

func nonNil(s []store.ShareInfo) []store.ShareInfo {
	if s == nil {
		return []store.ShareInfo{}
	}
	return s
}
