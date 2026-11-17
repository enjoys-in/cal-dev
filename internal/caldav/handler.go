package caldav

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/availability"
	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/scheduling"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/enjoys-in/cal-dev/internal/xmlutil"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// Handler provides CalDAV endpoint handlers.
type Handler struct {
	store      *store.Store
	scheduling *scheduling.Service
	avail      *availability.Service
}

// NewHandler creates a new CalDAV handler.
func NewHandler(s *store.Store, sched *scheduling.Service) *Handler {
	return &Handler{store: s, scheduling: sched}
}

// ============================================================
// OPTIONS — Capability Discovery (RFC 4918 §9.1)
// ============================================================

func (h *Handler) Options(c *fiber.Ctx) error {
	c.Set("DAV", "1, 2, 3, calendar-access, calendar-schedule, calendar-auto-schedule")
	c.Set("Allow", "OPTIONS, GET, HEAD, POST, PUT, DELETE, PROPFIND, PROPPATCH, REPORT, MKCALENDAR, MOVE, COPY, LOCK, UNLOCK")
	c.Set("Content-Type", "text/xml; charset=utf-8")
	c.Set("MS-Author-Via", "DAV")
	return c.SendStatus(200)
}

// ============================================================
// PROPFIND — Resource Discovery (RFC 4918 §9.1)
// ============================================================

func (h *Handler) Propfind(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)
	path := c.Path()

	depth := c.Get("Depth", "1")

	// Parse request body
	req, err := xmlutil.ParsePropfindRequest(bytes.NewReader(c.Body()))
	if err != nil {
		body := c.Body()
		log.Printf("PROPFIND parse error on %s: %v (body: %d bytes, starts %q)", path, err, len(body), body[:min(len(body), 200)])
		return c.Status(400).SendString("Bad Request: " + err.Error())
	}
	// nil names means allprop (also for propname and an empty body).
	var names []xml.Name
	if req.AllProp == nil && req.PropName == nil && req.Prop != nil {
		names = xmlutil.RequestedProps(req.Prop)
	}

	var responses []xmlutil.DAVResponse

	// Determine resource type from path
	switch {
	case isWellKnown(path):
		// /.well-known/caldav → redirect to principal
		c.Set("Location", fmt.Sprintf("/dav/%d/%d/", tenantID, userID))
		return c.SendStatus(301)

	case isPrincipal(path):
		// /dav/{tenant}/{user}/ — Principal resource
		email, _ := c.Locals("email").(string)
		responses = h.propfindPrincipal(tenantID, userID, email, path, req)

	case isScheduleInbox(path), isScheduleOutbox(path):
		// RFC 6638 scheduling collections. Invitations are delivered straight into calendars,
		// so the inbox stays empty; the outbox accepts free-busy POSTs.
		kind := "schedule-inbox"
		if isScheduleOutbox(path) {
			kind = "schedule-outbox"
		}
		responses = []xmlutil.DAVResponse{{
			Href: path,
			PropStats: []xmlutil.PropStat{{
				Props: map[string]string{
					"resourcetype": fmt.Sprintf(`<D:resourcetype><D:collection/><C:%s/></D:resourcetype>`, kind),
					"displayname":  fmt.Sprintf(`<D:displayname>%s</D:displayname>`, kind),
				},
				Status: 200,
			}},
		}}

	case isCalendarHome(path):
		// /dav/{tenant}/{user}/calendars/ — Calendar home set
		if err := h.store.EnsureDefaultCalendar(ctx, tenantID, userID); err != nil {
			log.Printf("Warning: auto-provision default calendar failed: %v", err)
		}
		calendars, err := h.store.GetCalendarsByUser(ctx, tenantID, userID)
		if err != nil {
			return c.Status(500).SendString("Internal Server Error")
		}
		responses = h.propfindCalendarHome(tenantID, userID, path, calendars, req, depth)

	case isCalendarCollection(path):
		// /dav/{tenant}/{user}/calendars/{calendar}/ — Calendar collection
		calendarID := extractCalendarID(path)
		cal, err := h.store.GetCalendar(ctx, tenantID, calendarID)
		if err != nil || cal == nil {
			return c.Status(404).SendString("Not Found")
		}

		responses = h.propfindCalendar(tenantID, userID, path, cal, req, depth)

		// If depth=1 include calendar events
		if depth == "1" || depth == "infinity" {
			events, err := h.store.GetResourcesByCalendar(ctx, tenantID, calendarID)
			if err != nil {
				return c.Status(500).SendString("Internal Server Error")
			}
			for _, evt := range events {
				eventPath := fmt.Sprintf("%s%s.ics", path, url.PathEscape(evt.UID))
				responses = append(responses, h.buildEventResponse(eventPath, evt, names))
			}
		}

	case isEventResource(path):
		// /dav/{tenant}/{user}/calendars/{calendar}/{uid}.ics — Event resource
		calendarID := extractCalendarID(path)
		uid := extractEventUID(path)
		evt, err := h.store.GetEvent(ctx, tenantID, calendarID, uid)
		if err != nil || evt == nil {
			return c.Status(404).SendString("Not Found")
		}
		responses = append(responses, h.buildEventResponse(path, evt, names))

	default:
		// Root discovery
		responses = h.propfindRoot(tenantID, userID, path, req)
	}

	for i := range responses {
		responses[i] = selectProps(responses[i], names)
	}

	c.Set("Content-Type", "application/xml; charset=utf-8")
	c.Status(207)

	var buf bytes.Buffer
	if err := xmlutil.WriteMultiStatus(&buf, responses); err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}
	return c.Send(buf.Bytes())
}

// ============================================================
// PROPPATCH — Property Updates (RFC 4918 §9.2)
// ============================================================

