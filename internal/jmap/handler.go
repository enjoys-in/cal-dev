package jmap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/availability"
	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/realtime"
	"github.com/enjoys-in/cal-dev/internal/scheduling"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// Handler provides JMAP (RFC 8620) endpoint handlers.
type Handler struct {
	store      *store.Store
	baseURL    string
	scheduling *scheduling.Service
	hub        *realtime.Hub
	avail      *availability.Service
}

// SetRealtime enables live streaming on /jmap/eventsource.
func (h *Handler) SetRealtime(hub *realtime.Hub) { h.hub = hub }

// SetAvailability enables the JMAP Principal/getAvailability method.
func (h *Handler) SetAvailability(a *availability.Service) { h.avail = a }

// NewHandler creates a new JMAP handler.
func NewHandler(s *store.Store, baseURL string, sched *scheduling.Service) *Handler {
	return &Handler{store: s, baseURL: baseURL, scheduling: sched}
}

// ============================================================
// JMAP Session Resource (RFC 8620 §2)
// ============================================================

func (h *Handler) Session(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)
	email, _ := c.Locals("email").(string)

	accountID := fmt.Sprintf("%d:%d", tenantID, userID)

	session := models.JMAPSession{
		Capabilities: map[string]interface{}{
			"urn:ietf:params:jmap:core": map[string]interface{}{
				"maxSizeUpload":         50000000,
				"maxConcurrentUpload":   8,
				"maxSizeRequest":        10000000,
				"maxConcurrentRequests": 8,
				"maxCallsInRequest":     16,
				"maxObjectsInGet":       500,
				"maxObjectsInSet":       500,
				"collationAlgorithms":   []string{"i;ascii-casemap", "i;ascii-numeric", "i;unicode-casemap"},
			},
			"urn:ietf:params:jmap:calendars": map[string]interface{}{
				"supportsRecurrences":  true,
				"supportsParticipants": true,
				"supportsAvailability": h.avail != nil,
				"supportsScheduling":   true,
				"maxCalendarsPerEvent": 1,
				"maxAttendeesPerEvent": 1000,
			},
		},
		Accounts: map[string]interface{}{
			accountID: map[string]interface{}{
				"name":       email,
				"isPersonal": true,
				"isReadOnly": false,
				"accountCapabilities": map[string]interface{}{
					"urn:ietf:params:jmap:calendars": map[string]interface{}{},
				},
			},
		},
		PrimaryAccounts: map[string]string{
			"urn:ietf:params:jmap:calendars": accountID,
		},
		Username:       email,
		APIURL:         h.baseURL + "/jmap/api",
		DownloadURL:    h.baseURL + "/jmap/download/{accountId}/{blobId}/{name}",
		UploadURL:      h.baseURL + "/jmap/upload/{accountId}/",
		EventSourceURL: h.baseURL + "/jmap/eventsource",
		State:          uuid.New().String()[:8],
	}

	c.Set("Content-Type", "application/json")
	return c.JSON(session)
}

// ============================================================
// JMAP API Endpoint (RFC 8620 §3)
// ============================================================

// JMAPMethodCall represents a parsed method call.
type JMAPMethodCall struct {
	MethodName string
	Args       json.RawMessage
	CallID     string
}

func (h *Handler) API(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)

	var req models.JMAPRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(jmapError("urn:ietf:params:jmap:error:notRequest", "Invalid request body"))
	}

	// Validate capabilities
	var responses []interface{}

	for _, rawCall := range req.MethodCalls {
		call, err := parseMethodCall(rawCall)
		if err != nil {
			responses = append(responses, []interface{}{"error", map[string]string{
				"type":        "unknownMethod",
				"description": err.Error(),
			}, ""})
			continue
		}

		result := h.dispatchMethod(c, tenantID, userID, call)
		responses = append(responses, result)
	}

	state, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "CalendarEvent")
	if state == "" {
		state = uuid.New().String()[:8]
	}

	resp := models.JMAPResponse{
		MethodResponses: responses,
		SessionState:    state,
	}

	c.Set("Content-Type", "application/json")
	return c.JSON(resp)
}

func (h *Handler) dispatchMethod(c *fiber.Ctx, tenantID, userID int, call *JMAPMethodCall) interface{} {
	switch call.MethodName {
	case "CalendarEvent/get":
		return h.calendarEventGet(c, tenantID, userID, call)
	case "CalendarEvent/set":
		return h.calendarEventSet(c, tenantID, userID, call)
	case "CalendarEvent/query":
		return h.calendarEventQuery(c, tenantID, userID, call)
	case "CalendarEvent/changes":
		return h.calendarEventChanges(c, tenantID, userID, call)
	case "Calendar/get":
		return h.calendarGet(c, tenantID, userID, call)
	case "Calendar/set":
		return h.calendarSet(c, tenantID, userID, call)
	case "Principal/getAvailability":
		return h.principalGetAvailability(c, userID, call)
	default:
		return []interface{}{"error", map[string]string{
			"type":        "unknownMethod",
			"description": fmt.Sprintf("Unknown method: %s", call.MethodName),
		}, call.CallID}
	}
}

// ============================================================
// CalendarEvent/get (RFC 8620 §5.1)
// ============================================================

