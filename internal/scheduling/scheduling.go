package scheduling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/enjoys-in/cal-dev/internal/crypto"
	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/jobs"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/publisher"
	"github.com/enjoys-in/cal-dev/internal/smtp"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/google/uuid"
)

// Service implements RFC 6638 scheduling.
// Supports direct delivery via Calendar BYO SMTP, with fallback to Redis Pub/Sub.
type Service struct {
	store         *store.Store
	queue         *jobs.Queue
	publisher     *publisher.Publisher
	jwtSecret     string
	publicBaseURL string
}

// NewService creates a new scheduling service.
func NewService(s *store.Store, q *jobs.Queue, pub *publisher.Publisher) *Service {
	return &Service{store: s, queue: q, publisher: pub}
}

// SetSecrets sets encryption secrets and public URL for web RSVP links.
func (svc *Service) SetSecrets(jwtSecret, publicBaseURL string) {
	svc.jwtSecret = jwtSecret
	svc.publicBaseURL = strings.TrimRight(publicBaseURL, "/")
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

	if len(attendees) > models.MaxAttendeesPerEvent {
		attendees = attendees[:models.MaxAttendeesPerEvent]
	}

	// Verify organizer matches the authenticated event owner to prevent organizer spoofing
	if event.UserID != 0 {
		user, err := svc.store.GetUserByID(ctx, event.UserID)
		if err == nil && user != nil {
			orgEmail := strings.TrimPrefix(strings.ToLower(event.Organizer), "mailto:")
			if !strings.EqualFold(orgEmail, user.Email) {
				log.Printf("scheduling: skipping ProcessEvent: organizer %s does not match event owner %s", event.Organizer, user.Email)
				return nil
			}
		}
	}

	organizer := normEmail(event.Organizer)
	emails := make([]string, 0, len(attendees))
	for _, attendee := range attendees {
		emails = append(emails, attendee.Email)
	}
	users := svc.localUsers(ctx, emails) // one lookup for all attendees
	for _, attendee := range attendees {
		if normEmail(attendee.Email) == organizer {
			continue // the organizer never invites themselves
		}
		// Local attendees get the event in their own calendar straight away...
		svc.deliverLocal(ctx, event, attendee, method, users)
		// ...and everyone also gets the standard iMIP email.
		svc.enqueueMessage(ctx, event, attendee, method)
	}

	return nil
}

// enqueueMessage records one scheduling message and queues it for delivery by the mail server.
func (svc *Service) enqueueMessage(ctx context.Context, event *models.Event, attendee models.Attendee, method string) {
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
		return
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
	_, err := svc.setAttendeeStatus(ctx, tenantID, eventUID, attendeeEmail, partstat)
	return err
}

// setAttendeeStatus sets one attendee's PARTSTAT on the copy of eventUID held by tenantID
// and returns the updated event.
func (svc *Service) setAttendeeStatus(ctx context.Context, tenantID int, eventUID, attendeeEmail, partstat string) (*models.Event, error) {
	// Work on the whole resource (master + overrides): a response applies to the attendee
	// throughout the series, and regenerating must not lose any component.
	rows, err := svc.store.FindResourceByUID(ctx, tenantID, eventUID)
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("event not found: %s", eventUID)
	}

	foundAny := false
	for _, event := range rows {
		var attendees []models.Attendee
		if err := json.Unmarshal(event.Attendees, &attendees); err != nil {
			return nil, fmt.Errorf("parse attendees: %w", err)
		}
		for i, a := range attendees {
			if strings.EqualFold(a.Email, attendeeEmail) {
				attendees[i].Status = partstat
				foundAny = true
				break
			}
		}
		attendeesJSON, _ := json.Marshal(attendees)
		event.Attendees = json.RawMessage(attendeesJSON)
	}
	if !foundAny {
		return nil, fmt.Errorf("attendee %s not found on event %s", attendeeEmail, eventUID)
	}

	// Note: RFC 5546 §2.1.4: Only organizer increments SEQUENCE; attendee replies do not bump it.
	rows = ics.FinalizeResource(rows)
	if err := svc.store.ReplaceResource(ctx, tenantID, rows[0].CalendarID, eventUID, rows); err != nil {
		return nil, err
	}
	return rows[0], nil
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

	return svc.replyToOrganizer(ctx, events[0], attendeeEmail, partstat)
}

