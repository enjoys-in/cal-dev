package scheduling

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/jobs"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/publisher"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/google/uuid"
)

// Service implements RFC 6638 scheduling.
// Instead of sending emails directly, it publishes mail events to Redis Pub/Sub.
// The internal mail server (separate repo) subscribes and handles actual delivery.
type Service struct {
	store     *store.Store
	queue     *jobs.Queue
	publisher *publisher.Publisher
}

// NewService creates a new scheduling service.
func NewService(s *store.Store, q *jobs.Queue, pub *publisher.Publisher) *Service {
	return &Service{store: s, queue: q, publisher: pub}
}

// ProcessEvent handles scheduling logic when an event is created/updated.
// Instead of sending emails synchronously, it enqueues jobs to Valkey.
func (svc *Service) ProcessEvent(ctx context.Context, event *models.Event, method string) error {
	var attendees []models.Attendee
	if err := json.Unmarshal(event.Attendees, &attendees); err != nil {
		return nil // No attendees, nothing to schedule
	}

	if len(attendees) == 0 || event.Organizer == "" {
		return nil
	}

	for _, attendee := range attendees {
		msg := &models.SchedulingMessage{
			ID:             uuid.New().String(),
			TenantID:       event.TenantID,
			SenderID:       event.UserID,
			RecipientEmail: attendee.Email,
			EventUID:       event.UID,
			Method:         method,
			Status:         "pending",
			RawICS:         ics.GenerateSchedulingICS(event, method),
		}

		if err := svc.store.CreateSchedulingMessage(ctx, msg); err != nil {
			log.Printf("scheduling: failed to create message for %s: %v", attendee.Email, err)
			continue
		}

		// Enqueue to Valkey for background processing
		if svc.queue != nil {
			payload := &jobs.SchedulingEmailPayload{
				MessageID: msg.ID,
				TenantID:  event.TenantID,
			}
			if _, err := svc.queue.Enqueue(ctx, jobs.JobSendSchedulingEmail, payload); err != nil {
				log.Printf("scheduling: failed to enqueue email job for %s: %v", attendee.Email, err)
			}
		}
	}

	return nil
}

// SendInvitation publishes a scheduling message to Redis Pub/Sub
// for the internal mail server to pick up and deliver.
func (svc *Service) SendInvitation(ctx context.Context, msg *models.SchedulingMessage, event *models.Event) error {
	if svc.publisher == nil {
		return fmt.Errorf("publisher not configured")
	}

	// Determine event type from method
	eventType := "invitation"
	switch msg.Method {
	case "CANCEL":
		eventType = "cancellation"
	case "REPLY":
		eventType = "reply"
	case "REQUEST":
		if event != nil && event.Sequence > 0 {
			eventType = "update"
		}
	}

	// Build attendee list from event
	var attendees []publisher.Attendee
	if event != nil {
		var modelAttendees []models.Attendee
		if err := json.Unmarshal(event.Attendees, &modelAttendees); err == nil {
			for _, a := range modelAttendees {
				attendees = append(attendees, publisher.Attendee{
					Email:       a.Email,
					DisplayName: a.DisplayName,
					Role:        a.Role,
					Status:      a.Status,
				})
			}
		}
	}

	// Build the mail event payload
	mailEvt := &publisher.MailEvent{
		EventType:      eventType,
		Method:         msg.Method,
		TenantID:       msg.TenantID,
		SenderID:       msg.SenderID,
		RecipientEmail: msg.RecipientEmail,
		EventUID:       msg.EventUID,
		RawICS:         msg.RawICS,
		MessageID:      msg.ID,
		Attendees:      attendees,
	}

	// Enrich with event details if available
	if event != nil {
		mailEvt.Summary = event.Summary
		mailEvt.Description = event.Description
		mailEvt.Location = event.Location
		mailEvt.DTStart = event.DTStart.Format(time.RFC3339)
		if event.DTEnd != nil {
			mailEvt.DTEnd = event.DTEnd.Format(time.RFC3339)
		}
		mailEvt.AllDay = event.AllDay
		mailEvt.Organizer = event.Organizer
		mailEvt.SenderEmail = event.Organizer
		if strings.HasPrefix(strings.ToLower(mailEvt.SenderEmail), "mailto:") {
			mailEvt.SenderEmail = mailEvt.SenderEmail[7:]
		}
	}

	return svc.publisher.PublishMailEvent(ctx, mailEvt)
}

// ProcessReply handles an incoming scheduling reply (PARTSTAT update).
func (svc *Service) ProcessReply(ctx context.Context, tenantID int, eventUID, attendeeEmail, partstat string) error {
	events, err := svc.store.QueryEvents(ctx, &models.EventFilter{
		TenantID: tenantID,
		UIDs:     []string{eventUID},
	})
	if err != nil || len(events) == 0 {
		return fmt.Errorf("event not found: %s", eventUID)
	}

	event := events[0]

	var attendees []models.Attendee
	if err := json.Unmarshal(event.Attendees, &attendees); err != nil {
		return fmt.Errorf("parse attendees: %w", err)
	}

	for i, a := range attendees {
		if a.Email == attendeeEmail {
			attendees[i].Status = partstat
			break
		}
	}

	attendeesJSON, _ := json.Marshal(attendees)
	event.Attendees = json.RawMessage(attendeesJSON)
	event.Sequence++
	event.ETag = ics.GenerateETag(event.UID + fmt.Sprintf("%d", event.Sequence))
	event.RawICS = ics.GenerateICS(event)
	event.SizeBytes = len(event.RawICS)

	return svc.store.UpdateEvent(ctx, event)
}