type calendarEventGetArgs struct {
	AccountID  string   `json:"accountId"`
	IDs        []string `json:"ids"`
	Properties []string `json:"properties"`
}

func (h *Handler) calendarEventGet(c *fiber.Ctx, tenantID, userID int, call *JMAPMethodCall) interface{} {
	var args calendarEventGetArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return jmapMethodError("invalidArguments", "Invalid arguments", call.CallID)
	}

	var folded []foldedEvent
	var notFound []string

	if len(args.IDs) == 0 {
		// Return all events for user: one object per series, overrides folded into it
		events, err := h.store.QueryResources(c.UserContext(), &models.EventFilter{
			TenantID: tenantID,
			UserID:   userID,
		})
		if err != nil {
			return jmapMethodError("serverFail", err.Error(), call.CallID)
		}
		folded = foldResources(events)
	} else {
		for _, id := range args.IDs {
			evt, err := h.store.GetEventByID(c.UserContext(), tenantID, id)
			if err != nil {
				return jmapMethodError("serverFail", err.Error(), call.CallID)
			}
			if evt == nil {
				notFound = append(notFound, id)
				continue
			}
			fe := foldedEvent{event: evt}
			if evt.RecurrenceID == "" {
				rows, err := h.store.GetResource(c.UserContext(), tenantID, evt.CalendarID, evt.UID)
				if err != nil {
					return jmapMethodError("serverFail", err.Error(), call.CallID)
				}
				for _, r := range rows {
					if r.RecurrenceID != "" {
						fe.overrides = append(fe.overrides, r)
					}
				}
			}
			folded = append(folded, fe)
		}
	}

	state, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "CalendarEvent")

	list := make([]map[string]interface{}, 0, len(folded))
	for _, fe := range folded {
		list = append(list, eventToJMAP(fe.event, fe.overrides))
	}

	result := map[string]interface{}{
		"accountId": fmt.Sprintf("%d:%d", tenantID, userID),
		"state":     state,
		"list":      list,
		"notFound":  notFound,
	}

	return []interface{}{"CalendarEvent/get", result, call.CallID}
}

// ============================================================
// CalendarEvent/set (RFC 8620 §5.3)
// ============================================================

type calendarEventSetArgs struct {
	AccountID string                            `json:"accountId"`
	IfInState string                            `json:"ifInState,omitempty"`
	Create    map[string]map[string]interface{} `json:"create,omitempty"`
	Update    map[string]map[string]interface{} `json:"update,omitempty"`
	Destroy   []string                          `json:"destroy,omitempty"`
}

