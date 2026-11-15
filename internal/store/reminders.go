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

// QueryUpcomingEventsForReminders finds events starting within [from, to] (strictly enforced),
// as well as recurring series that have at least one occurrence in that window.
func (s *Store) QueryUpcomingEventsForReminders(ctx context.Context, from, to time.Time) ([]*models.Event, error) {
	query := `SELECT id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		status, transparency, classification, organizer, attendees, categories,
		priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties,
		created_at, updated_at
		FROM caldev_events
		WHERE status != 'CANCELLED'
		  AND ((dtstart >= $1 AND dtstart <= $2) OR (recurrence_rule != ''))
		ORDER BY dtstart ASC`

	rows, err := s.db.QueryContext(ctx, query, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: query upcoming events for reminders: %w", err)
	}
	defer rows.Close()

	var events []*models.Event
	for rows.Next() {
		e := &models.Event{}
		if err := rows.Scan(&e.ID, &e.TenantID, &e.UserID, &e.CalendarID, &e.UID,
			&e.Summary, &e.Description, &e.Location,
			&e.DTStart, &e.DTEnd, &e.Duration, &e.AllDay,
			&e.RecurrenceRule, &e.RecurrenceID, &e.Sequence,
			&e.Status, &e.Transparency, &e.Classification,
			&e.Organizer, &e.Attendees, pq.Array(&e.Categories),
			&e.Priority, &e.URL, &e.Geo, &e.DTStamp,
			&e.ETag, &e.RawICS, &e.SizeBytes, &e.ComponentType, &e.Properties,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}

		// Filter recurring events: only keep if they actually have an occurrence in [from, to)
		if e.RecurrenceRule != "" {
			if !recurrence.HasOccurrenceIn(e, from, to) {
				continue
			}
		}

		events = append(events, e)
	}

	return events, rows.Err()
}
