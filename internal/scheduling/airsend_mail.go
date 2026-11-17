package scheduling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/airsend"
	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/jobs"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/store"
)

// SetAirsendClient turns on delivery of internal accounts' calendar emails through
// airsend-api's internal route. nil = off.
func (svc *Service) SetAirsendClient(c *airsend.Client) {
	svc.airsend = c
}

// senderAccountID is the mail_accounts id the email is sent from: the event owner
// (organizer) for REQUEST/CANCEL, the answering guest for REPLY. Tenant id == account id.
func senderAccountID(msg *models.SchedulingMessage) int {
	if msg.SenderID > 0 {
		return msg.SenderID
	}
	return msg.TenantID
}

// isInternalSender reports whether the sender is an Airsend mailbox (account_origin 'internal').
// 'external' and unknown (NULL) accounts are not: they have no Airsend mailbox to send from.
// A read error is returned so the job is retried instead of guessing.
func (svc *Service) isInternalSender(ctx context.Context, msg *models.SchedulingMessage) (bool, error) {
	origin, err := svc.store.GetAccountOrigin(ctx, senderAccountID(msg))
	if err != nil {
		return false, fmt.Errorf("read account origin of %d: %w", senderAccountID(msg), err)
	}
	return origin == store.AccountOriginInternal, nil
}

// sendViaAirsend delivers msg through airsend-api. nil = accepted (status 'sent').
// A non-retryable answer marks the message 'failed' and returns nil (no retry); a retryable
// one returns an error so the worker retries on the 5m/10m/15m/25m/1h schedule.
func (svc *Service) sendViaAirsend(ctx context.Context, job *jobs.Job, msg *models.SchedulingMessage, event *models.Event) error {
	mail, err := svc.buildCalendarMail(ctx, job, msg, event)
	if err != nil {
		svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "failed", err.Error())
		log.Printf("scheduling: cannot build airsend request for %s: %v", msg.ID, err)
		return nil
	}

	err = svc.airsend.SendCalendarMail(ctx, mail)
	if err == nil {
		svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "sent", "")
		log.Printf("scheduling: airsend-api accepted %s for %s → event %s", msg.Method, msg.RecipientEmail, msg.EventUID)
		return nil
	}

	var se *airsend.SendError
	if errors.As(err, &se) && !se.Retryable {
		svc.store.UpdateSchedulingMessageStatus(ctx, msg.ID, "failed", err.Error())
		log.Printf("scheduling: airsend-api refused %s for %s (no retry): %v", msg.Method, msg.RecipientEmail, err)
		return nil
	}

	svc.recordSendFailure(ctx, job, msg, err)
	var hint time.Duration
	if se != nil {
		hint = se.RetryAfter
	}
	return jobs.WithRetryAfter(fmt.Errorf("airsend delivery for %s: %w", msg.RecipientEmail, err), hint)
}

// buildCalendarMail turns a scheduling message into the v1 request body.
func (svc *Service) buildCalendarMail(ctx context.Context, job *jobs.Job, msg *models.SchedulingMessage, event *models.Event) (*airsend.CalendarMail, error) {
	// A cancelled (deleted) event is gone from the store: read it back from the message's .ics
	if event == nil && msg.RawICS != "" {
		if evs, err := ics.ParseICSAll(msg.RawICS, msg.TenantID, senderAccountID(msg), ""); err == nil && len(evs) > 0 {
			event = evs[0]
		}
	}
	if event == nil {
		return nil, fmt.Errorf("event %s not found", msg.EventUID)
	}

	sender, err := svc.store.GetUserByID(ctx, senderAccountID(msg))
	if err != nil || sender == nil {
		return nil, fmt.Errorf("sender account %d not found", senderAccountID(msg))
	}

	var attendees []models.Attendee
	_ = json.Unmarshal(event.Attendees, &attendees)
	guests := make([]airsend.Attendee, 0, len(attendees))
	recipient := airsend.Person{Email: msg.RecipientEmail}
	for _, a := range attendees {
		guests = append(guests, airsend.Attendee{Email: a.Email, Name: a.DisplayName, Role: a.Role, Status: a.Status})
		if strings.EqualFold(a.Email, msg.RecipientEmail) {
			recipient.Name = a.DisplayName
		}
	}

	mailType := "invitation"
	switch msg.Method {
	case "CANCEL":
		mailType = "cancellation"
	case "REPLY":
		mailType = "reply"
	case "REQUEST":
		if event.Sequence > 0 {
			mailType = "update"
		}
	}

	ev := airsend.Event{
		UID:            event.UID,
		Sequence:       event.Sequence,
		Summary:        event.Summary,
		Description:    event.Description,
		Location:       event.Location,
		MeetingURL:     meetingURL(event.Location, event.Description),
		Start:          event.DTStart.UTC().Format(time.RFC3339),
		AllDay:         event.AllDay,
		TimeZone:       ics.EventTZID(event),
		RecurrenceRule: event.RecurrenceRule,
		Organizer:      strings.TrimPrefix(strings.TrimPrefix(event.Organizer, "mailto:"), "MAILTO:"),
		Attendees:      guests,
	}
	if event.DTEnd != nil {
		ev.End = event.DTEnd.UTC().Format(time.RFC3339)
	}

	mail := &airsend.CalendarMail{
		Version:   1,
		MessageID: msg.ID,
		Type:      mailType,
		Method:    msg.Method,
		TenantID:  msg.TenantID,
		Sender:    airsend.Sender{AccountID: sender.ID, Email: sender.Email, Name: sender.Name},
		Recipient: recipient,
		Event:     ev,
		ICS:       msg.RawICS,
		Attempt:   job.Retries + 1,
		CreatedAt: msg.CreatedAt.UTC().Format(time.RFC3339),
	}
	if msg.Method == "REQUEST" {
		acc, tent, decl := svc.rsvpLinks(msg, event)
		mail.RSVP = &airsend.RSVP{AcceptURL: acc, TentativeURL: tent, DeclineURL: decl}
	}
	return mail, nil
}

var (
	urlRe = regexp.MustCompile(`https?://[^\s<>"'\x60)\]]+`)
	// known meeting hosts — a link to one of these in the description counts as the meeting link
	meetingHostRe = regexp.MustCompile(`(?i)^https?://([a-z0-9-]+\.)*(meet\.google\.com|teams\.(microsoft|live)\.(com|us)|zoom(gov)?\.(us|com)|webex\.com|meet\.jit\.si|8x8\.vc|whereby\.com|gotomeeting\.com|meet\.goto\.com|chime\.aws|join\.skype\.com)(/|$)`)
)

// meetingURL: the first link in the location, otherwise a known meeting provider's link
// in the description. Same rule as the web client (lib/calendar/meeting-link.ts).
func meetingURL(location, description string) string {
	trim := func(u string) string { return strings.TrimRight(u, ".,;:!?") }
	if u := urlRe.FindString(location); u != "" {
		return trim(u)
	}
	for _, u := range urlRe.FindAllString(description, -1) {
		if u = trim(u); meetingHostRe.MatchString(u) {
			return u
		}
	}
	return ""
}
