package scheduling

import (
	"context"
	"fmt"
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
		rawICS = req.ICS
		if tid, ok := c.Locals("tenant_id").(int); ok && tid > 0 {
			tenantID = tid
		} else {
			tenantID = req.TenantID
		}
	} else {
		// Raw ICS body (text/calendar)
		rawICS = string(c.Body())
		if tid, ok := c.Locals("tenant_id").(int); ok && tid > 0 {
			tenantID = tid
		} else if tidStr := c.Get("X-Tenant-ID"); tidStr != "" {
			tenantID, _ = strconv.Atoi(tidStr)
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
		// Organizer cancelled the event — remove the recipient's copy (organizer must match).
		if err := h.svc.HandleInboundCancel(c.UserContext(), tenantID, rawICS); err != nil {
			log.Printf("scheduling: CANCEL for event %s not applied: %v", reply.EventUID, err)
			return response.Error(c, 422, "cancellation not applied", err.Error())
		}
		return response.Success(c, "Cancellation applied", fiber.Map{
			"method":    "CANCEL",
			"event_uid": reply.EventUID,
		})

	case "REQUEST":
		// New or updated invitation — add it to the recipient's calendar.
		if err := h.svc.HandleInboundRequest(c.UserContext(), tenantID, rawICS); err != nil {
			log.Printf("scheduling: REQUEST for event %s not applied: %v", reply.EventUID, err)
			return response.Error(c, 422, "invitation not applied", err.Error())
		}
		return response.Success(c, "Invitation added to calendar", fiber.Map{
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

	// Send REPLY email to organizer (use detached context since HTTP response may finish first)
	go func() {
		if err := h.svc.SendReplyToOrganizer(context.Background(), tenantID, req.EventUID, email, partstat); err != nil {
			log.Printf("scheduling: failed to send RSVP reply email: %v", err)
		}
	}()

	return response.Success(c, "RSVP recorded. Reply sent to organizer.", fiber.Map{
		"event_uid": req.EventUID,
		"attendee":  email,
		"response":  partstat,
	})
}

// PublicWebRSVP handles 1-click RSVP links clicked by external email recipients.
// GET or POST /api/scheduling/rsvp?token=...&action=ACCEPTED|DECLINED|TENTATIVE
func (h *Handler) PublicWebRSVP(c *fiber.Ctx) error {
	token := c.Query("token")
	if token == "" {
		token = c.FormValue("token")
	}
	action := strings.ToUpper(c.Query("action"))
	if action == "" {
		action = strings.ToUpper(c.FormValue("action"))
	}

	if token == "" {
		return c.Status(400).SendString("Invalid RSVP link: missing token")
	}

	payload, err := ParseRSVPToken(token, h.svc.jwtSecret)
	if err != nil {
		return c.Status(400).SendString("This invitation response link is invalid or has expired.")
	}

	switch action {
	case "ACCEPTED", "ACCEPT", "YES":
		action = "ACCEPTED"
	case "DECLINED", "DECLINE", "NO":
		action = "DECLINED"
	case "TENTATIVE", "MAYBE":
		action = "TENTATIVE"
	default:
		action = ""
	}

	// If no action selected yet, render a choice page
	if action == "" {
		html := fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Calendar RSVP</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #f8fafc; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; }
.card { background: white; padding: 32px; border-radius: 16px; box-shadow: 0 4px 12px rgba(0,0,0,0.06); text-align: center; max-width: 420px; width: 100%%; }
.btn { display: inline-block; padding: 10px 20px; margin: 8px 4px; border-radius: 8px; text-decoration: none; font-weight: 600; font-size: 14px; color: white; }
.btn-acc { background: #10B981; }
.btn-tent { background: #F59E0B; }
.btn-dec { background: #EF4444; }
</style></head><body>
<div class="card">
<h2>Respond to Invitation</h2>
<p style="color:#64748b; font-size:14px;">Responding as: <b>%s</b></p>
<div style="margin-top:24px;">
<a href="/api/scheduling/rsvp?token=%s&action=ACCEPTED" class="btn btn-acc">Accept</a>
<a href="/api/scheduling/rsvp?token=%s&action=TENTATIVE" class="btn btn-tent">Tentative</a>
<a href="/api/scheduling/rsvp?token=%s&action=DECLINED" class="btn btn-dec">Decline</a>
</div></div></body></html>`, payload.AttendeeEmail, token, token, token)
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(html)
	}

	// Update attendee status
	err = h.svc.ProcessReply(c.UserContext(), payload.TenantID, payload.EventUID, payload.AttendeeEmail, action)
	if err != nil {
		log.Printf("scheduling: public RSVP failed for %s: %v", payload.AttendeeEmail, err)
		return c.Status(404).SendString("Event not found or invitation has expired.")
	}

	// Send reply to organizer
	go func() {
		_ = h.svc.SendReplyToOrganizer(context.Background(), payload.TenantID, payload.EventUID, payload.AttendeeEmail, action)
	}()

	badgeColor := "#10B981"
	statusText := "Accepted"
	if action == "DECLINED" {
		badgeColor = "#EF4444"
		statusText = "Declined"
	} else if action == "TENTATIVE" {
		badgeColor = "#F59E0B"
		statusText = "Marked as Tentative"
	}

	html := fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>RSVP Confirmed</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #f8fafc; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; }
.card { background: white; padding: 40px; border-radius: 16px; box-shadow: 0 4px 12px rgba(0,0,0,0.06); text-align: center; max-width: 440px; width: 100%%; }
.badge { display: inline-block; width: 48px; height: 48px; line-height: 48px; border-radius: 50%%; background: %s; color: white; font-size: 24px; margin-bottom: 16px; }
h2 { margin: 0 0 8px 0; color: #0f172a; }
p { color: #64748b; font-size: 14px; margin: 8px 0; }
</style></head><body>
<div class="card">
<div class="badge">&#10003;</div>
<h2>RSVP Confirmed</h2>
<p>You have <b>%s</b> this invitation.</p>
<p style="font-size: 13px; color: #94a3b8; margin-top: 20px;">Email: %s</p>
<p style="font-size: 12px; color: #cbd5e1;">You can now close this tab.</p>
</div></body></html>`, badgeColor, statusText, payload.AttendeeEmail)

	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.SendString(html)
}