func (h *Handler) Proppatch(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	path := c.Path()

	if _, err := xmlutil.ParseProppatch(bytes.NewReader(c.Body())); err != nil {
		return c.Status(400).SendString("Bad Request: " + err.Error())
	}

	if isCalendarCollection(path) {
		calendarID := extractCalendarID(path)
		cal, err := h.store.GetCalendar(ctx, tenantID, calendarID)
		if err != nil || cal == nil {
			return c.Status(404).SendString("Not Found")
		}
		if cal.IsReadOnly {
			return c.Status(403).SendString("Calendar is read-only")
		}

		updatedProps := make(map[string]string)
		failedProps := make(map[string]string) // properties that cannot be set: reported as 403
		// Walk the whole body (not the prop's inner XML, which loses namespace declarations made
		// on outer elements) and visit each property element of propertyupdate/set/prop.
		decoder := xml.NewDecoder(bytes.NewReader(c.Body()))
		var open []xml.Name
		for {
			tok, err := decoder.Token()
			if err != nil {
				break
			}
			if _, ok := tok.(xml.EndElement); ok && len(open) > 0 {
				open = open[:len(open)-1]
				continue
			}
			se, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			if len(open) != 3 || open[1] != (xml.Name{Space: xmlutil.NSDAVUrl, Local: "set"}) || open[2] != (xml.Name{Space: xmlutil.NSDAVUrl, Local: "prop"}) {
				open = append(open, se.Name)
				continue
			}
			var val string
			_ = decoder.DecodeElement(&val, &se)
			local := strings.ToLower(se.Name.Local)
			switch local {
			case "displayname":
				cal.Name = val
				updatedProps["displayname"] = "<D:displayname/>"
			case "calendar-color":
				cal.Color = val
				updatedProps["calendar-color"] = "<A:calendar-color/>"
			case "calendar-description":
				cal.Description = val
				updatedProps["calendar-description"] = "<C:calendar-description/>"
			case "calendar-timezone":
				if name, ok := ics.CalendarTimezoneName(val); ok {
					cal.Timezone = name
					updatedProps["calendar-timezone"] = "<C:calendar-timezone/>"
				} else {
					failedProps["calendar-timezone"] = "<C:calendar-timezone/>"
				}
			case "calendar-order":
				var order int
				if n, _ := fmt.Sscanf(val, "%d", &order); n > 0 {
					cal.SortOrder = order
				}
				updatedProps["calendar-order"] = "<A:calendar-order/>"
			default:
				failedProps[se.Name.Space+" "+se.Name.Local] = emptyPropXML(se.Name)
			}
		}

		if len(updatedProps) > 0 {
			if err := h.store.UpdateCalendar(ctx, cal); err != nil {
				log.Printf("caldav: PROPPATCH update error: %v", err)
				return c.Status(500).SendString("Internal Server Error")
			}
		}

		response := xmlutil.DAVResponse{Href: path}
		if len(updatedProps) > 0 {
			response.PropStats = append(response.PropStats, xmlutil.PropStat{Props: updatedProps, Status: 200})
		}
		if len(failedProps) > 0 {
			response.PropStats = append(response.PropStats, xmlutil.PropStat{Props: failedProps, Status: 403})
		}
		if len(response.PropStats) == 0 {
			response.StatusCode = 200 // nothing to set
		}

		c.Set("Content-Type", "application/xml; charset=utf-8")
		c.Status(207)

		var buf bytes.Buffer
		xmlutil.WriteMultiStatus(&buf, []xmlutil.DAVResponse{response})
		return c.Send(buf.Bytes())
	}

	return c.SendStatus(403)
}

// ============================================================
// REPORT — Calendar Queries (RFC 4791 §7)
// ============================================================

func (h *Handler) Report(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)
	path := c.Path()
	body := c.Body()
	ctx := c.UserContext()

	bodyStr := string(body)

	// Detect report type from body
	switch {
	case strings.Contains(bodyStr, "calendar-query"):
		return h.reportCalendarQuery(c, ctx, tenantID, userID, path, body)
	case strings.Contains(bodyStr, "calendar-multiget"):
		return h.reportCalendarMultiget(c, ctx, tenantID, path, body)
	case strings.Contains(bodyStr, "sync-collection"):
		return h.reportSyncCollection(c, ctx, tenantID, path, body)
	case strings.Contains(bodyStr, "free-busy-query"):
		return h.reportFreeBusy(c, tenantID, userID, path, body)
	default:
		return c.Status(400).SendString("Unsupported REPORT type")
	}
}

func (h *Handler) reportCalendarQuery(c *fiber.Ctx, ctx context.Context, tenantID, userID int, path string, body []byte) error {
	req, err := xmlutil.ParseCalendarQuery(bytes.NewReader(body))
	if err != nil {
		return c.Status(400).SendString("Bad Request: " + err.Error())
	}

	calendarID := extractCalendarID(path)

	filter := &models.EventFilter{
		TenantID:   tenantID,
		CalendarID: calendarID,
		UserID:     userID,
	}

	// Extract filter from CompFilter chain
	if req.Filter != nil {
		if err := h.applyCompFilter(filter, req.Filter); err != nil {
			return c.Status(400).SendString("Bad Request: " + err.Error())
		}
	}

	matched, err := h.store.QueryEvents(ctx, filter)
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}
	// The query matches components (rows); the response is per resource (href), once each, built
	// from the resource's primary row so the calendar-data holds master and overrides together.
	events, err := h.store.PrimaryEventsFor(ctx, tenantID, matched)
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}

	var responses []xmlutil.DAVResponse
	for _, evt := range events {
		// On the calendar home the event's href must include its calendar.
		eventPath := fmt.Sprintf("%s%s/%s.ics", ensureTrailingSlash(path), url.PathEscape(evt.CalendarID), url.PathEscape(evt.UID))
		if isCalendarCollection(path) {
			eventPath = fmt.Sprintf("%s%s.ics", path, url.PathEscape(evt.UID))
		}
		responses = append(responses, h.reportEventResponse(eventPath, evt, req.Prop))
	}

	c.Set("Content-Type", "application/xml; charset=utf-8")
	c.Status(207)

	var buf bytes.Buffer
	xmlutil.WriteMultiStatus(&buf, responses)
	return c.Send(buf.Bytes())
}