func (h *Handler) calendarEventSet(c *fiber.Ctx, tenantID, userID int, call *JMAPMethodCall) interface{} {
	var args calendarEventSetArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return jmapMethodError("invalidArguments", "Invalid arguments", call.CallID)
	}

	if args.IfInState != "" {
		currentState, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "CalendarEvent")
		if args.IfInState != currentState {
			return []interface{}{"error", map[string]string{
				"type":        "stateMismatch",
				"description": "State mismatch",
			}, call.CallID}
		}
	}

	created := make(map[string]map[string]interface{})
	updated := make(map[string]interface{})
	destroyed := make([]string, 0)
	notCreated := make(map[string]interface{})
	notUpdated := make(map[string]interface{})
	notDestroyed := make(map[string]interface{})

	// Handle creates
	for tempID, props := range args.Create {
		evt := jmapToEvent(props, tenantID, userID)
		// An event with attendees needs an organizer or scheduling is skipped entirely and nobody is
		// invited. The creator is the organizer; never trust an organizer sent by the client.
		evt.Organizer = ""
		if email, _ := c.Locals("email").(string); email != "" && hasAttendees(evt.Attendees) {
			evt.Organizer = email
		}
		if evt.CalendarID == "" {
			// Get default calendar
			calendars, err := h.store.GetCalendarsByUser(c.UserContext(), tenantID, userID)
			if err != nil || len(calendars) == 0 {
				notCreated[tempID] = map[string]string{"type": "serverFail", "description": "No calendar found"}
				continue
			}
			// Prefer the default calendar, else any writable one; never a read-only subscription
			// calendar (its events are replaced on every sync).
			picked := ""
			for _, cal := range calendars {
				if cal.IsReadOnly {
					continue
				}
				if cal.IsDefault {
					picked = cal.ID
					break
				}
				if picked == "" {
					picked = cal.ID
				}
			}
			if picked == "" {
				notCreated[tempID] = map[string]string{"type": "forbidden", "description": "No writable calendar"}
				continue
			}
			evt.CalendarID = picked
		} else {
			cal, err := h.store.GetCalendar(c.UserContext(), tenantID, evt.CalendarID)
			if err != nil || cal == nil || cal.UserID != userID || cal.IsReadOnly {
				notCreated[tempID] = map[string]string{"type": "forbidden", "description": "Calendar not found, read-only or access denied"}
				continue
			}
		}

		evt.RawICS = ics.GenerateICS(evt)
		evt.SizeBytes = len(evt.RawICS)
		evt.ETag = ics.GenerateETag(evt.RawICS)

		if err := h.store.CreateEvent(c.UserContext(), evt); err != nil {
			log.Printf("jmap: create event error: %v", err)
			notCreated[tempID] = map[string]string{"type": "serverFail", "description": err.Error()}
			continue
		}

		created[tempID] = map[string]interface{}{
			"id":   evt.ID,
			"uid":  evt.UID,
			"etag": evt.ETag,
		}

		// Send invitation emails to attendees
		if h.scheduling != nil {
			evtCopy := evt
			go func() {
				if err := h.scheduling.ProcessEvent(context.Background(), evtCopy, "REQUEST"); err != nil {
					log.Printf("jmap: scheduling ProcessEvent error: %v", err)
				}
			}()
		}
	}

	// Handle updates
	for id, props := range args.Update {
		evt, err := h.store.GetEventByID(c.UserContext(), tenantID, id)
		if err != nil || evt == nil {
			notUpdated[id] = map[string]string{"type": "notFound", "description": "Event not found"}
			continue
		}

		// Load the whole resource (master + overrides) so an edit can never drop an override.
		rows, err := h.store.GetResource(c.UserContext(), tenantID, evt.CalendarID, evt.UID)
		if err != nil || len(rows) == 0 {
			notUpdated[id] = map[string]string{"type": "serverFail", "description": "cannot load event"}
			continue
		}
		if evt.RecurrenceID != "" && rows[0].RecurrenceID == "" {
			notUpdated[id] = map[string]string{"type": "invalidProperties",
				"description": "this id is a single occurrence; update the series id and use recurrenceOverrides"}
			continue
		}
		if cal, _ := h.store.GetCalendar(c.UserContext(), tenantID, evt.CalendarID); cal != nil && cal.IsReadOnly {
			notUpdated[id] = map[string]string{"type": "forbidden", "description": "Calendar is read-only"}
			continue
		}
		// Edit the loaded master row itself (the one FinalizeResource/ReplaceResource will write).
		// An orphan override addressed by its own id is its own resource: rows[0] is then that override.
		evt = rows[0]

		// Moving to another calendar: only into one of the caller's own writable calendars.
		if newCal, ok := props["calendarId"].(string); ok && newCal != "" && newCal != evt.CalendarID {
			dest, err := h.store.GetCalendar(c.UserContext(), tenantID, newCal)
			if err != nil || dest == nil || dest.UserID != userID || dest.IsReadOnly {
				notUpdated[id] = map[string]string{"type": "forbidden", "description": "Calendar not found or access denied"}
				continue
			}
			if err := h.store.MoveEvent(c.UserContext(), tenantID, evt.CalendarID, evt.UID, newCal); err != nil {
				notUpdated[id] = map[string]string{"type": "serverFail", "description": err.Error()}
				continue
			}
			for _, r := range rows {
				r.CalendarID = newCal
			}
		}

		// Apply updates (keep a snapshot of the previous version for scheduling diffs;
		// applyJMAPUpdates reassigns fields rather than mutating shared slices).
		oldEvt := *evt
		applyJMAPUpdates(evt, props)
		if patches, ok := props["recurrenceOverrides"].(map[string]interface{}); ok {
			if rows, err = applyOverridePatches(rows, patches); err != nil {
				notUpdated[id] = map[string]string{"type": "invalidProperties", "description": err.Error()}
				continue
			}
		}
		rows = ics.FinalizeResource(rows) // regenerates every component; overrides, alarms and EXDATEs survive

		if err := h.store.ReplaceResource(c.UserContext(), tenantID, evt.CalendarID, evt.UID, rows); err != nil {
			notUpdated[id] = map[string]string{"type": "serverFail", "description": err.Error()}
			continue
		}
		updated[id] = nil

		// Send update notification to attendees
		if h.scheduling != nil {
			evtCopy := evt
			oldCopy := oldEvt
			go func() {
				if err := h.scheduling.ProcessUpdate(context.Background(), evtCopy, &oldCopy); err != nil {
					log.Printf("jmap: scheduling ProcessUpdate error: %v", err)
				}
			}()
		}
	}

	// Handle destroys
	for _, id := range args.Destroy {
		evt, err := h.store.GetEventByID(c.UserContext(), tenantID, id)
		if err != nil || evt == nil {
			notDestroyed[id] = map[string]string{"type": "notFound", "description": "Event not found"}
			continue
		}
		if cal, _ := h.store.GetCalendar(c.UserContext(), tenantID, evt.CalendarID); cal != nil && cal.IsReadOnly {
			notDestroyed[id] = map[string]string{"type": "forbidden", "description": "Calendar is read-only"}
			continue
		}
		if evt.RecurrenceID != "" {
			if master, _ := h.store.GetEvent(c.UserContext(), tenantID, evt.CalendarID, evt.UID); master != nil && master.RecurrenceID == "" {
				// destroying one occurrence must not delete the whole series
				notDestroyed[id] = map[string]string{"type": "invalidProperties",
					"description": "this id is a single occurrence; set recurrenceOverrides[<start>] = {\"excluded\": true} on the series instead"}
				continue
			}
		}
		if err := h.store.DeleteEvent(c.UserContext(), tenantID, evt.CalendarID, evt.UID); err != nil {
			notDestroyed[id] = map[string]string{"type": "serverFail", "description": err.Error()}
			continue
		}
		destroyed = append(destroyed, id)

		// Send cancellation to attendees
		if h.scheduling != nil {
			evtCopy := evt
			go func() {
				if err := h.scheduling.SendCancellation(context.Background(), evtCopy); err != nil {
					log.Printf("jmap: scheduling SendCancellation error: %v", err)
				}
			}()
		}
	}

	newState, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "CalendarEvent")

	result := map[string]interface{}{
		"accountId":    fmt.Sprintf("%d:%d", tenantID, userID),
		"oldState":     args.IfInState,
		"newState":     newState,
		"created":      created,
		"updated":      updated,
		"destroyed":    destroyed,
		"notCreated":   notCreated,
		"notUpdated":   notUpdated,
		"notDestroyed": notDestroyed,
	}

	return []interface{}{"CalendarEvent/set", result, call.CallID}
}

