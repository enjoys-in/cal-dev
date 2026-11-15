package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/recurrence"
	"github.com/lib/pq"
)

// RecordReminderDelivery logs that a milestone reminder (e.g. 15m, 10m, 5m, 0m) was sent to a recipient.
// Returns true if the delivery was newly recorded, or false if it was already previously delivered (deduplicated).
func (s *Store) RecordReminderDelivery(ctx context.Context, tenantID int, eventUID, recurrenceID, recipientEmail string, milestoneMinutes int, channel string) (bool, error) {
	if channel == "" {
		channel = "push"
	}
	query := `INSERT INTO caldev_reminder_deliveries 
		(tenant_id, event_uid, recurrence_id, recipient_email, milestone_minutes, channel)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_uid, recurrence_id, recipient_email, milestone_minutes, channel) DO NOTHING
		RETURNING id`

	var id int64
	err := s.db.QueryRowContext(ctx, query, tenantID, eventUID, recurrenceID, recipientEmail, milestoneMinutes, channel).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil // Already delivered
	}
	if err != nil {
		return false, fmt.Errorf("store: record reminder delivery: %w", err)
	}
	return true, nil
}

// reminderColumns is eventColumns without raw_ics, which the reminder sweep never needs.
const reminderColumns = `id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		status, transparency, classification, organizer, attendees, categories,
		priority, url, geo, dtstamp, etag, '' AS raw_ics, size_bytes, component_type, properties,
		created_at, updated_at`

// QueryUpcomingEventsForReminders finds events starting within [from, to] (strictly enforced),
// as well as recurring series that have at least one occurrence in that window. For every such
// series all override rows are returned too, whatever their status or time, so expansion can drop
// occurrences that were cancelled or moved away.
func (s *Store) QueryUpcomingEventsForReminders(ctx context.Context, from, to time.Time) ([]*models.Event, error) {
	// Recurring masters: skip series that start after the window or whose UNTIL date has passed
	// (UNTIL's date part is compared one day early so a local-time UNTIL is never cut short).
	query := `SELECT ` + reminderColumns + `
		FROM caldev_events
		WHERE status != 'CANCELLED'
		  AND ((dtstart >= $1 AND dtstart <= $2)
		    OR (recurrence_rule != '' AND recurrence_id = '' AND dtstart <= $2
		        AND COALESCE(substring(recurrence_rule from 'UNTIL=([0-9]{8})'), '99999999') >= $3))
		ORDER BY dtstart ASC`

	rows, err := s.db.QueryContext(ctx, query, from, to, from.UTC().AddDate(0, 0, -1).Format("20060102"))
	if err != nil {
		return nil, fmt.Errorf("store: query upcoming events for reminders: %w", err)
	}
	all, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	var events []*models.Event
	var masterIDs []string
	seen := make(map[string]bool, len(all))
	for _, e := range all {
		// Filter recurring events: only keep if they actually have an occurrence in [from, to)
		if e.RecurrenceRule != "" {
			if !recurrence.HasOccurrenceIn(e, from, to) {
				continue
			}
			if e.RecurrenceID == "" {
				masterIDs = append(masterIDs, e.ID)
			}
		}
		seen[e.ID] = true
		events = append(events, e)
	}
	if len(masterIDs) == 0 {
		return events, nil
	}

	rows, err = s.db.QueryContext(ctx, `SELECT `+reminderColumns+`
		FROM caldev_events
		WHERE recurrence_id != '' AND (calendar_id, uid) IN (
		  SELECT calendar_id, uid FROM caldev_events WHERE id = ANY($1::uuid[]))`, pq.Array(masterIDs))
	if err != nil {
		return nil, fmt.Errorf("store: query overrides for reminders: %w", err)
	}
	overrides, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}
	for _, o := range overrides {
		if !seen[o.ID] {
			seen[o.ID] = true
			events = append(events, o)
		}
	}
	return events, nil
}

// ReleaseReminderDelivery deletes a delivery row recorded by RecordReminderDelivery, so a reminder
// whose publish failed can be delivered by the job's retry.
func (s *Store) ReleaseReminderDelivery(ctx context.Context, eventUID, recurrenceID, recipientEmail string, milestoneMinutes int, channel string) error {
	if channel == "" {
		channel = "push"
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM caldev_reminder_deliveries
		WHERE event_uid = $1 AND recurrence_id = $2 AND recipient_email = $3 AND milestone_minutes = $4 AND channel = $5`,
		eventUID, recurrenceID, recipientEmail, milestoneMinutes, channel)
	if err != nil {
		return fmt.Errorf("store: release reminder delivery: %w", err)
	}
	return nil
}