func (h *Handler) reportCalendarMultiget(c *fiber.Ctx, ctx context.Context, tenantID int, path string, body []byte) error {
	req, err := xmlutil.ParseCalendarMultiget(bytes.NewReader(body))
	if err != nil {
		return c.Status(400).SendString("Bad Request: " + err.Error())
	}

	calendarID := extractCalendarID(path)
	var responses []xmlutil.DAVResponse

	// One query for all hrefs instead of one per href.
	uids := make([]string, len(req.Hrefs))
	for i, href := range req.Hrefs {
		uids[i] = extractEventUID(href)
	}
	found, lookupErr := h.store.GetPrimaryEventsByUIDs(ctx, tenantID, calendarID, uids)

	for i, href := range req.Hrefs {
		if lookupErr != nil {
			break // as before, hrefs whose lookup failed are left out
		}
		evt := found[uids[i]]
		if evt == nil {
			responses = append(responses, xmlutil.DAVResponse{
				Href:       href,
				StatusCode: 404,
			})
			continue
		}
		responses = append(responses, h.reportEventResponse(href, evt, req.Prop))
	}

	c.Set("Content-Type", "application/xml; charset=utf-8")
	c.Status(207)

	var buf bytes.Buffer
	xmlutil.WriteMultiStatus(&buf, responses)
	return c.Send(buf.Bytes())
}

func (h *Handler) reportSyncCollection(c *fiber.Ctx, ctx context.Context, tenantID int, path string, body []byte) error {
	req, err := xmlutil.ParseSyncCollection(bytes.NewReader(body))
	if err != nil {
		return c.Status(400).SendString("Bad Request: " + err.Error())
	}

	calendarID := extractCalendarID(path)

	// Get changes since the given sync token
	changes, err := h.store.GetSyncChanges(ctx, tenantID, calendarID, req.SyncToken)
	if errors.Is(err, sql.ErrNoRows) {
		// RFC 6578 §3.2: an unknown token makes the client restart with an initial sync.
		c.Set("Content-Type", "application/xml; charset=utf-8")
		return c.Status(403).SendString(`<?xml version="1.0" encoding="UTF-8"?><D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`)
	}
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}

	// A resource is one href however many rows (master + overrides) changed, and a single PUT can
	// log both "deleted" (a dropped override) and "modified" rows for the same UID. So report each
	// UID once, by what exists now: present -> its current state, gone -> 404.
	var responses []xmlutil.DAVResponse
	reported := make(map[string]bool, len(changes))
	var uids []string
	for _, ch := range changes {
		if !reported[ch.EventUID] {
			reported[ch.EventUID] = true
			uids = append(uids, ch.EventUID)
		}
	}
	// One query for all changed UIDs instead of one per UID.
	found, lookupErr := h.store.GetPrimaryEventsByUIDs(ctx, tenantID, calendarID, uids)

	for _, uid := range uids {
		if lookupErr != nil {
			break // as before, changes whose lookup failed are left out
		}
		eventPath := fmt.Sprintf("%s%s.ics", path, url.PathEscape(uid))

		evt := found[uid]
		if evt == nil {
			responses = append(responses, xmlutil.DAVResponse{
				Href:       eventPath,
				StatusCode: 404,
			})
			continue
		}
		responses = append(responses, h.reportEventResponse(eventPath, evt, req.Prop))
	}

	// Get new sync token
	newSyncToken, err := h.store.GetCalendarSyncToken(ctx, tenantID, calendarID)
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}

	c.Set("Content-Type", "application/xml; charset=utf-8")
	c.Status(207)

	var buf bytes.Buffer
	xmlutil.WriteSyncResponse(&buf, responses, newSyncToken)
	return c.Send(buf.Bytes())
}

// ============================================================
// PUT — Create/Update Events (RFC 4791 §5.3.2)
// ============================================================