// ============================================================
// CalendarEvent/query (RFC 8620 §5.2)
// ============================================================

type calendarEventQueryArgs struct {
	AccountID string                 `json:"accountId"`
	Filter    map[string]interface{} `json:"filter,omitempty"`
	Sort      []map[string]string    `json:"sort,omitempty"`
	Position  int                    `json:"position,omitempty"`
	Limit     int                    `json:"limit,omitempty"`
}

func (h *Handler) calendarEventQuery(c *fiber.Ctx, tenantID, userID int, call *JMAPMethodCall) interface{} {
	var args calendarEventQueryArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return jmapMethodError("invalidArguments", "Invalid arguments", call.CallID)
	}

	filter := &models.EventFilter{
		TenantID: tenantID,
		UserID:   userID,
		Limit:    args.Limit,
		Offset:   args.Position,
	}

	if args.Limit == 0 {
		filter.Limit = 100 // Default limit
	}

	// Apply filters
	if args.Filter != nil {
		if calID, ok := args.Filter["calendarId"].(string); ok {
			filter.CalendarID = calID
		}
		if after, ok := args.Filter["after"].(string); ok {
			t, _ := time.Parse(time.RFC3339, after)
			filter.TimeRangeStart = &t
		}
		if before, ok := args.Filter["before"].(string); ok {
			t, _ := time.Parse(time.RFC3339, before)
			filter.TimeRangeEnd = &t
		}
	}

	events, err := h.store.QueryResources(c.UserContext(), filter)
	if err != nil {
		return jmapMethodError("serverFail", err.Error(), call.CallID)
	}

	// One id per series: an override that matched on its own resolves to its master's id.
	folded := foldResources(events)
	ids := make([]string, 0, len(folded))
	for _, fe := range folded {
		ids = append(ids, fe.event.ID)
	}

	state, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "CalendarEvent")

	result := map[string]interface{}{
		"accountId":           fmt.Sprintf("%d:%d", tenantID, userID),
		"queryState":          state,
		"canCalculateChanges": false,
		"position":            args.Position,
		"ids":                 ids,
		"total":               len(ids),
	}

	return []interface{}{"CalendarEvent/query", result, call.CallID}
}

// ============================================================
// CalendarEvent/changes (RFC 8620 §5.4)
// ============================================================

type calendarEventChangesArgs struct {
	AccountID  string `json:"accountId"`
	SinceState string `json:"sinceState"`
}

func (h *Handler) calendarEventChanges(c *fiber.Ctx, tenantID, userID int, call *JMAPMethodCall) interface{} {
	var args calendarEventChangesArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return jmapMethodError("invalidArguments", "Invalid arguments", call.CallID)
	}

	newState, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "CalendarEvent")
	if newState == "" {
		newState = "initial"
	}

	if args.SinceState == "" || args.SinceState == newState {
		result := map[string]interface{}{
			"accountId":      fmt.Sprintf("%d:%d", tenantID, userID),
			"oldState":       args.SinceState,
			"newState":       newState,
			"hasMoreChanges": false,
			"created":        []string{},
			"updated":        []string{},
			"destroyed":      []string{},
		}
		return []interface{}{"CalendarEvent/changes", result, call.CallID}
	}

	changes, err := h.store.GetEventChangesSinceState(c.UserContext(), tenantID, userID, args.SinceState)
	if err != nil {
		return []interface{}{"error", map[string]string{
			"type":        "cannotCalculateChanges",
			"description": "State too old or unknown: " + err.Error(),
		}, call.CallID}
	}

	result := map[string]interface{}{
		"accountId":      fmt.Sprintf("%d:%d", tenantID, userID),
		"oldState":       args.SinceState,
		"newState":       newState,
		"hasMoreChanges": false,
		"created":        changes.Created,
		"updated":        changes.Updated,
		"destroyed":      changes.Destroyed,
	}

	return []interface{}{"CalendarEvent/changes", result, call.CallID}
}

// ============================================================
// Calendar/get
// ============================================================

