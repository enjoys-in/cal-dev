package scheduling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/jobs"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/publisher"
	"github.com/enjoys-in/cal-dev/internal/recurrence"
)

// DefaultReminderMilestones are the countdown intervals evaluated for events.
var DefaultReminderMilestones = []int{15, 10, 5, 0}

// ReminderJobPayload is serialized into Valkey delayed jobs for countdown reminders.
// Summary and Location are no longer set by the scheduler (the handler reads the current event),
// so the payload, and with it the delayed job, stays identical across title edits.
type ReminderJobPayload struct {
	TenantID         int       `json:"tenant_id"`
	CalendarID       string    `json:"calendar_id,omitempty"`
	EventUID         string    `json:"event_uid"`
	RecurrenceID     string    `json:"recurrence_id,omitempty"`
	Summary          string    `json:"summary"`
	Location         string    `json:"location,omitempty"`
	StartUTC         time.Time `json:"start_utc"`
	RecipientEmail   string    `json:"recipient_email"`
	RecipientUserID  int       `json:"recipient_user_id,omitempty"`
	MilestoneMinutes int       `json:"milestone_minutes"`
	OrganizerEmail   string    `json:"organizer_email,omitempty"`
}

// ReminderRecipient represents an individual (organizer or attendee) eligible for a reminder.
type ReminderRecipient struct {
	UserID int
	Email  string
	Name   string
}

// resolveRecipients gathers the organizer and all non-declining attendees for an event.
func (svc *Service) resolveRecipients(ctx context.Context, event *models.Event) []ReminderRecipient {
	recipients := make(map[string]ReminderRecipient)

	orgEmail := normEmail(event.Organizer)
	var atts []models.Attendee
	if len(event.Attendees) > 0 {
		_ = json.Unmarshal(event.Attendees, &atts)
	}

	// Look up the organizer and every attendee in one query instead of one per person.
	var users map[string]*models.User
	if svc.store != nil {
		emails := []string{orgEmail}
		for _, a := range atts {
			if !strings.EqualFold(a.Status, "DECLINED") {
				emails = append(emails, normEmail(a.Email))
			}
		}
		users, _ = svc.store.GetUsersByEmails(ctx, emails)
	}

	// 1. Organizer
	if orgEmail != "" {
		uid := 0
		name := orgEmail
		if u := users[orgEmail]; u != nil {
			uid = u.ID
			if u.Name != "" {
				name = u.Name
			}
		}
		recipients[orgEmail] = ReminderRecipient{UserID: uid, Email: orgEmail, Name: name}
	}

	// 2. Attendees (skip DECLINED)
	for _, a := range atts {
		em := normEmail(a.Email)
		if em == "" || strings.EqualFold(a.Status, "DECLINED") {
			continue
		}
		uid := 0
		name := em
		if u := users[em]; u != nil {
			uid = u.ID
			if u.Name != "" {
				name = u.Name
			}
		}
		recipients[em] = ReminderRecipient{UserID: uid, Email: em, Name: name}
	}

	out := make([]ReminderRecipient, 0, len(recipients))
	for _, r := range recipients {
		if len(out) >= models.MaxAttendeesPerEvent {
			break
		}
		out = append(out, r)
	}
	return out
}

// isLocalInvitationCopy reports whether e is an attendee's copy of an event whose organizer has
// an account here: the organizer's own copy schedules everyone's reminders.
func isLocalInvitationCopy(e *models.Event) bool {
	var p struct {
		Invitation struct {
			OrganizerUserID int `json:"organizer_user_id"`
		} `json:"invitation"`
	}
	if len(e.Properties) > 0 {
		_ = json.Unmarshal(e.Properties, &p)
	}
	return p.Invitation.OrganizerUserID != 0 && p.Invitation.OrganizerUserID != e.UserID
}

// ownerScope keeps every recipient when the copy belongs to the organizer, otherwise only the
// copy's owner (e.g. an invitation from an external organizer), so each person is reminded once.
func ownerScope(e *models.Event, recipients []ReminderRecipient) []ReminderRecipient {
	org := normEmail(e.Organizer)
	for _, r := range recipients {
		if r.Email == org && r.UserID != 0 && r.UserID == e.UserID {
			return recipients
		}
	}
	var out []ReminderRecipient
	for _, r := range recipients {
		if r.UserID != 0 && r.UserID == e.UserID {
			out = append(out, r)
		}
	}
	return out
}