func (h *Handler) Put(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)
	// Cloned: c.Path() aliases Fiber's reusable request buffer, and calendarID (stored on the
	// event) is used by the scheduling goroutine after this handler has returned.
	path := strings.Clone(c.Path())

	calendarID := extractCalendarID(path)
	uid := extractEventUID(path)

	// The target calendar must belong to the caller and be writable. (Without this a user could
	// write rows pointing at someone else's calendar id, or into a read-only subscription calendar.)
	targetCal, err := h.store.GetCalendar(ctx, tenantID, calendarID)
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}
	if targetCal == nil || targetCal.UserID != userID {
		return c.Status(404).SendString("Calendar not found")
	}
	if targetCal.IsReadOnly {
		return c.Status(403).SendString("Calendar is read-only")
	}

	// Check WebDAV lock (RFC 4918 §9.10)
	if existingLock, err := h.store.GetLock(ctx, tenantID, lockPath(path)); err == nil && existingLock != nil {
		ifHdr := c.Get("If")
		lockHdr := c.Get("Lock-Token")
		if !strings.Contains(ifHdr, existingLock.LockToken) && !strings.Contains(lockHdr, existingLock.LockToken) {
			return c.Status(423).SendString("Locked")
		}
	}

	// Validate content type
	contentType := c.Get("Content-Type")
	if !strings.Contains(contentType, "text/calendar") {
		return c.Status(415).SendString("Unsupported Media Type: expected text/calendar")
	}

	rawICS := string(c.Body())

	// Validate ICS
	if err := ics.ValidateICS(rawICS); err != nil {
		if errors.Is(err, ics.ErrUnsupportedComponent) {
			c.Set("Content-Type", "application/xml; charset=utf-8")
			return c.Status(403).SendString(`<?xml version="1.0" encoding="UTF-8"?><D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><C:supported-calendar-component/></D:error>`)
		}
		return c.Status(400).SendString("Invalid ICS: " + err.Error())
	}

	// Parse every component of the resource: the master series plus one row per
	// RECURRENCE-ID override (they used to be flattened into a single event).
	components, err := ics.ParseICSAll(rawICS, tenantID, userID, calendarID)
	if err != nil {
		return c.Status(400).SendString("Failed to parse ICS: " + err.Error())
	}

	// Enforce maximum attendees limit
	for _, comp := range components {
		var atts []models.Attendee
		if len(comp.Attendees) > 0 {
			if err := json.Unmarshal(comp.Attendees, &atts); err == nil && len(atts) > models.MaxAttendeesPerEvent {
				return c.Status(400).SendString(fmt.Sprintf("Event exceeds maximum limit of %d attendees", models.MaxAttendeesPerEvent))
			}
		}
	}

	// Resources are addressed by UID everywhere (GET/DELETE/hrefs use <uid>.ics), so the body's
	// UID must be the resource name; rewriting it would leave RawICS and invitations with another UID.
	event := components[0] // primary row: the master, or the earliest override
	if uid != "" && event.UID != uid {
		return c.Status(400).SendString("Bad Request: the UID in the calendar data must match the resource name")
	}

	// Check If-Match (ETag precondition) for updates
	ifMatch := c.Get("If-Match")
	ifNoneMatch := c.Get("If-None-Match")

	existing, err := h.store.GetEvent(ctx, tenantID, calendarID, event.UID)
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}

	if ifNoneMatch == "*" && existing != nil {
		return c.Status(412).SendString("Precondition Failed: event already exists")
	}

	if ifMatch != "" && (existing == nil || !etagMatches(ifMatch, existing.ETag)) {
		return c.Status(412).SendString("Precondition Failed: ETag mismatch")
	}

	// Store the whole resource atomically: components are inserted/updated in place (ids are
	// kept) and components the client dropped, such as a removed override, are deleted.
	statusCode := 201
	if existing != nil {
		statusCode = 204
	}
	err = h.store.ReplaceResource(ctx, tenantID, calendarID, event.UID, components)

	if err != nil {
		log.Printf("caldav: PUT error: %v", err)
		return c.Status(500).SendString("Internal Server Error")
	}

	// Scheduling: invite/update/cancel attendees (organizer edit) or reply to the organizer (attendee edit)
	if h.scheduling != nil {
		go func() {
			_ = h.scheduling.ScheduleEventReminders(context.Background(), event)
			if err := h.scheduling.ProcessUpdate(context.Background(), event, existing); err != nil {
				log.Printf("caldav: scheduling ProcessUpdate error: %v", err)
			}
		}()
	}

	c.Set("ETag", event.ETag)
	return c.SendStatus(statusCode)
}

// ============================================================
// DELETE — Delete Events (RFC 4918 §9.6)
// ============================================================

func (h *Handler) Delete(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	path := c.Path()

	if isCalendarCollection(path) {
		calendarID := extractCalendarID(path)
		cal, err := h.store.GetCalendar(ctx, tenantID, calendarID)
		if err != nil {
			return c.Status(500).SendString("Internal Server Error")
		}
		if cal == nil {
			return c.Status(404).SendString("Not Found")
		}
		if err := h.store.DeleteCalendar(ctx, tenantID, calendarID); err != nil {
			return c.Status(500).SendString("Internal Server Error")
		}
		return c.SendStatus(204)
	}

	if isEventResource(path) {
		calendarID := extractCalendarID(path)
		uid := extractEventUID(path)

		cal, err := h.store.GetCalendar(ctx, tenantID, calendarID)
		if err != nil {
			return c.Status(500).SendString("Internal Server Error")
		}
		if cal != nil && cal.IsReadOnly {
			return c.Status(403).SendString("Calendar is read-only")
		}

		// Check WebDAV lock (RFC 4918 §9.10)
		if existingLock, err := h.store.GetLock(ctx, tenantID, lockPath(path)); err == nil && existingLock != nil {
			ifHdr := c.Get("If")
			lockHdr := c.Get("Lock-Token")
			if !strings.Contains(ifHdr, existingLock.LockToken) && !strings.Contains(lockHdr, existingLock.LockToken) {
				return c.Status(423).SendString("Locked")
			}
		}

		// Fetch event before deletion (needed for cancellation emails)
		event, err := h.store.GetEvent(ctx, tenantID, calendarID, uid)
		if err != nil || event == nil {
			return c.Status(404).SendString("Not Found")
		}

		// Check If-Match
		ifMatch := c.Get("If-Match")
		if ifMatch != "" && !etagMatches(ifMatch, event.ETag) {
			return c.Status(412).SendString("Precondition Failed")
		}

		if err := h.store.DeleteEvent(ctx, tenantID, calendarID, uid); err != nil {
			return c.Status(500).SendString("Internal Server Error")
		}

		// Send cancellation to attendees
		if h.scheduling != nil {
			go func() {
				if err := h.scheduling.SendCancellation(context.Background(), event); err != nil {
					log.Printf("caldav: scheduling SendCancellation error: %v", err)
				}
			}()
		}

		return c.SendStatus(204)
	}

	return c.Status(403).SendString("Forbidden")
}

// ============================================================
// GET — Retrieve Event ICS / Calendar
// ============================================================

func (h *Handler) Get(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	path := c.Path()

	if isEventResource(path) {
		calendarID := extractCalendarID(path)
		uid := extractEventUID(path)

		evt, err := h.store.GetEvent(ctx, tenantID, calendarID, uid)
		if err != nil || evt == nil {
			return c.Status(404).SendString("Not Found")
		}

		c.Set("Content-Type", "text/calendar; charset=utf-8")
		c.Set("ETag", evt.ETag)
		return c.SendString(ics.GenerateICS(evt))
	}

	return c.Status(404).SendString("Not Found")
}

// ============================================================
// MKCALENDAR — Create Calendar (RFC 4791 §5.3.1)
// ============================================================

