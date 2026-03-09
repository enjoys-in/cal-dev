package scheduling

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/smtp"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/jobs"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/google/uuid"
)

// Service implements RFC 6638 scheduling.
// CPU-intensive work (SMTP sends, ICS generation) is dispatched to the Valkey job queue.
type Service struct {
	store   *store.Store
	smtpCfg *config.SMTPConfig
	queue   *jobs.Queue
}

// NewService creates a new scheduling service.
func NewService(s *store.Store, smtpCfg *config.SMTPConfig, q *jobs.Queue) *Service {
	return &Service{store: s, smtpCfg: smtpCfg, queue: q}
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

// SendInvitation sends a scheduling invitation email (called by worker).
func (svc *Service) SendInvitation(ctx context.Context, msg *models.SchedulingMessage) error {
	if svc.smtpCfg.Host == "" {
		return fmt.Errorf("SMTP not configured")
	}

	from := svc.smtpCfg.From
	to := msg.RecipientEmail
	subject := fmt.Sprintf("Calendar Invitation: %s", msg.EventUID)

	body := fmt.Sprintf("From: %s\r\n"+
		"To: %s\r\n"+
		"Subject: %s\r\n"+
		"Content-Type: text/calendar; charset=utf-8; method=%s\r\n"+
		"MIME-Version: 1.0\r\n"+
		"\r\n"+
		"%s", from, to, subject, msg.Method, msg.RawICS)

	addr := fmt.Sprintf("%s:%d", svc.smtpCfg.Host, svc.smtpCfg.Port)
	var auth smtp.Auth
	if svc.smtpCfg.User != "" {
		auth = smtp.PlainAuth("", svc.smtpCfg.User, svc.smtpCfg.Password, svc.smtpCfg.Host)
	}

	if svc.smtpCfg.UseTLS {
		return svc.sendTLS(addr, auth, from, to, []byte(body))
	}

	return smtp.SendMail(addr, auth, from, []string{to}, []byte(body))
}

func (svc *Service) sendTLS(addr string, auth smtp.Auth, from, to string, body []byte) error {
	tlsConfig := &tls.Config{
		ServerName: svc.smtpCfg.Host,
	}

	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("TLS dial: %w", err)
	}

	client, err := smtp.NewClient(conn, svc.smtpCfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP client: %w", err)
	}
	defer client.Close()

	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("SMTP mail: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP rcpt: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP data: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("SMTP write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP close: %w", err)
	}

	return client.Quit()
}

// ProcessReply handles an incoming scheduling reply (PARTSTAT update).
func (svc *Service) ProcessReply(ctx context.Context, tenantID string, eventUID, attendeeEmail, partstat string) error {
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
func (svc *Service) SendReplyToOrganizer(ctx context.Context, tenantID string, eventUID, attendeeEmail, partstat string) error {
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
// It fetches the scheduling message from DB and sends it via SMTP.
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
			if err := svc.SendInvitation(ctx, msg); err != nil {
				svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "failed", err.Error())
				return fmt.Errorf("send to %s: %w", msg.RecipientEmail, err)
			}
			svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "sent", "")
			log.Printf("scheduling: sent %s to %s for event %s", msg.Method, msg.RecipientEmail, msg.EventUID)
			return nil
		}
	}

	return nil // message not found or already processed
}