func (h *Handler) calendarGet(c *fiber.Ctx, tenantID, userID int, call *JMAPMethodCall) interface{} {
	if err := h.store.EnsureDefaultCalendar(c.UserContext(), tenantID, userID); err != nil {
		log.Printf("Warning: auto-provision default calendar failed: %v", err)
	}
	calendars, err := h.store.GetCalendarsByUser(c.UserContext(), tenantID, userID)
	if err != nil {
		return jmapMethodError("serverFail", err.Error(), call.CallID)
	}

	list := make([]map[string]interface{}, 0, len(calendars))
	for _, cal := range calendars {
		list = append(list, map[string]interface{}{
			"id":                  cal.ID,
			"name":                cal.Name,
			"description":         cal.Description,
			"color":               cal.Color,
			"sortOrder":           cal.SortOrder,
			"isDefault":           cal.IsDefault,
			"isVisible":           cal.IsVisible,
			"isReadOnly":          cal.IsReadOnly,
			"timezone":            cal.Timezone,
			"supportedComponents": cal.SupportedComponents,
			"myRights": map[string]bool{
				"mayReadItems": true,
				"mayWriteAll":  !cal.IsReadOnly,
				"mayDelete":    !cal.IsReadOnly,
				"mayRename":    !cal.IsReadOnly,
			},
		})
	}

	state, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "Calendar")

	result := map[string]interface{}{
		"accountId": fmt.Sprintf("%d:%d", tenantID, userID),
		"state":     state,
		"list":      list,
		"notFound":  []string{},
	}

	return []interface{}{"Calendar/get", result, call.CallID}
}

// ============================================================
// Calendar/set
// ============================================================

type calendarSetArgs struct {
	AccountID string                            `json:"accountId"`
	IfInState string                            `json:"ifInState,omitempty"`
	Create    map[string]map[string]interface{} `json:"create,omitempty"`
	Update    map[string]map[string]interface{} `json:"update,omitempty"`
	Destroy   []string                          `json:"destroy,omitempty"`
}

func (h *Handler) calendarSet(c *fiber.Ctx, tenantID, userID int, call *JMAPMethodCall) interface{} {
	var args calendarSetArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return jmapMethodError("invalidArguments", "Invalid arguments", call.CallID)
	}

	if args.IfInState != "" {
		currentState, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "Calendar")
		if args.IfInState != currentState {
			return []interface{}{"error", map[string]string{
				"type":        "stateMismatch",
				"description": "State mismatch",
			}, call.CallID}
		}
	}

	created := make(map[string]map[string]interface{})
	updated := make(map[string]interface{})
	destroyed := make([]string, 0)
	notCreated := make(map[string]interface{})
	notUpdated := make(map[string]interface{})

	// Handle creates
	for tempID, props := range args.Create {
		// Enforce: per email only one calendar
		existing, err := h.store.GetCalendarsByUser(c.UserContext(), tenantID, userID)
		if err == nil && len(existing) >= 1 {
			notCreated[tempID] = map[string]string{
				"type":        "forbidden",
				"description": "Only one calendar per email is allowed",
			}
			continue
		}

		cal := &models.Calendar{
			ID:                  uuid.New().String(),
			TenantID:            tenantID,
			UserID:              userID,
			Name:                "New Calendar",
			Color:               "#0078D4",
			Timezone:            "UTC",
			CTag:                ics.GenerateETag(uuid.New().String()),
			SyncToken:           "http://cal.example.com/sync/" + uuid.New().String(),
			SupportedComponents: []string{"VEVENT", "VTODO"},
			MaxResourceSize:     10485760,
			IsVisible:           true,
			Properties:          json.RawMessage("{}"),
		}

		if name, ok := props["name"].(string); ok {
			cal.Name = name
		}
		if desc, ok := props["description"].(string); ok {
			cal.Description = desc
		}
		if color, ok := props["color"].(string); ok {
			cal.Color = color
		}
		if tz, ok := props["timezone"].(string); ok {
			cal.Timezone = tz
		}

		if err := h.store.CreateCalendar(c.UserContext(), cal); err != nil {
			notCreated[tempID] = map[string]string{"type": "serverFail", "description": err.Error()}
			continue
		}

		created[tempID] = map[string]interface{}{
			"id": cal.ID,
		}
	}

	// Handle updates
	for id, props := range args.Update {
		cal, err := h.store.GetCalendar(c.UserContext(), tenantID, id)
		if err != nil || cal == nil {
			notUpdated[id] = map[string]string{"type": "notFound", "description": "Calendar not found"}
			continue
		}

		if name, ok := props["name"].(string); ok && name != "" {
			cal.Name = name
		}
		if desc, ok := props["description"].(string); ok {
			cal.Description = desc
		}
		if color, ok := props["color"].(string); ok && color != "" {
			cal.Color = color
		}
		if tz, ok := props["timezone"].(string); ok && tz != "" {
			cal.Timezone = tz
		}
		if isDefault, ok := props["isDefault"].(bool); ok {
			cal.IsDefault = isDefault
		}
		if isVisible, ok := props["isVisible"].(bool); ok {
			cal.IsVisible = isVisible
		}
		if sortOrder, ok := props["sortOrder"].(float64); ok {
			cal.SortOrder = int(sortOrder)
		}

		if err := h.store.UpdateCalendar(c.UserContext(), cal); err != nil {
			notUpdated[id] = map[string]string{"type": "serverFail", "description": err.Error()}
			continue
		}
		updated[id] = nil
	}

	// Handle destroys
	for _, id := range args.Destroy {
		if err := h.store.DeleteCalendar(c.UserContext(), tenantID, id); err == nil {
			destroyed = append(destroyed, id)
		}
	}

	newState, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "Calendar")

	result := map[string]interface{}{
		"accountId":  fmt.Sprintf("%d:%d", tenantID, userID),
		"oldState":   args.IfInState,
		"newState":   newState,
		"created":    created,
		"updated":    updated,
		"destroyed":  destroyed,
		"notCreated": notCreated,
		"notUpdated": notUpdated,
	}

	return []interface{}{"Calendar/set", result, call.CallID}
}

