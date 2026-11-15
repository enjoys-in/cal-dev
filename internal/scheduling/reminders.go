package scheduling

import (
	"context"
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
type ReminderJobPayload struct {
	TenantID         int       `json:"tenant_id"`
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

	// 1. Organizer
	orgEmail := normEmail(event.Organizer)
	if orgEmail != "" {
		uid := 0
		name := orgEmail
		if svc.store != nil {
			if u, _ := svc.store.GetUserByEmail(ctx, orgEmail); u != nil {
				uid = u.ID
				if u.Name != "" {
					name = u.Name
				}
			}
		}
		recipients[orgEmail] = ReminderRecipient{UserID: uid, Email: orgEmail, Name: name}
	}

	// 2. Attendees (skip DECLINED)
	var atts []models.Attendee
	if len(event.Attendees) > 0 {
		_ = json.Unmarshal(event.Attendees, &atts)
	}
	for _, a := range atts {
		em := normEmail(a.Email)
		if em == "" || strings.EqualFold(a.Status, "DECLINED") {
			continue
		}
		uid := 0
		name := em
		if svc.store != nil {
			if u, _ := svc.store.GetUserByEmail(ctx, em); u != nil {
				uid = u.ID
				if u.Name != "" {
					name = u.Name
				}
			}
		}
		recipients[em] = ReminderRecipient{UserID: uid, Email: em, Name: name}
	}

	out := make([]ReminderRecipient, 0, len(recipients))
	for _, r := range recipients {
		out = append(out, r)
	}
	return out
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

	// Expand occurrences: handles both single standalone events and recurring series with overrides!
	occurrences := s.Expand(now, oneDayAhead, 100)
	if len(occurrences) == 0 {
		return nil
	}

	recipients := svc.resolveRecipients(ctx, s.Master)
	if len(recipients) == 0 {
		return nil
	}

	for _, occ := range occurrences {
		// Only consider occurrences starting in the window [now, oneDayAhead]
		if occ.Start.Before(now) || occ.Start.After(oneDayAhead) {
			continue
		}

		recID := occ.Event.RecurrenceID
		if recID == "" && s.Master.RecurrenceRule != "" {
			if s.Master.AllDay {
				recID = time.Date(occ.Start.Year(), occ.Start.Month(), occ.Start.Day(), 0, 0, 0, 0, time.UTC).Format("20060102")
			} else {
				recID = occ.Start.UTC().Format("20060102T150405Z")
			}
		}

		summary := occ.Event.Summary
		if summary == "" {
			summary = s.Master.Summary
		}
		if summary == "" {
			summary = "Scheduled Meeting"
		}

		location := occ.Event.Location
		if location == "" {
			location = s.Master.Location
		}

		for _, recipient := range recipients {
			for _, minutes := range DefaultReminderMilestones {
				triggerAt := occ.Start.Add(-time.Duration(minutes) * time.Minute)

				payload := ReminderJobPayload{
					TenantID:         s.Master.TenantID,
					EventUID:         s.Master.UID,
					RecurrenceID:     recID,
					Summary:          summary,
					Location:         location,
					StartUTC:         occ.Start,
					RecipientEmail:   recipient.Email,
					RecipientUserID:  recipient.UserID,
					MilestoneMinutes: minutes,
					OrganizerEmail:   s.Master.Organizer,
				}

				if triggerAt.After(now) {
					// Future milestone: schedule delayed job
					delay := triggerAt.Sub(now)
					if _, err := svc.queue.EnqueueDelayed(ctx, jobs.JobCalendarReminder, payload, delay); err != nil {
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

// HandleCalendarReminderJob processes a due reminder job from Valkey.
// It deduplicates via caldev_reminder_deliveries and publishes the notification to Redis (calendar:push).
func (svc *Service) HandleCalendarReminderJob(ctx context.Context, job *jobs.Job) error {
	payload, err := jobs.ParsePayload[ReminderJobPayload](job)
	if err != nil {
		return err
	}

	// 1. Deduplication check in DB: ensure milestone is only delivered once per recipient
	newlyDelivered, err := svc.store.RecordReminderDelivery(
		ctx,
		payload.TenantID,
		payload.EventUID,
		payload.RecurrenceID,
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

	// 2. Format notification message
	timeStr := "Starting now"
	if payload.MilestoneMinutes > 1 {
		timeStr = fmt.Sprintf("Starts in %d minutes", payload.MilestoneMinutes)
	} else if payload.MilestoneMinutes == 1 {
		timeStr = "Starts in 1 minute"
	}

	body := timeStr
	if payload.Location != "" {
		body = fmt.Sprintf("%s • %s", timeStr, payload.Location)
	}

	tag := fmt.Sprintf("cal-%s-%dm", payload.EventUID, payload.MilestoneMinutes)
	if payload.RecurrenceID != "" {
		tag = fmt.Sprintf("cal-%s-%s-%dm", payload.EventUID, payload.RecurrenceID, payload.MilestoneMinutes)
	}

	// 3. Publish to Redis calendar:push channel
	if svc.publisher != nil {
		pushEvt := &publisher.PushEvent{
			EventUID:         payload.EventUID,
			RecurrenceID:     payload.RecurrenceID,
			Title:            fmt.Sprintf("Meeting Reminder: %s", payload.Summary),
			Location:         payload.Location,
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
			log.Printf("scheduling: push publish error for %s: %v", payload.RecipientEmail, err)
		}
	}

	log.Printf("scheduling: reminder delivered for %s → %q (%dm)",
		payload.RecipientEmail, payload.Summary, payload.MilestoneMinutes)
	return nil
}

// HandleSweepRemindersJob is the worker handler for the periodic 1-hour sweep job.
func (svc *Service) HandleSweepRemindersJob(ctx context.Context, job *jobs.Job) error {
	_, err := svc.SweepReminders(ctx)
	return err
}
