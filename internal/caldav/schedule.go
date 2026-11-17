package caldav

import (
	"fmt"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/availability"
	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

const (
	fbTimeFormat    = "20060102T150405Z"
	maxFreeBusySpan = 62 * 24 * time.Hour
	maxFreeBusyAtts = 50
)

// SetAvailability enables cross-user free/busy (schedule outbox).
func (h *Handler) SetAvailability(a *availability.Service) { h.avail = a }

func isScheduleOutbox(path string) bool {
	p := splitPath(path)
	return len(p) == 4 && p[0] == "dav" && p[3] == "outbox"
}

func isScheduleInbox(path string) bool {
	p := splitPath(path)
	return len(p) == 4 && p[0] == "dav" && p[3] == "inbox"
}

// pathMatchesUser reports whether /dav/{tenant}/{user}/... is the caller's own space.
func pathMatchesUser(path string, tenantID, userID int) bool {
	p := splitPath(path)
	return len(p) >= 3 && p[1] == fmt.Sprint(tenantID) && p[2] == fmt.Sprint(userID)
}

// writeVFreeBusy renders busy intervals as a VCALENDAR/VFREEBUSY object.
func writeVFreeBusy(from, to time.Time, intervals []availability.Interval, organizer, attendee string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:" + ics.ProdID + "\r\nMETHOD:REPLY\r\nBEGIN:VFREEBUSY\r\n")
	b.WriteString(fmt.Sprintf("UID:%s@caldev\r\n", uuid.New().String()))
	b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", time.Now().UTC().Format(fbTimeFormat)))
	if organizer != "" {
		b.WriteString(fmt.Sprintf("ORGANIZER:mailto:%s\r\n", organizer))
	}
	if attendee != "" {
		b.WriteString(fmt.Sprintf("ATTENDEE:mailto:%s\r\n", attendee))
	}
	b.WriteString(fmt.Sprintf("DTSTART:%s\r\nDTEND:%s\r\n", from.UTC().Format(fbTimeFormat), to.UTC().Format(fbTimeFormat)))

	byType := map[string][]string{}
	for _, iv := range intervals {
		t := "BUSY"
		if iv.Status == availability.StatusTentative {
			t = "BUSY-TENTATIVE"
		}
		byType[t] = append(byType[t], iv.Start.UTC().Format(fbTimeFormat)+"/"+iv.End.UTC().Format(fbTimeFormat))
	}
	for _, t := range []string{"BUSY", "BUSY-TENTATIVE"} {
		if periods := byType[t]; len(periods) > 0 {
			b.WriteString(fmt.Sprintf("FREEBUSY;FBTYPE=%s:%s\r\n", t, strings.Join(periods, ",")))
		}
	}
	b.WriteString("END:VFREEBUSY\r\nEND:VCALENDAR\r\n")
	return b.String()
}

// reportFreeBusy answers a free-busy-query REPORT (RFC 4791 §7.10) for the calendar at path,
// or for all of the caller's calendars when path is not a single calendar. Recurring events
// are expanded and transparent/cancelled/declined events are ignored.
func (h *Handler) reportFreeBusy(c *fiber.Ctx, tenantID, userID int, path string, body []byte) error {
	ctx := c.UserContext()
	email, _ := c.Locals("email").(string)

	from, to, ok := parseFreeBusyRange(string(body))
	if !ok {
		return c.Status(400).SendString("Bad Request: free-busy-query needs a UTC time-range start and end")
	}
	if to.Sub(from) > maxFreeBusySpan {
		return c.Status(400).SendString("Bad Request: time-range too large")
	}

	filter := &models.EventFilter{
		TenantID:       tenantID,
		UserID:         userID,
		ComponentType:  "VEVENT",
		TimeRangeStart: &from,
		TimeRangeEnd:   &to,
	}
	if isCalendarCollection(path) {
		filter.CalendarID = extractCalendarID(path)
	}
	events, err := h.store.QueryEventsWithOverrides(ctx, filter)
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}

	c.Set("Content-Type", "text/calendar; charset=utf-8")
	return c.SendString(writeVFreeBusy(from, to, availability.Compute(events, email, from, to), "", ""))
}

// parseFreeBusyRange reads start/end attributes of the <C:time-range> element.
func parseFreeBusyRange(body string) (time.Time, time.Time, bool) {
	attr := func(name string) (time.Time, bool) {
		i := strings.Index(body, name+`="`)
		if i < 0 {
			return time.Time{}, false
		}
		rest := body[i+len(name)+2:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return time.Time{}, false
		}
		t, err := time.Parse(fbTimeFormat, rest[:j])
		return t, err == nil
	}
	from, ok1 := attr("start")
	to, ok2 := attr("end")
	return from, to, ok1 && ok2 && to.After(from)
}

// Post handles POST to the schedule outbox (RFC 6638 §5): a free-busy request for one or
// more attendees. Each attendee is answered individually; users that do not exist, are
// external, or have not shared availability with the caller all get the same
// "3.7;Invalid Calendar User" status so the endpoint cannot be used to discover accounts.
func (h *Handler) Post(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)
	path := c.Path()

	if !isScheduleOutbox(path) {
		return c.Status(405).SendString("Method Not Allowed")
	}
	if !pathMatchesUser(path, tenantID, userID) {
		return c.Status(403).SendString("Forbidden")
	}
	if h.avail == nil {
		return c.Status(501).SendString("Scheduling is not enabled")
	}
	if !strings.Contains(strings.ToLower(c.Get("Content-Type")), "text/calendar") {
		return c.Status(415).SendString("Unsupported Media Type: expected text/calendar")
	}

	req, err := ics.ParseFreeBusyRequest(string(c.Body()))
	if err != nil {
		return c.Status(400).SendString("Bad Request: " + err.Error())
	}
	if req.End.Sub(req.Start) > maxFreeBusySpan {
		return c.Status(400).SendString("Bad Request: time-range too large")
	}
	if len(req.Attendees) > maxFreeBusyAtts {
		return c.Status(400).SendString("Bad Request: too many attendees")
	}

	viewer, err := h.store.GetUserByID(ctx, userID)
	if err != nil || viewer == nil {
		return c.Status(401).SendString("Unauthorized")
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<C:schedule-response xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
	for _, att := range req.Attendees {
		status, data := "3.7;Invalid Calendar User", ""
		if owner, err := h.store.GetUserByEmail(ctx, att); err == nil && owner != nil {
			if ok, err := h.avail.CanView(ctx, viewer, owner); err == nil && ok {
				if busy, err := h.avail.Busy(ctx, owner, req.Start, req.End); err == nil {
					status = "2.0;Success"
					data = writeVFreeBusy(req.Start, req.End, busy, viewer.Email, att)
				} else {
					status = "5.1;Service unavailable"
				}
			}
		}
		b.WriteString("<C:response><C:recipient><D:href>mailto:" + xmlEscapeStr(att) + "</D:href></C:recipient>")
		b.WriteString("<C:request-status>" + xmlEscapeStr(status) + "</C:request-status>")
		if data != "" {
			b.WriteString("<C:calendar-data>" + xmlEscapeStr(data) + "</C:calendar-data>")
		}
		b.WriteString("</C:response>")
	}
	b.WriteString("</C:schedule-response>")

	c.Set("Content-Type", "application/xml; charset=utf-8")
	return c.Status(200).SendString(b.String())
}