// ============================================================
// Principal/getAvailability (JMAP Calendars)
// ============================================================

type getAvailabilityArgs struct {
	AccountID string `json:"accountId"`
	ID        string `json:"id"` // principal: "tenant:user" account id, numeric user id, or email
	UTCStart  string `json:"utcStart"`
	UTCEnd    string `json:"utcEnd"`
}

const maxAvailabilityWindow = 62 * 24 * time.Hour

// principalGetAvailability returns the busy blocks of another user the caller may see.
// A principal that does not exist and one the caller may not see look identical (notFound)
// so the method cannot be used to discover accounts. Event details are never returned.
func (h *Handler) principalGetAvailability(c *fiber.Ctx, userID int, call *JMAPMethodCall) interface{} {
	if h.avail == nil {
		return jmapMethodError("unknownMethod", "Availability is not enabled", call.CallID)
	}
	var args getAvailabilityArgs
	if err := json.Unmarshal(call.Args, &args); err != nil || args.ID == "" {
		return jmapMethodError("invalidArguments", "id, utcStart and utcEnd are required", call.CallID)
	}
	from, err1 := time.Parse(time.RFC3339, args.UTCStart)
	to, err2 := time.Parse(time.RFC3339, args.UTCEnd)
	if err1 != nil || err2 != nil || !to.After(from) {
		return jmapMethodError("invalidArguments", "utcStart/utcEnd must be RFC 3339 with utcEnd after utcStart", call.CallID)
	}
	if to.Sub(from) > maxAvailabilityWindow {
		return jmapMethodError("invalidArguments", "window too large (max 62 days)", call.CallID)
	}

	ctx := c.UserContext()
	viewer, err := h.store.GetUserByID(ctx, userID)
	if err != nil || viewer == nil {
		return jmapMethodError("serverFail", "cannot resolve current user", call.CallID)
	}
	owner := h.resolvePrincipal(c, args.ID)
	if owner == nil {
		return jmapMethodError("notFound", "No such principal", call.CallID)
	}
	ok, err := h.avail.CanView(ctx, viewer, owner)
	if err != nil {
		return jmapMethodError("serverFail", err.Error(), call.CallID)
	}
	if !ok {
		return jmapMethodError("notFound", "No such principal", call.CallID)
	}
	busy, err := h.avail.Busy(ctx, owner, from, to)
	if err != nil {
		return jmapMethodError("serverFail", err.Error(), call.CallID)
	}

	list := make([]map[string]interface{}, 0, len(busy))
	for _, b := range busy {
		list = append(list, map[string]interface{}{
			"utcStart":   b.Start.UTC().Format(time.RFC3339),
			"utcEnd":     b.End.UTC().Format(time.RFC3339),
			"busyStatus": b.Status,
		})
	}
	return []interface{}{"Principal/getAvailability", map[string]interface{}{
		"accountId": args.AccountID,
		"list":      list,
	}, call.CallID}
}

// resolvePrincipal maps a JMAP principal id to a user: "tenant:user", numeric id, or email.
func (h *Handler) resolvePrincipal(c *fiber.Ctx, id string) *models.User {
	ctx := c.UserContext()
	id = strings.TrimSpace(id)
	if strings.Contains(id, "@") {
		u, _ := h.store.GetUserByEmail(ctx, id)
		return u
	}
	if i := strings.LastIndex(id, ":"); i >= 0 {
		id = id[i+1:]
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return nil
	}
	u, _ := h.store.GetUserByID(ctx, n)
	return u
}

// ============================================================
// Helpers
// ============================================================

func parseMethodCall(raw json.RawMessage) (*JMAPMethodCall, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("method call must be a JSON array")
	}
	if len(arr) != 3 {
		return nil, fmt.Errorf("method call must have 3 elements [name, args, callId]")
	}

	var name, callID string
	if err := json.Unmarshal(arr[0], &name); err != nil {
		return nil, fmt.Errorf("invalid method name")
	}
	if err := json.Unmarshal(arr[2], &callID); err != nil {
		return nil, fmt.Errorf("invalid call ID")
	}

	return &JMAPMethodCall{
		MethodName: name,
		Args:       arr[1],
		CallID:     callID,
	}, nil
}