// isActiveParticipant reports whether email is e's organizer or a non-declining attendee.
func isActiveParticipant(e *models.Event, email string) bool {
	email = normEmail(email)
	if email == "" {
		return false
	}
	if normEmail(e.Organizer) == email {
		return true
	}
	var atts []models.Attendee
	if len(e.Attendees) > 0 {
		_ = json.Unmarshal(e.Attendees, &atts)
	}
	for _, a := range atts {
		if normEmail(a.Email) == email {
			return !strings.EqualFold(a.Status, "DECLINED")
		}
	}
	return false
}

// occurrenceRecurrenceID is the RECURRENCE-ID used for an occurrence in reminder payloads.
func occurrenceRecurrenceID(master *models.Event, occ recurrence.Occurrence) string {
	recID := occ.Event.RecurrenceID
	if recID == "" && master.RecurrenceRule != "" {
		if master.AllDay {
			recID = time.Date(occ.Start.Year(), occ.Start.Month(), occ.Start.Day(), 0, 0, 0, 0, time.UTC).Format("20060102")
		} else {
			recID = occ.Start.UTC().Format("20060102T150405Z")
		}
	}
	return recID
}

// reminderJobID is deterministic, so re-scheduling the same reminder replaces the pending job.
func reminderJobID(p ReminderJobPayload) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s|%d|%s|%d",
		p.TenantID, p.CalendarID, p.EventUID, p.RecurrenceID, p.StartUTC.Unix(), normEmail(p.RecipientEmail), p.MilestoneMinutes)))
	return "rem-" + hex.EncodeToString(sum[:16])
}

// deliveryKey is the recurrence_id stored in caldev_reminder_deliveries: it includes the
// occurrence start, so a moved meeting is reminded again for its new time.
func deliveryKey(p *ReminderJobPayload) string {
	return p.RecurrenceID + "@" + p.StartUTC.UTC().Format("20060102T150405Z")
}

// ScheduleEventReminders evaluates upcoming milestones (15m, 10m, 5m, 0m)
// for both single events and recurring event series within [NOW(), NOW() + 24 hours].
// Enqueues delayed jobs into Valkey for each recipient (organizer + attendees).
func (svc *Service) ScheduleEventReminders(ctx context.Context, event *models.Event) error {
	if svc.queue == nil || event == nil {
		return nil
	}
	if strings.EqualFold(event.Status, "CANCELLED") {
		return nil
	}

	now := time.Now().UTC()
	oneDayAhead := now.Add(24 * time.Hour)

	var s recurrence.Series
	if svc.store != nil && event.CalendarID != "" && event.UID != "" {
		if rows, err := svc.store.GetResource(ctx, event.TenantID, event.CalendarID, event.UID); err == nil && len(rows) > 0 {
			groups := recurrence.Group(rows)
			if len(groups) > 0 {
				s = groups[0]
			}
		}
	}
	if s.Master == nil {
		s = recurrence.Series{Master: event}
	}

	return svc.scheduleSeriesReminders(ctx, s, now, oneDayAhead)
}