// replyToOrganizer delivers an attendee's response for event (the attendee's own copy).
// A local organizer is updated directly; an external one gets a METHOD:REPLY email.
func (svc *Service) replyToOrganizer(ctx context.Context, event *models.Event, attendeeEmail, partstat string) error {
	tenantID, eventUID := event.TenantID, event.UID

	if handled, err := svc.propagateReply(ctx, event, attendeeEmail, partstat); handled {
		return err
	}

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
// It prioritizes calendar BYO SMTP when configured, falling back to Redis Pub/Sub for managed mode.
func (svc *Service) HandleSendEmailJob(ctx context.Context, job *jobs.Job) error {
	payload, err := jobs.ParsePayload[jobs.SchedulingEmailPayload](job)
	if err != nil {
		return err
	}

	// Fetch just this message (not every pending message of the tenant)
	msg, err := svc.store.GetPendingSchedulingMessage(ctx, payload.TenantID, payload.MessageID)
	if err != nil {
		return fmt.Errorf("fetch pending message: %w", err)
	}
	if msg == nil {
		return nil // message not found or already processed
	}

	// Fetch the event for enriching the mail payload
	var event *models.Event
	events, err := svc.store.QueryEvents(ctx, &models.EventFilter{
		TenantID: msg.TenantID,
		UIDs:     []string{msg.EventUID},
	})
	if err == nil && len(events) > 0 {
		event = events[0]
	}

	// 1. Try delivery via Calendar BYO SMTP if configured
	if event != nil && event.CalendarID != "" {
		smtpCfg, err := svc.store.GetCalendarSMTP(ctx, msg.TenantID, event.CalendarID)
		if err == nil && smtpCfg != nil && smtpCfg.Status == "active" {
			if err := svc.sendViaBYOSMTP(ctx, msg, event, smtpCfg); err != nil {
				svc.recordSendFailure(ctx, job, msg, err)
				return fmt.Errorf("BYO SMTP delivery for %s: %w", msg.RecipientEmail, err)
			}
			svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "sent", "")
			log.Printf("scheduling: delivered %s via BYO SMTP for %s → event %s", msg.Method, msg.RecipientEmail, msg.EventUID)
			return nil
		}
	}

	// 2. Fallback to managed mail server via Redis Pub/Sub if configured
	if svc.publisher != nil {
		if err := svc.SendInvitation(ctx, msg, event); err != nil {
			svc.recordSendFailure(ctx, job, msg, err)
			return fmt.Errorf("publish for %s: %w", msg.RecipientEmail, err)
		}
		svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "sent", "")
		log.Printf("scheduling: published %s for %s → event %s", msg.Method, msg.RecipientEmail, msg.EventUID)
		return nil
	}

	// 3. Neither BYO SMTP nor managed mail service is configured
	errMsg := "Calendar has no SMTP configured and managed fallback is disabled"
	svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "unconfigured_smtp", errMsg)
	log.Printf("scheduling: skipping delivery for %s: %s", msg.RecipientEmail, errMsg)
	return nil
}

// recordSendFailure keeps the message 'pending' (with its error) so the worker's retry can
// pick it up again, and marks it 'failed' only on the final attempt.
func (svc *Service) recordSendFailure(ctx context.Context, job *jobs.Job, msg *models.SchedulingMessage, err error) {
	if job.Retries >= job.MaxRetry {
		svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "failed", err.Error())
		return
	}
	svc.store.RecordSchedulingMessageError(ctx, msg.ID, err.Error())
}

// warnLocalhostOnce logs once when RSVP links would point at localhost.
var warnLocalhostOnce sync.Once