func eventToJMAP(evt *models.Event, overrides []*models.Event) map[string]interface{} {
	result := map[string]interface{}{
		"id":             evt.ID,
		"calendarId":     evt.CalendarID,
		"uid":            evt.UID,
		"@type":          "CalendarEvent",
		"title":          evt.Summary,
		"description":    evt.Description,
		"location":       evt.Location,
		"start":          evt.DTStart.Format(time.RFC3339),
		"status":         evt.Status,
		"isAllDay":       evt.AllDay,
		"sequence":       evt.Sequence,
		"priority":       evt.Priority,
		"classification": evt.Classification,
		"created":        evt.CreatedAt.Format(time.RFC3339),
		"updated":        evt.UpdatedAt.Format(time.RFC3339),
	}
	if evt.RecurrenceID != "" { // an override that has no master in this account
		if key, _, ok := overrideIDToKey(evt.RecurrenceID); ok {
			result["recurrenceId"] = key
		}
	}
	if ov := recurrenceOverridesJMAP(evt, overrides); ov != nil {
		result["recurrenceOverrides"] = ov
	}

	if evt.DTEnd != nil {
		result["end"] = evt.DTEnd.Format(time.RFC3339)
	}
	if evt.Duration != "" {
		result["duration"] = evt.Duration
	}
	if evt.RecurrenceRule != "" {
		result["recurrenceRules"] = []map[string]string{
			{"rule": evt.RecurrenceRule},
		}
	}
	if evt.Organizer != "" {
		result["organizer"] = evt.Organizer
	}
	if evt.Location != "" {
		result["locations"] = map[string]interface{}{
			"1": map[string]string{
				"name": evt.Location,
			},
		}
	}

	// Parse attendees
	var attendees []models.Attendee
	if err := json.Unmarshal(evt.Attendees, &attendees); err == nil && len(attendees) > 0 {
		participants := make(map[string]interface{})
		for i, a := range attendees {
			participants[fmt.Sprintf("%d", i+1)] = map[string]interface{}{
				"name":                a.DisplayName,
				"email":               a.Email,
				"kind":                "individual",
				"roles":               map[string]bool{"attendee": true},
				"participationStatus": a.Status,
				"expectReply":         a.RSVP,
			}
		}
		result["participants"] = participants
	}

	return result
}

func jmapToEvent(props map[string]interface{}, tenantID, userID int) *models.Event {
	evt := &models.Event{
		ID:             uuid.New().String(),
		TenantID:       tenantID,
		UserID:         userID,
		UID:            uuid.New().String(),
		Status:         "CONFIRMED",
		Transparency:   "OPAQUE",
		Classification: "PUBLIC",
		ComponentType:  "VEVENT",
		DTStamp:        time.Now().UTC(),
		DTStart:        time.Now().UTC(),
		Attendees:      json.RawMessage("[]"),
		Properties:     json.RawMessage("{}"),
		Categories:     []string{},
	}

	if title, ok := props["title"].(string); ok {
		evt.Summary = title
	}
	if desc, ok := props["description"].(string); ok {
		evt.Description = desc
	}
	if loc, ok := props["location"].(string); ok {
		evt.Location = loc
	}
	if start, ok := props["start"].(string); ok {
		if t, err := time.Parse(time.RFC3339, start); err == nil {
			evt.DTStart = t
		}
	}
	if end, ok := props["end"].(string); ok {
		if t, err := time.Parse(time.RFC3339, end); err == nil {
			evt.DTEnd = &t
		}
	}
	if dur, ok := props["duration"].(string); ok {
		evt.Duration = dur
	}
	if allDay, ok := props["isAllDay"].(bool); ok {
		evt.AllDay = allDay
	}
	if calID, ok := props["calendarId"].(string); ok {
		evt.CalendarID = calID
	}
	if uid, ok := props["uid"].(string); ok {
		evt.UID = uid
	}
	if status, ok := props["status"].(string); ok && status != "" {
		evt.Status = strings.ToUpper(status)
	}
	if priority, ok := props["priority"].(float64); ok {
		evt.Priority = int(priority)
	}
	if rule, ok := parseJMAPRecurrence(props); ok {
		evt.RecurrenceRule = rule
	}
	if atts, ok := parseJMAPParticipants(props); ok {
		evt.Attendees = atts
	}

	return evt
}

func applyJMAPUpdates(evt *models.Event, props map[string]interface{}) {
	if title, ok := props["title"].(string); ok {
		evt.Summary = title
	}
	if desc, ok := props["description"].(string); ok {
		evt.Description = desc
	}
	if loc, ok := props["location"].(string); ok {
		evt.Location = loc
	}
	if start, ok := props["start"].(string); ok {
		if t, err := time.Parse(time.RFC3339, start); err == nil {
			evt.DTStart = t
		}
	}
	if end, ok := props["end"].(string); ok {
		if t, err := time.Parse(time.RFC3339, end); err == nil {
			evt.DTEnd = &t
		}
	}
	if dur, ok := props["duration"].(string); ok {
		evt.Duration = dur
	}
	if allDay, ok := props["isAllDay"].(bool); ok {
		evt.AllDay = allDay
	}
	if calID, ok := props["calendarId"].(string); ok && calID != "" {
		evt.CalendarID = calID
	}
	if status, ok := props["status"].(string); ok && status != "" {
		evt.Status = strings.ToUpper(status)
	}
	if priority, ok := props["priority"].(float64); ok {
		evt.Priority = int(priority)
	}
	if rule, ok := parseJMAPRecurrence(props); ok {
		evt.RecurrenceRule = rule
	}
	if atts, ok := parseJMAPParticipants(props); ok {
		evt.Attendees = atts
	}
	evt.Sequence++
}

func hasAttendees(raw json.RawMessage) bool {
	var atts []models.Attendee
	return json.Unmarshal(raw, &atts) == nil && len(atts) > 0
}