func (h *Handler) Mkcalendar(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)
	path := c.Path()

	// Parse MKCALENDAR body
	var body io.Reader = bytes.NewReader(c.Body())
	req, err := xmlutil.ParseMkcalendar(body)
	if err != nil {
		return c.Status(400).SendString("Bad Request: " + err.Error())
	}

	calendarID := extractCalendarID(path)
	if calendarID == "" {
		calendarID = uuid.New().String()
	} else if existing, err := h.store.GetCalendar(ctx, tenantID, calendarID); err != nil {
		return c.Status(500).SendString("Internal Server Error")
	} else if existing != nil {
		return c.Status(405).SendString("Method Not Allowed: calendar already exists") // RFC 4791 §5.3.1
	}

	cal := &models.Calendar{
		ID:                  calendarID,
		TenantID:            tenantID,
		UserID:              userID,
		Name:                "New Calendar",
		Color:               "#0078D4",
		Timezone:            "UTC",
		CTag:                ics.GenerateETag(uuid.New().String()),
		SyncToken:           "http://cal.example.com/sync/" + uuid.New().String(),
		IsDefault:           false,
		IsVisible:           true,
		IsReadOnly:          false,
		SupportedComponents: []string{"VEVENT", "VTODO"},
		MaxResourceSize:     10485760,
		Properties:          json.RawMessage("{}"),
	}

	// Apply values from request
	if req.Set != nil {
		if req.Set.Prop.DisplayName != "" {
			cal.Name = req.Set.Prop.DisplayName
		}
		if req.Set.Prop.CalendarDescription != "" {
			cal.Description = req.Set.Prop.CalendarDescription
		}
		if name, ok := ics.CalendarTimezoneName(req.Set.Prop.CalendarTimezone); ok {
			cal.Timezone = name // the zone name, not the whole VCALENDAR/VTIMEZONE text
		}
		if req.Set.Prop.CalendarColor != "" {
			cal.Color = req.Set.Prop.CalendarColor
		}
	}

	if err := h.store.CreateCalendar(ctx, cal); err != nil {
		log.Printf("caldav: MKCALENDAR error: %v", err)
		return c.Status(500).SendString("Internal Server Error")
	}

	return c.SendStatus(201)
}

// ============================================================
// MOVE — Move Events (RFC 4918 §9.9)
// ============================================================

func (h *Handler) Move(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	path := c.Path()

	destination := c.Get("Destination")
	if destination == "" {
		return c.Status(400).SendString("Bad Request: Destination header required")
	}

	if isEventResource(path) {
		calendarID := extractCalendarID(path)
		uid := extractEventUID(path)
		destCalendarID := extractCalendarID(destination)
		if destCalendarID == "" {
			return c.Status(400).SendString("Bad Request: Invalid destination")
		}

		destCal, err := h.store.GetCalendar(ctx, tenantID, destCalendarID)
		if err != nil || destCal == nil {
			return c.Status(404).SendString("Destination calendar not found")
		}

		srcCal, err := h.store.GetCalendar(ctx, tenantID, calendarID)
		if err != nil || srcCal == nil {
			return c.Status(404).SendString("Not Found")
		}
		if srcCal.IsReadOnly || destCal.IsReadOnly {
			return c.Status(403).SendString("Calendar is read-only")
		}
		status, done := h.prepareTransfer(c, tenantID, calendarID, destCalendarID, uid)
		if done {
			return nil // the error response is already written
		}

		if err := h.store.MoveEvent(ctx, tenantID, calendarID, uid, destCalendarID); err != nil {
			return c.Status(500).SendString("Internal Server Error")
		}
		return c.SendStatus(status)
	}

	return c.Status(403).SendString("Forbidden")
}

// prepareTransfer applies the RFC 4918 rules shared by COPY and MOVE (§9.8.5, §9.9.4):
//   - source and destination being the same resource is 403;
//   - a missing source is 404;
//   - an existing destination is replaced (204) unless the client sent "Overwrite: F" (412).
//
// It returns the success status to use. When done is true the error response has already been
// written to c and the caller must return immediately without doing the transfer.
func (h *Handler) prepareTransfer(c *fiber.Ctx, tenantID int, srcCal, destCal, uid string) (status int, done bool) {
	ctx := c.UserContext()
	if srcCal == destCal {
		_ = c.Status(403).SendString("Source and destination are the same resource")
		return 0, true
	}
	// The source must exist before anything at the destination is touched.
	source, err := h.store.GetEvent(ctx, tenantID, srcCal, uid)
	if err != nil {
		_ = c.Status(500).SendString("Internal Server Error")
		return 0, true
	}
	if source == nil {
		_ = c.Status(404).SendString("Not Found")
		return 0, true
	}
	existing, err := h.store.GetEvent(ctx, tenantID, destCal, uid)
	if err != nil {
		_ = c.Status(500).SendString("Internal Server Error")
		return 0, true
	}
	if existing == nil {
		return 201, false
	}
	if strings.EqualFold(strings.TrimSpace(c.Get("Overwrite")), "F") {
		_ = c.Status(412).SendString("Precondition Failed: destination exists and Overwrite is F")
		return 0, true
	}
	if err := h.store.DeleteEvent(ctx, tenantID, destCal, uid); err != nil {
		_ = c.Status(500).SendString("Internal Server Error")
		return 0, true
	}
	return 204, false
}

// ============================================================
// COPY — Copy Events (RFC 4918 §9.8)
// ============================================================

func (h *Handler) Copy(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	path := c.Path()

	destination := c.Get("Destination")
	if destination == "" {
		return c.Status(400).SendString("Bad Request: Destination header required")
	}

	if isEventResource(path) {
		calendarID := extractCalendarID(path)
		uid := extractEventUID(path)
		destCalendarID := extractCalendarID(destination)
		if destCalendarID == "" {
			return c.Status(400).SendString("Bad Request: Invalid destination")
		}

		destCal, err := h.store.GetCalendar(ctx, tenantID, destCalendarID)
		if err != nil || destCal == nil {
			return c.Status(404).SendString("Destination calendar not found")
		}

		if destCal.IsReadOnly {
			return c.Status(403).SendString("Calendar is read-only")
		}
		status, done := h.prepareTransfer(c, tenantID, calendarID, destCalendarID, uid)
		if done {
			return nil // the error response is already written
		}

		newID := uuid.New().String()
		newETag := ics.GenerateETag(newID)

		if err := h.store.CopyEvent(ctx, tenantID, calendarID, uid, destCalendarID, newID, newETag); err != nil {
			return c.Status(500).SendString("Internal Server Error")
		}
		return c.SendStatus(status)
	}

	return c.Status(403).SendString("Forbidden")
}