// sendViaBYOSMTP sends an invitation email directly via the calendar's custom SMTP server.
func (svc *Service) sendViaBYOSMTP(ctx context.Context, msg *models.SchedulingMessage, event *models.Event, smtpCfg *models.CalendarSMTP) error {
	decryptedPassword, err := crypto.DecryptWithFallback(smtpCfg.PasswordEncrypted, svc.jwtSecret)
	if err != nil {
		return fmt.Errorf("decrypt smtp password: %w", err)
	}

	baseURL := svc.publicBaseURL
	if baseURL == "" {
		baseURL = "http://localhost:8443"
	}
	if strings.Contains(baseURL, "localhost") {
		warnLocalhostOnce.Do(func() {
			log.Printf("scheduling: WARNING: public URL %q contains localhost; RSVP links in emails will not work for recipients", baseURL)
		})
	}

	startFmt := ""
	endFmt := ""
	summary := "Calendar Event"
	description := ""
	location := ""
	organizer := ""

	if event != nil {
		if event.AllDay {
			// Date only; DTEND of an all-day event is exclusive, so show the last day.
			startFmt = event.DTStart.UTC().Format("Mon, 02 Jan 2006")
			if event.DTEnd != nil {
				if last := event.DTEnd.UTC().AddDate(0, 0, -1); last.After(event.DTStart.UTC()) {
					endFmt = last.Format("Mon, 02 Jan 2006")
				}
			}
		} else {
			// Times are shown in the event's own zone with its abbreviation; UTC when it has none.
			loc := ics.EventLocation(event)
			if loc == nil {
				loc = time.UTC
			}
			const layout = "Mon, 02 Jan 2006 15:04 MST"
			startFmt = event.DTStart.In(loc).Format(layout)
			if event.DTEnd != nil {
				endFmt = event.DTEnd.In(loc).Format(layout)
			}
		}
		summary = event.Summary
		description = event.Description
		location = event.Location
		organizer = event.Organizer
	}

	subject := fmt.Sprintf("Invitation: %s", summary)
	if msg.Method == "CANCEL" {
		subject = fmt.Sprintf("Canceled: %s", summary)
	} else if msg.Method == "REQUEST" && event != nil && event.Sequence > 0 {
		subject = fmt.Sprintf("Updated: %s", summary)
	} else if msg.Method == "REPLY" {
		subject = fmt.Sprintf("RSVP: %s", summary)
	}

	invData := &smtp.InvitationData{
		SenderEmail:    smtpCfg.FromEmail,
		SenderName:     smtpCfg.FromName,
		RecipientEmail: msg.RecipientEmail,
		ReplyTo:        smtpCfg.ReplyTo,
		Subject:        subject,
		Method:         msg.Method,
		Summary:        summary,
		Description:    description,
		Location:       location,
		StartFormatted: startFmt,
		EndFormatted:   endFmt,
		Organizer:      organizer,
		RawICS:         msg.RawICS,
	}

	// RSVP links only make sense on invitations (not on cancellations or replies).
	if msg.Method == "REQUEST" {
		// Valid for at least 30 days, and until a day after the event ends.
		ttl := 30 * 24 * time.Hour
		if event != nil {
			end := event.DTStart
			if event.DTEnd != nil {
				end = *event.DTEnd
			}
			if d := time.Until(end.Add(24 * time.Hour)); d > ttl {
				ttl = d
			}
		}
		rsvpToken := url.QueryEscape(GenerateRSVPToken(msg.TenantID, msg.EventUID, msg.RecipientEmail, svc.jwtSecret, ttl))
		invData.RSVPAcceptURL = fmt.Sprintf("%s/api/public/rsvp?token=%s&action=ACCEPTED", baseURL, rsvpToken)
		invData.RSVPTentURL = fmt.Sprintf("%s/api/public/rsvp?token=%s&action=TENTATIVE", baseURL, rsvpToken)
		invData.RSVPDeclURL = fmt.Sprintf("%s/api/public/rsvp?token=%s&action=DECLINED", baseURL, rsvpToken)
	}

	mimeBytes, err := smtp.BuildInvitationMIME(invData)
	if err != nil {
		return fmt.Errorf("build mime: %w", err)
	}

	clientCfg := &smtp.Config{
		Host:       smtpCfg.Host,
		Port:       smtpCfg.Port,
		Username:   smtpCfg.Username,
		Password:   decryptedPassword,
		FromEmail:  smtpCfg.FromEmail,
		FromName:   smtpCfg.FromName,
		ReplyTo:    smtpCfg.ReplyTo,
		Encryption: smtpCfg.Encryption,
	}

	if err := smtp.SendMessage(clientCfg, msg.RecipientEmail, mimeBytes); err != nil {
		// A connection/auth failure means the config itself is broken: mark it so later
		// messages fall back to managed delivery.
		if errors.Is(err, smtp.ErrConnect) {
			_ = svc.store.UpdateCalendarSMTPTestResult(ctx, smtpCfg.TenantID, smtpCfg.CalendarID, "failing", err.Error())
		}
		return err
	}
	return nil
}