func parseJMAPParticipants(props map[string]interface{}) (json.RawMessage, bool) {
	if participants, ok := props["participants"].(map[string]interface{}); ok {
		var attendees []models.Attendee
		for _, p := range participants {
			if pMap, ok := p.(map[string]interface{}); ok {
				a := models.Attendee{
					Role:   "REQ-PARTICIPANT",
					Status: "NEEDS-ACTION",
					Type:   "INDIVIDUAL",
				}
				if email, ok := pMap["email"].(string); ok {
					a.Email = email
				}
				if name, ok := pMap["name"].(string); ok {
					a.DisplayName = name
				}
				if status, ok := pMap["participationStatus"].(string); ok {
					a.Status = status
				}
				if expectReply, ok := pMap["expectReply"].(bool); ok {
					a.RSVP = expectReply
				}
				if a.Email != "" {
					attendees = append(attendees, a)
				}
			}
		}
		b, err := json.Marshal(attendees)
		if err == nil {
			return json.RawMessage(b), true
		}
	}
	return nil, false
}

func parseJMAPRecurrence(props map[string]interface{}) (string, bool) {
	if rrules, ok := props["recurrenceRules"].([]interface{}); ok {
		if len(rrules) > 0 {
			if rmap, ok := rrules[0].(map[string]interface{}); ok {
				if rule, ok := rmap["rule"].(string); ok {
					return rule, true
				}
			}
		} else {
			return "", true
		}
	}
	return "", false
}

// maxStreamLife bounds how long one SSE connection may stay open. The client reconnects
// (re-authenticating with its token), so an expired or revoked token cannot keep a stream alive.
const maxStreamLife = 30 * time.Minute

// heartbeatInterval is how often an idle stream writes a keep-alive comment. The write also
// detects clients that disappeared. Override with SSE_HEARTBEAT_SECONDS (tests, proxies).
func heartbeatInterval() time.Duration {
	if v := os.Getenv("SSE_HEARTBEAT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 20 * time.Second
}

func writeSSE(w *bufio.Writer, m realtime.Message) error {
	if m.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", m.ID); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Event, m.Data); err != nil {
		return err
	}
	return w.Flush()
}

// EventSource streams live updates over Server-Sent Events (RFC 8620 §7.3):
//   - event "state":        JMAP StateChange for the caller's own account
//   - event "availability": busy/free status of teammates who share availability with the caller
//   - event "shares":       a share involving the caller changed; re-read the share lists
//   - event "resync":       events may have been missed; refetch everything
func (h *Handler) EventSource(c *fiber.Ctx) error {
	tenantID := c.Locals("tenant_id").(int)
	userID := c.Locals("user_id").(int)
	if h.hub == nil {
		return h.eventSourceOnce(c, tenantID, userID)
	}

	user, err := h.store.GetUserByID(c.UserContext(), userID)
	if err != nil || user == nil {
		return c.Status(401).JSON(fiber.Map{"error": "User not found"})
	}

	// The fiber context is recycled once this handler returns, so capture everything the
	// stream needs now and use an independent context inside the stream writer.
	ctx, cancel := context.WithTimeout(context.Background(), maxStreamLife)
	sub, unsubscribe, err := h.hub.Subscribe(ctx, user)
	if err != nil {
		cancel()
		return c.Status(429).JSON(fiber.Map{"error": err.Error()})
	}
	conn := c.Context().Conn()
	beat := heartbeatInterval()

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache, no-transform")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no") // tell nginx not to buffer the stream
	c.Status(200)

	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer cancel()
		defer unsubscribe()

		write := func(m realtime.Message) bool {
			if conn != nil {
				// fasthttp's WriteTimeout would otherwise cut long-lived streams.
				_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			}
			return writeSSE(w, m) == nil
		}

		if _, err := fmt.Fprint(w, "retry: 5000\n\n"); err != nil || w.Flush() != nil {
			return
		}
		for _, m := range h.hub.Snapshot(ctx, sub) {
			if !write(m) {
				return
			}
		}

		ticker := time.NewTicker(beat)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-sub.C:
				if !write(m) {
					return
				}
			case <-ticker.C:
				if conn != nil {
					_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				}
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || w.Flush() != nil {
					return
				}
			}
			if sub.Lagged() && !write(realtime.ResyncMessage()) {
				return
			}
		}
	})
	return nil
}

// eventSourceOnce is the legacy single-snapshot behaviour, used only when no realtime hub is configured.
func (h *Handler) eventSourceOnce(c *fiber.Ctx, tenantID, userID int) error {
	accountID := fmt.Sprintf("%d:%d", tenantID, userID)

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")

	state, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "CalendarEvent")
	calState, _ := h.store.GetJMAPState(c.UserContext(), tenantID, userID, "Calendar")

	payload := map[string]interface{}{
		"@type": "StateChange",
		"changed": map[string]interface{}{
			accountID: map[string]string{
				"CalendarEvent": state,
				"Calendar":      calState,
			},
		},
	}
	data, _ := json.Marshal(payload)
	return c.SendString(fmt.Sprintf("retry: 3000\r\nevent: state\r\ndata: %s\r\n\r\n", string(data)))
}

func jmapError(errType, description string) map[string]interface{} {
	return map[string]interface{}{
		"type":   errType,
		"status": 400,
		"detail": description,
	}
}

func jmapMethodError(errType, description, callID string) interface{} {
	return []interface{}{"error", map[string]string{
		"type":        errType,
		"description": description,
	}, callID}
}