func (svc *Service) scheduleSeriesReminders(ctx context.Context, s recurrence.Series, now, oneDayAhead time.Time) error {
	if s.Master == nil || strings.EqualFold(s.Master.Status, "CANCELLED") {
		return nil
	}
	// Only one copy of a meeting schedules reminders (see isLocalInvitationCopy / ownerScope).
	if isLocalInvitationCopy(s.Master) {
		return nil
	}

	// Expand occurrences: handles both single standalone events and recurring series with overrides!
	occurrences := s.Expand(now, oneDayAhead, 100)
	if len(occurrences) == 0 {
		return nil
	}

	recipients := ownerScope(s.Master, svc.resolveRecipients(ctx, s.Master))
	if len(recipients) == 0 {
		return nil
	}

	for _, occ := range occurrences {
		// Only consider occurrences starting in the window [now, oneDayAhead]
		if occ.Start.Before(now) || occ.Start.After(oneDayAhead) {
			continue
		}
		if strings.EqualFold(occ.Event.Status, "CANCELLED") {
			continue
		}
		// All-day events have no meaningful start time (UTC midnight is not the user's
		// midnight), so the minute countdown pushes are not sent for them.
		if occ.Event.AllDay {
			continue
		}

		recID := occurrenceRecurrenceID(s.Master, occ)

		for _, recipient := range recipients {
			for _, minutes := range DefaultReminderMilestones {
				triggerAt := occ.Start.Add(-time.Duration(minutes) * time.Minute)

				payload := ReminderJobPayload{
					TenantID:         s.Master.TenantID,
					CalendarID:       s.Master.CalendarID,
					EventUID:         s.Master.UID,
					RecurrenceID:     recID,
					StartUTC:         occ.Start.UTC(),
					RecipientEmail:   recipient.Email,
					RecipientUserID:  recipient.UserID,
					MilestoneMinutes: minutes,
					OrganizerEmail:   s.Master.Organizer,
				}

				if triggerAt.After(now) {
					// Future milestone: schedule delayed job
					delay := triggerAt.Sub(now)
					if _, err := svc.queue.EnqueueDelayedWithID(ctx, reminderJobID(payload), jobs.JobCalendarReminder, payload, delay); err != nil {
						log.Printf("scheduling: failed to enqueue delayed reminder for %s: %v", recipient.Email, err)
					}
				} else if now.Sub(triggerAt) <= 2*time.Minute {
					// Milestone due right now (e.g. event booked 4 minutes before start): enqueue immediately
					if _, err := svc.queue.Enqueue(ctx, jobs.JobCalendarReminder, payload); err != nil {
						log.Printf("scheduling: failed to enqueue immediate reminder for %s: %v", recipient.Email, err)
					}
				}
			}
		}
	}

	return nil
}

// SweepReminders is the 1-hour periodic sweeper.
// It queries the database for all events in [NOW(), NOW() + 24 hours] (not older, not more)
// and ensures all milestones for both single events and series are scheduled.
func (svc *Service) SweepReminders(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	oneDayAhead := now.Add(24 * time.Hour)

	events, err := svc.store.QueryUpcomingEventsForReminders(ctx, now, oneDayAhead)
	if err != nil {
		return 0, fmt.Errorf("sweep reminders: %w", err)
	}

	groups := recurrence.Group(events)
	count := 0
	for _, g := range groups {
		if g.Master == nil {
			continue
		}
		if err := svc.scheduleSeriesReminders(ctx, g, now, oneDayAhead); err == nil {
			count++
		}
	}

	log.Printf("scheduling: swept %d upcoming event series for reminder scheduling (window %s to %s)",
		count, now.Format(time.RFC3339), oneDayAhead.Format(time.RFC3339))
	return count, nil
}

// currentOccurrence re-reads the event a reminder was scheduled for and returns the occurrence it
// still refers to. ok is false when the reminder is stale: the event or occurrence is gone,
// cancelled, moved to another time, became all-day, or the recipient no longer takes part.
func (svc *Service) currentOccurrence(ctx context.Context, p *ReminderJobPayload) (occ recurrence.Occurrence, master *models.Event, ok bool, err error) {
	if p.CalendarID == "" {
		// Job queued before reminders carried their calendar: the startup sweep re-schedules it.
		return occ, nil, false, nil
	}
	rows, err := svc.store.GetResource(ctx, p.TenantID, p.CalendarID, p.EventUID)
	if err != nil {
		return occ, nil, false, fmt.Errorf("reload event: %w", err)
	}
	for _, s := range recurrence.Group(rows) {
		if s.Master == nil || strings.EqualFold(s.Master.Status, "CANCELLED") {
			continue
		}
		for _, o := range s.Expand(p.StartUTC, p.StartUTC.Add(time.Minute), 0) {
			if !o.Start.Equal(p.StartUTC) || occurrenceRecurrenceID(s.Master, o) != p.RecurrenceID {
				continue
			}
			if strings.EqualFold(o.Event.Status, "CANCELLED") || o.Event.AllDay {
				return occ, nil, false, nil
			}
			e := o.Event
			if e.Organizer == "" && len(e.Attendees) == 0 {
				e = s.Master // override without participant lines
			}
			if !isActiveParticipant(e, p.RecipientEmail) {
				return occ, nil, false, nil
			}
			return o, s.Master, true, nil
		}
	}
	return occ, nil, false, nil
}