// ============================================================
// LOCK — Concurrency Control (RFC 4918 §9.10)
// ============================================================

func (h *Handler) Lock(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)
	path := lockPath(c.Path())
	depth := c.Get("Depth", "0")

	// Check existing lock
	existing, err := h.store.GetLock(ctx, tenantID, path)
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}
	if existing != nil {
		return c.Status(423).SendString("Locked")
	}

	lockToken := uuid.New().String()
	timeout := 3600 // default 1 hour

	lock := &models.Lock{
		ID:             uuid.New().String(),
		TenantID:       tenantID,
		ResourcePath:   path,
		LockToken:      lockToken,
		Owner:          "",
		LockType:       "write",
		LockScope:      "exclusive",
		Depth:          depth,
		TimeoutSeconds: timeout,
		ExpiresAt:      time.Now().Add(time.Duration(timeout) * time.Second),
	}

	if err := h.store.CreateLock(ctx, lock); err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}

	c.Set("Lock-Token", fmt.Sprintf("<urn:uuid:%s>", lockToken))
	c.Set("Content-Type", "application/xml; charset=utf-8")
	c.Status(200)

	var buf bytes.Buffer
	xmlutil.WriteLockResponse(&buf, lockToken, lock.Owner, lock.LockScope, lock.LockType, lock.Depth, lock.TimeoutSeconds)
	return c.Send(buf.Bytes())
}

// ============================================================
// UNLOCK — Release Lock (RFC 4918 §9.11)
// ============================================================

func (h *Handler) Unlock(c *fiber.Ctx) error {
	ctx := c.UserContext()
	tenantID := c.Locals("tenant_id").(int)

	lockToken := c.Get("Lock-Token")
	if lockToken == "" {
		return c.Status(400).SendString("Bad Request: Lock-Token header required")
	}

	// Clean up the token format
	lockToken = strings.TrimPrefix(lockToken, "<urn:uuid:")
	lockToken = strings.TrimPrefix(lockToken, "<")
	lockToken = strings.TrimSuffix(lockToken, ">")

	// RFC 4918 §9.11.1: a token that does not lock this resource is a 409.
	existing, err := h.store.GetLock(ctx, tenantID, lockPath(c.Path()))
	if err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}
	if existing == nil || existing.LockToken != lockToken {
		return c.Status(409).SendString("Conflict: lock token does not match a lock on this resource")
	}

	if err := h.store.DeleteLock(ctx, tenantID, lockToken); err != nil {
		return c.Status(500).SendString("Internal Server Error")
	}

	return c.SendStatus(204)
}

// ============================================================
// WellKnown handles /.well-known/caldav (RFC 6764 §6)
// ============================================================

// WellKnown is the public CalDAV discovery entry point. Clients (Apple Calendar, Thunderbird,
// DAVx5) probe it before they have credentials, so it must not require authentication, and
// it cannot know the user — it redirects to the DAV context path. The client then sends its
// PROPFIND there, gets the Basic challenge, and reads current-user-principal (see propfindRoot).
func (h *Handler) WellKnown(c *fiber.Ctx) error {
	c.Set("Location", "/dav/")
	return c.SendStatus(fiber.StatusMovedPermanently)
}

// ============================================================
// Property Building Helpers
// ============================================================

func (h *Handler) propfindRoot(tenantID, userID int, path string, req *xmlutil.PropfindRequest) []xmlutil.DAVResponse {
	props := make(map[string]string)
	props["resourcetype"] = `<D:resourcetype><D:collection/></D:resourcetype>`
	props["current-user-principal"] = fmt.Sprintf(`<D:current-user-principal><D:href>/dav/%d/%d/</D:href></D:current-user-principal>`, tenantID, userID)
	props["displayname"] = `<D:displayname>CalDAV Server</D:displayname>`

	return []xmlutil.DAVResponse{{
		Href: path,
		PropStats: []xmlutil.PropStat{{
			Props:  props,
			Status: 200,
		}},
	}}
}

func (h *Handler) propfindPrincipal(tenantID, userID int, email, path string, req *xmlutil.PropfindRequest) []xmlutil.DAVResponse {
	calHomePath := fmt.Sprintf("/dav/%d/%d/calendars/", tenantID, userID)
	principalPath := fmt.Sprintf("/dav/%d/%d/", tenantID, userID)

	props := make(map[string]string)
	props["resourcetype"] = `<D:resourcetype><D:collection/><D:principal/></D:resourcetype>`
	props["current-user-principal"] = fmt.Sprintf(`<D:current-user-principal><D:href>%s</D:href></D:current-user-principal>`, principalPath)
	props["calendar-home-set"] = fmt.Sprintf(`<C:calendar-home-set><D:href>%s</D:href></C:calendar-home-set>`, calHomePath)
	// RFC 6638: where to find the scheduling collections and which addresses identify this user
	props["schedule-inbox-URL"] = fmt.Sprintf(`<C:schedule-inbox-URL><D:href>%sinbox/</D:href></C:schedule-inbox-URL>`, principalPath)
	props["schedule-outbox-URL"] = fmt.Sprintf(`<C:schedule-outbox-URL><D:href>%soutbox/</D:href></C:schedule-outbox-URL>`, principalPath)
	if email != "" {
		props["calendar-user-address-set"] = fmt.Sprintf(`<C:calendar-user-address-set><D:href>mailto:%s</D:href></C:calendar-user-address-set>`, xmlEscapeStr(email))
	}
	props["displayname"] = fmt.Sprintf(`<D:displayname>%d</D:displayname>`, userID)
	props["principal-URL"] = fmt.Sprintf(`<D:principal-URL><D:href>%s</D:href></D:principal-URL>`, principalPath)

	return []xmlutil.DAVResponse{{
		Href: path,
		PropStats: []xmlutil.PropStat{{
			Props:  props,
			Status: 200,
		}},
	}}
}

