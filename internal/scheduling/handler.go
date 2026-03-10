package scheduling

import (
	"log"
	"strconv"
	"strings"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/response"
	"github.com/gofiber/fiber/v2"
)

// Handler provides REST API endpoints for inbound scheduling (iMIP replies).
type Handler struct {
	svc *Service
}

// NewHandler creates a new scheduling API handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// IncomingIMIP handles an incoming iMIP message (METHOD:REPLY, METHOD:CANCEL, etc.)
// from the mail system. When a user replies Accept/Decline/Tentative to a calendar
// invite email, the mail server should POST the ICS body here.
//
// POST /api/scheduling/incoming
//
// Accepts two content types:
//   - application/json with {"tenant_id": "...", "ics": "BEGIN:VCALENDAR..."}
//   - text/calendar with raw ICS body + X-Tenant-ID header
func (h *Handler) IncomingIMIP(c *fiber.Ctx) error {
	var tenantID int
	var rawICS string

	contentType := strings.ToLower(c.Get("Content-Type"))

	if strings.Contains(contentType, "application/json") {
		// JSON body: {"tenant_id": 123, "ics": "BEGIN:VCALENDAR..."}
		var req struct {
			TenantID int    `json:"tenant_id"`
			ICS      string `json:"ics"`
			// Optional: override attendee email (mail system knows the sender)
			SenderEmail string `json:"sender_email"`
		}
		if err := c.BodyParser(&req); err != nil {
			return response.Error(c, 400, "invalid request body")
		}
		tenantID = req.TenantID
		rawICS = req.ICS

		if tenantID == 0 {
			// Fall back to auth context
			if tid, ok := c.Locals("tenant_id").(int); ok {
				tenantID = tid
			}
		}
	} else {
		// Raw ICS body (text/calendar)
		rawICS = string(c.Body())
		if tidStr := c.Get("X-Tenant-ID"); tidStr != "" {
			tenantID, _ = strconv.Atoi(tidStr)
		}
		if tenantID == 0 {
			if tid, ok := c.Locals("tenant_id").(int); ok {
				tenantID = tid
			}
		}
	}

	if rawICS == "" {
		return response.Error(c, 400, "no ICS data provided")
	}
	if tenantID == 0 {
		return response.Error(c, 400, "tenant_id is required (via JSON body, X-Tenant-ID header, or auth context)")
	}

	// Parse the scheduling ICS
	reply, err := ics.ParseSchedulingICS(rawICS)
	if err != nil {
		return response.Error(c, 400, "failed to parse scheduling ICS", err.Error())
	}

	log.Printf("scheduling: incoming %s for event %s from %s (partstat=%s)",
		reply.Method, reply.EventUID, reply.AttendeeEmail, reply.Partstat)

	switch reply.Method {
	case "REPLY":
		// Attendee replied to an invitation — update their PARTSTAT
		if reply.AttendeeEmail == "" {
			return response.Error(c, 400, "no attendee found in REPLY")
		}
		if reply.Partstat == "" {
			reply.Partstat = "NEEDS-ACTION"
		}

		err := h.svc.ProcessReply(c.UserContext(), tenantID, reply.EventUID, reply.AttendeeEmail, reply.Partstat)
		if err != nil {
			log.Printf("scheduling: failed to process reply: %v", err)
			return response.Error(c, 404, "failed to process reply", err.Error())
		}

		return response.Success(c, "Attendee status updated successfully", fiber.Map{
			"method":    "REPLY",
			"event_uid": reply.EventUID,
			"attendee":  reply.AttendeeEmail,
			"partstat":  reply.Partstat,
		})

	case "CANCEL":
		// Organizer cancelled the event — we could mark it as CANCELLED
		log.Printf("scheduling: received CANCEL for event %s", reply.EventUID)
		return response.Success(c, "Cancellation acknowledged", fiber.Map{
			"method":    "CANCEL",
			"event_uid": reply.EventUID,
		})

	case "REQUEST":
		// New invitation — could auto-create event in attendee's calendar
		log.Printf("scheduling: received REQUEST for event %s", reply.EventUID)
		return response.Success(c, "Invitation received", fiber.Map{
			"method":    "REQUEST",
			"event_uid": reply.EventUID,
		})

	default:
		return response.Error(c, 400, "unsupported scheduling method", "supported methods: REPLY, CANCEL, REQUEST")
	}
}

// RSVPRespond allows the authenticated user to RSVP to an event they've been invited to.
// This updates the attendee's PARTSTAT and optionally sends a REPLY back to the organizer.
//
// POST /api/scheduling/rsvp
func (h *Handler) RSVPRespond(c *fiber.Ctx) error {
	tenantID, _ := c.Locals("tenant_id").(int)
	email, _ := c.Locals("email").(string)

	var req struct {
		EventUID string `json:"event_uid"`
		Response string `json:"response"` // ACCEPTED, DECLINED, TENTATIVE
		Comment  string `json:"comment"`  // optional comment
	}
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, 400, "invalid request body")
	}

	if req.EventUID == "" {
		return response.Error(c, 400, "event_uid is required")
	}

	// Normalize response
	partstat := strings.ToUpper(req.Response)
	switch partstat {
	case "ACCEPTED", "DECLINED", "TENTATIVE":
		// valid
	case "ACCEPT", "YES":
		partstat = "ACCEPTED"
	case "DECLINE", "NO":
		partstat = "DECLINED"
	case "MAYBE":
		partstat = "TENTATIVE"
	default:
		return response.Error(c, 400, "invalid response", "valid values: ACCEPTED, DECLINED, TENTATIVE (or: accept, decline, maybe)")
	}

	// Use the authenticated user's email as the attendee
	if email == "" {
		return response.Error(c, 400, "could not determine attendee email from auth context")
	}

	// Update the attendee's status on the organizer's event
	err := h.svc.ProcessReply(c.UserContext(), tenantID, req.EventUID, email, partstat)
	if err != nil {
		return response.Error(c, 404, "event not found or you are not an attendee", err.Error())
	}

	// Send REPLY email to organizer
	go func() {
		if err := h.svc.SendReplyToOrganizer(c.UserContext(), tenantID, req.EventUID, email, partstat); err != nil {
			log.Printf("scheduling: failed to send RSVP reply email: %v", err)
		}
	}()

	return response.Success(c, "RSVP recorded. Reply sent to organizer.", fiber.Map{
		"event_uid": req.EventUID,
		"attendee":  email,
		"response":  partstat,
	})
}