// HandleCalendarReminderJob processes a due reminder job from Valkey.
// It re-checks the event, deduplicates via caldev_reminder_deliveries and publishes the
// notification to Redis (calendar:push). A failed publish returns an error so the job is retried.
func (svc *Service) HandleCalendarReminderJob(ctx context.Context, job *jobs.Job) error {
	payload, err := jobs.ParsePayload[ReminderJobPayload](job)
	if err != nil {
		return err
	}

	// 1. The event may have been moved, cancelled or deleted, or the recipient may have declined
	// since the reminder was scheduled.
	occ, master, ok, err := svc.currentOccurrence(ctx, payload)
	if err != nil {
		return err
	}
	if !ok {
		log.Printf("scheduling: stale reminder skipped for %s (event %s, start %s, %dm)",
			payload.RecipientEmail, payload.EventUID, payload.StartUTC.Format(time.RFC3339), payload.MilestoneMinutes)
		return nil
	}
	summary := occ.Event.Summary
	if summary == "" {
		summary = master.Summary
	}
	if summary == "" {
		summary = "Scheduled Meeting"
	}
	location := occ.Event.Location
	if location == "" {
		location = master.Location
	}

	// 2. Deduplication in DB: claim this milestone for this recipient and occurrence start. The
	// claim is released again if the publish fails.
	key := deliveryKey(payload)
	newlyDelivered, err := svc.store.RecordReminderDelivery(
		ctx,
		payload.TenantID,
		payload.EventUID,
		key,
		payload.RecipientEmail,
		payload.MilestoneMinutes,
		"push",
	)
	if err != nil {
		return fmt.Errorf("record reminder delivery: %w", err)
	}
	if !newlyDelivered {
		// Already delivered (deduplicated against on-demand or periodic sweep)
		return nil
	}

	// 3. Format notification message
	timeStr := "Starting now"
	if payload.MilestoneMinutes > 1 {
		timeStr = fmt.Sprintf("Starts in %d minutes", payload.MilestoneMinutes)
	} else if payload.MilestoneMinutes == 1 {
		timeStr = "Starts in 1 minute"
	}

	body := timeStr
	if location != "" {
		body = fmt.Sprintf("%s • %s", timeStr, location)
	}

	tag := fmt.Sprintf("cal-%s-%dm", payload.EventUID, payload.MilestoneMinutes)
	if payload.RecurrenceID != "" {
		tag = fmt.Sprintf("cal-%s-%s-%dm", payload.EventUID, payload.RecurrenceID, payload.MilestoneMinutes)
	}

	// 4. Publish to Redis calendar:push channel
	if svc.publisher != nil {
		pushEvt := &publisher.PushEvent{
			EventUID:         payload.EventUID,
			RecurrenceID:     payload.RecurrenceID,
			Title:            fmt.Sprintf("Meeting Reminder: %s", summary),
			Location:         location,
			StartUTC:         payload.StartUTC.Format(time.RFC3339),
			RecipientEmail:   payload.RecipientEmail,
			RecipientUserID:  payload.RecipientUserID,
			MilestoneMinutes: payload.MilestoneMinutes,
			Body:             body,
			Tag:              tag,
			URL:              "/calendar",
			Timestamp:        time.Now().UTC().Format(time.RFC3339),
		}

		if err := svc.publisher.PublishPushEvent(ctx, pushEvt); err != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if relErr := svc.store.ReleaseReminderDelivery(rctx, payload.EventUID, key, payload.RecipientEmail, payload.MilestoneMinutes, "push"); relErr != nil {
				log.Printf("scheduling: reminder for %s will not be retried: %v", payload.RecipientEmail, relErr)
			}
			return fmt.Errorf("publish reminder for %s: %w", payload.RecipientEmail, err)
		}
	}

	log.Printf("scheduling: reminder delivered for %s → %q (%dm)",
		payload.RecipientEmail, summary, payload.MilestoneMinutes)
	return nil
}

// HandleSweepRemindersJob is the worker handler for the periodic 1-hour sweep job.
func (svc *Service) HandleSweepRemindersJob(ctx context.Context, job *jobs.Job) error {
	_, err := svc.SweepReminders(ctx)
	return err
}