func (h *Handler) propfindCalendarHome(tenantID, userID int, path string, calendars []*models.Calendar, req *xmlutil.PropfindRequest, depth string) []xmlutil.DAVResponse {
	var responses []xmlutil.DAVResponse

	// Calendar home itself
	props := make(map[string]string)
	props["resourcetype"] = `<D:resourcetype><D:collection/></D:resourcetype>`
	props["displayname"] = `<D:displayname>calendars</D:displayname>`
	props["current-user-principal"] = fmt.Sprintf(`<D:current-user-principal><D:href>/dav/%d/%d/</D:href></D:current-user-principal>`, tenantID, userID)

	responses = append(responses, xmlutil.DAVResponse{
		Href: path,
		PropStats: []xmlutil.PropStat{{
			Props:  props,
			Status: 200,
		}},
	})

	// List calendars at depth=1
	if depth == "1" || depth == "infinity" {
		for _, cal := range calendars {
			calPath := fmt.Sprintf("%s%s/", path, url.PathEscape(cal.ID))
			responses = append(responses, h.buildCalendarResponse(calPath, cal))
		}
	}

	return responses
}

func (h *Handler) propfindCalendar(tenantID, userID int, path string, cal *models.Calendar, req *xmlutil.PropfindRequest, depth string) []xmlutil.DAVResponse {
	var responses []xmlutil.DAVResponse
	responses = append(responses, h.buildCalendarResponse(path, cal))
	return responses
}

func (h *Handler) buildCalendarResponse(path string, cal *models.Calendar) xmlutil.DAVResponse {
	props := make(map[string]string)
	props["resourcetype"] = `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`
	props["displayname"] = fmt.Sprintf(`<D:displayname>%s</D:displayname>`, xmlEscapeStr(cal.Name))
	props["calendar-description"] = fmt.Sprintf(`<C:calendar-description>%s</C:calendar-description>`, xmlEscapeStr(cal.Description))
	props["calendar-color"] = fmt.Sprintf(`<A:calendar-color>%s</A:calendar-color>`, xmlEscapeStr(cal.Color))
	props["getctag"] = fmt.Sprintf(`<CS:getctag>%s</CS:getctag>`, cal.CTag)
	props["sync-token"] = fmt.Sprintf(`<D:sync-token>%s</D:sync-token>`, cal.SyncToken)
	props["calendar-timezone"] = fmt.Sprintf(`<C:calendar-timezone>%s</C:calendar-timezone>`, xmlEscapeStr(cal.Timezone))

	// Supported calendar component set
	var compSetXML string
	for _, comp := range cal.SupportedComponents {
		compSetXML += fmt.Sprintf(`<C:comp name="%s"/>`, comp)
	}
	props["supported-calendar-component-set"] = fmt.Sprintf(`<C:supported-calendar-component-set>%s</C:supported-calendar-component-set>`, compSetXML)
	props["max-resource-size"] = fmt.Sprintf(`<C:max-resource-size>%d</C:max-resource-size>`, cal.MaxResourceSize)

	// Supported report set
	props["supported-report-set"] = `<D:supported-report-set>` +
		`<D:supported-report><D:report><C:calendar-query/></D:report></D:supported-report>` +
		`<D:supported-report><D:report><C:calendar-multiget/></D:report></D:supported-report>` +
		`<D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report>` +
		`<D:supported-report><D:report><C:free-busy-query/></D:report></D:supported-report>` +
		`</D:supported-report-set>`

	// Current user privilege set
	if !cal.IsReadOnly {
		props["current-user-privilege-set"] = `<D:current-user-privilege-set>` +
			`<D:privilege><D:read/></D:privilege>` +
			`<D:privilege><D:write/></D:privilege>` +
			`<D:privilege><D:write-content/></D:privilege>` +
			`<D:privilege><D:bind/></D:privilege>` +
			`<D:privilege><D:unbind/></D:privilege>` +
			`</D:current-user-privilege-set>`
	} else {
		props["current-user-privilege-set"] = `<D:current-user-privilege-set>` +
			`<D:privilege><D:read/></D:privilege>` +
			`</D:current-user-privilege-set>`
	}

	return xmlutil.DAVResponse{
		Href: path,
		PropStats: []xmlutil.PropStat{{
			Props:  props,
			Status: 200,
		}},
	}
}