// SendCancellation sends a METHOD:CANCEL for a deleted event.
func (svc *Service) SendCancellation(ctx context.Context, event *models.Event) error {
	return svc.ProcessEvent(ctx, event, "CANCEL")
}

// SendReplyToOrganizer sends a METHOD:REPLY email to the event organizer
// when an attendee RSVPs (accept/decline/tentative).
func (svc *Service) SendReplyToOrganizer(ctx context.Context, tenantID int, eventUID, attendeeEmail, partstat string) error {
	events, err := svc.store.QueryEvents(ctx, &models.EventFilter{
		TenantID: tenantID,
		UIDs:     []string{eventUID},
	})
	if err != nil || len(events) == 0 {
		return fmt.Errorf("event not found: %s", eventUID)
	}

	event := events[0]

	// Extract organizer email
	organizerEmail := event.Organizer
	if strings.HasPrefix(strings.ToLower(organizerEmail), "mailto:") {
		organizerEmail = organizerEmail[7:]
	}
	if organizerEmail == "" {
		return fmt.Errorf("no organizer email on event %s", eventUID)
	}

	// Generate a METHOD:REPLY ICS
	replyICS := generateReplyICS(event, attendeeEmail, partstat)

	msg := &models.SchedulingMessage{
		ID:             uuid.New().String(),
		TenantID:       tenantID,
		SenderID:       event.UserID,
		RecipientEmail: organizerEmail,
		EventUID:       eventUID,
		Method:         "REPLY",
		Status:         "pending",
		RawICS:         replyICS,
	}

	if err := svc.store.CreateSchedulingMessage(ctx, msg); err != nil {
		return fmt.Errorf("create reply message: %w", err)
	}

	// Enqueue for background sending
	if svc.queue != nil {
		payload := &jobs.SchedulingEmailPayload{
			MessageID: msg.ID,
			TenantID:  tenantID,
		}
		if _, err := svc.queue.Enqueue(ctx, jobs.JobSendSchedulingEmail, payload); err != nil {
			log.Printf("scheduling: failed to enqueue reply email: %v", err)
		}
	}

	log.Printf("scheduling: queued REPLY (%s) from %s to organizer %s for event %s",
		partstat, attendeeEmail, organizerEmail, eventUID)
	return nil
}

// generateReplyICS generates a METHOD:REPLY ICS for an RSVP response.
func generateReplyICS(event *models.Event, attendeeEmail, partstat string) string {
	now := time.Now().UTC().Format("20060102T150405Z")
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//CalDev//CalDev 1.0//EN\r\n")
	b.WriteString("METHOD:REPLY\r\n")
	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString(fmt.Sprintf("UID:%s\r\n", event.UID))
	b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", now))
	b.WriteString(fmt.Sprintf("DTSTART:%s\r\n", event.DTStart.UTC().Format("20060102T150405Z")))
	if event.DTEnd != nil {
		b.WriteString(fmt.Sprintf("DTEND:%s\r\n", event.DTEnd.UTC().Format("20060102T150405Z")))
	}
	b.WriteString(fmt.Sprintf("SUMMARY:%s\r\n", event.Summary))
	if event.Organizer != "" {
		org := event.Organizer
		if !strings.HasPrefix(strings.ToLower(org), "mailto:") {
			org = "mailto:" + org
		}
		b.WriteString(fmt.Sprintf("ORGANIZER:%s\r\n", org))
	}
	b.WriteString(fmt.Sprintf("ATTENDEE;PARTSTAT=%s:mailto:%s\r\n", partstat, attendeeEmail))
	b.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", event.Sequence))
	b.WriteString("END:VEVENT\r\n")
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// HandleSendEmailJob is the worker handler for send_scheduling_email jobs.
// It fetches the scheduling message from DB and publishes it to Redis for the mail server.
func (svc *Service) HandleSendEmailJob(ctx context.Context, job *jobs.Job) error {
	payload, err := jobs.ParsePayload[jobs.SchedulingEmailPayload](job)
	if err != nil {
		return err
	}

	// Fetch the pending message
	messages, err := svc.store.GetPendingSchedulingMessages(ctx, payload.TenantID)
	if err != nil {
		return fmt.Errorf("fetch pending messages: %w", err)
	}

	for _, msg := range messages {
		if msg.ID == payload.MessageID {
			// Fetch the event for enriching the mail payload
			var event *models.Event
			events, err := svc.store.QueryEvents(ctx, &models.EventFilter{
				TenantID: msg.TenantID,
				UIDs:     []string{msg.EventUID},
			})
			if err == nil && len(events) > 0 {
				event = events[0]
			}

			if err := svc.SendInvitation(ctx, msg, event); err != nil {
				svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "failed", err.Error())
				return fmt.Errorf("publish for %s: %w", msg.RecipientEmail, err)
			}
			svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "sent", "")
			log.Printf("scheduling: published %s for %s → event %s", msg.Method, msg.RecipientEmail, msg.EventUID)
			return nil
		}
	}

	return nil // message not found or already processed
}