// buildEventResponse builds an event's properties. calendar-data is only rendered when names
// (the requested properties; nil for allprop) asks for it.
func (h *Handler) buildEventResponse(path string, evt *models.Event, names []xml.Name) xmlutil.DAVResponse {
	props := make(map[string]string)
	props["resourcetype"] = `<D:resourcetype/>`
	props["getcontenttype"] = `<D:getcontenttype>text/calendar; charset=utf-8; component=vevent</D:getcontenttype>`
	props["getetag"] = fmt.Sprintf(`<D:getetag>%s</D:getetag>`, evt.ETag)
	props["getcontentlength"] = fmt.Sprintf(`<D:getcontentlength>%d</D:getcontentlength>`, evt.SizeBytes)
	props["getlastmodified"] = fmt.Sprintf(`<D:getlastmodified>%s</D:getlastmodified>`, evt.UpdatedAt.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	props["displayname"] = fmt.Sprintf(`<D:displayname>%s</D:displayname>`, xmlEscapeStr(evt.Summary))
	if xmlutil.HasProp(names, xmlutil.NSCalDAVUrl, "calendar-data") {
		props["calendar-data"] = fmt.Sprintf(`<C:calendar-data>%s</C:calendar-data>`, xmlEscapeStr(ics.GenerateICS(evt)))
	}

	return xmlutil.DAVResponse{
		Href: path,
		PropStats: []xmlutil.PropStat{{
			Props:  props,
			Status: 200,
		}},
	}
}

// reportEventResponse builds a REPORT response holding only the properties in prop (all but
// calendar-data when prop is nil).
func (h *Handler) reportEventResponse(path string, evt *models.Event, prop *xmlutil.Prop) xmlutil.DAVResponse {
	var names []xml.Name
	if prop != nil {
		names = xmlutil.RequestedProps(prop)
		if names == nil {
			names = []xml.Name{} // an empty <D:prop/> asks for nothing
		}
	}
	return selectProps(h.buildEventResponse(path, evt, names), names)
}

// selectProps keeps only the requested properties of a single-propstat response and reports the
// requested ones it does not have as 404. names == nil (allprop) keeps the response as built.
func selectProps(resp xmlutil.DAVResponse, names []xml.Name) xmlutil.DAVResponse {
	if names == nil || len(resp.PropStats) != 1 {
		return resp
	}
	have := resp.PropStats[0].Props
	found := make(map[string]string)
	missing := make(map[string]string)
	for _, n := range names {
		if v, ok := have[n.Local]; ok {
			found[n.Local] = v
		} else {
			missing[n.Space+" "+n.Local] = emptyPropXML(n)
		}
	}
	resp.PropStats = nil
	if len(found) > 0 {
		resp.PropStats = append(resp.PropStats, xmlutil.PropStat{Props: found, Status: 200})
	}
	if len(missing) > 0 {
		resp.PropStats = append(resp.PropStats, xmlutil.PropStat{Props: missing, Status: 404})
	}
	if len(resp.PropStats) == 0 {
		resp.StatusCode = 200
	}
	return resp
}

// emptyPropXML renders an empty property element in its own namespace.
func emptyPropXML(n xml.Name) string {
	return fmt.Sprintf(`<%s xmlns="%s"/>`, n.Local, xmlEscapeStr(n.Space))
}

// ============================================================
// Filter Helpers
// ============================================================

func (h *Handler) applyCompFilter(filter *models.EventFilter, cf *xmlutil.CompFilter) error {
	if cf.Name == "VEVENT" || cf.Name == "VTODO" || cf.Name == "VJOURNAL" {
		filter.ComponentType = cf.Name
	}

	if cf.TimeRange != nil {
		if cf.TimeRange.Start != "" {
			t, err := time.Parse("20060102T150405Z", cf.TimeRange.Start)
			if err != nil {
				return fmt.Errorf("invalid time-range start %q", cf.TimeRange.Start)
			}
			filter.TimeRangeStart = &t
		}
		if cf.TimeRange.End != "" {
			t, err := time.Parse("20060102T150405Z", cf.TimeRange.End)
			if err != nil {
				return fmt.Errorf("invalid time-range end %q", cf.TimeRange.End)
			}
			filter.TimeRangeEnd = &t
		}
	}

	// Recurse into nested comp-filter
	if cf.CompFilter != nil {
		return h.applyCompFilter(filter, cf.CompFilter)
	}
	return nil
}

// ============================================================
// Path Parsing Utilities
// ============================================================

func isWellKnown(path string) bool {
	return strings.HasPrefix(path, "/.well-known/caldav")
}

func isPrincipal(path string) bool {
	parts := splitPath(path)
	// /dav/{tenant}/{user}/
	return len(parts) == 3 && parts[0] == "dav"
}

func isCalendarHome(path string) bool {
	parts := splitPath(path)
	// /dav/{tenant}/{user}/calendars/
	return len(parts) == 4 && parts[0] == "dav" && parts[3] == "calendars"
}

func isCalendarCollection(path string) bool {
	parts := splitPath(path)
	// /dav/{tenant}/{user}/calendars/{calendarID}/
	return len(parts) == 5 && parts[0] == "dav" && parts[3] == "calendars"
}

func isEventResource(path string) bool {
	return strings.HasSuffix(path, ".ics")
}

func extractCalendarID(path string) string {
	parts := splitPath(path)
	if len(parts) >= 5 && parts[3] == "calendars" {
		return parts[4]
	}
	return ""
}

// extractEventUID returns the UID named by the last "<uid>.ics" segment, percent-decoded once.
func extractEventUID(path string) string {
	if u, err := url.Parse(path); err == nil && u.Path != "" {
		path = u.EscapedPath() // still encoded: the segment is decoded below, exactly once
	}
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if strings.HasSuffix(p, ".ics") {
			raw := strings.TrimSuffix(p, ".ics")
			if unescaped, err := url.PathUnescape(raw); err == nil {
				return unescaped
			}
			return raw
		}
	}
	return ""
}

func splitPath(path string) []string {
	if u, err := url.Parse(path); err == nil && u.Path != "" {
		path = u.Path
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	// Remove .ics suffix from last element if present
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		parts[len(parts)-1] = strings.TrimSuffix(last, ".ics")
	}
	return parts
}

func ensureTrailingSlash(path string) string {
	if !strings.HasSuffix(path, "/") {
		return path + "/"
	}
	return path
}

// lockPath is the key locks are stored under: the request path percent-decoded once, so the
// same resource gets the same key however the client encoded it.
func lockPath(path string) string {
	if p, err := url.PathUnescape(path); err == nil {
		return p
	}
	return path
}

// etagMatches reports whether an If-Match header ("*" or a comma-separated list of entity
// tags) matches etag.
func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		if t = strings.TrimSpace(t); t == "*" || t == etag {
			return true
		}
	}
	return false
}

// xmlEscapeStr escapes s for XML text or attribute content, dropping characters XML 1.0 forbids.
func xmlEscapeStr(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == 0x9 || r == 0xA || r == 0xD || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || r >= 0x10000 {
			return r
		}
		return -1
	}, s)
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
