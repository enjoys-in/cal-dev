package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/lib/pq"
)

// A calendar object resource (one .ics) is stored as one row per VEVENT/VTODO component:
// the master (recurrence_id = '') plus one row per RECURRENCE-ID override, unique on
// (calendar_id, uid, recurrence_id). The "primary" row is the master, or the earliest override
// when there is no master; ICS storage conventions are documented on ics.ParseICSAll.

const eventColumns = `id, tenant_id, user_id, calendar_id, uid, summary, description, location,
	dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
	status, transparency, classification, organizer, attendees, categories,
	priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties,
	created_at, updated_at`

func scanEventRow(row interface{ Scan(...any) error }) (*models.Event, error) {
	e := &models.Event{}
	err := row.Scan(&e.ID, &e.TenantID, &e.UserID, &e.CalendarID, &e.UID,
		&e.Summary, &e.Description, &e.Location,
		&e.DTStart, &e.DTEnd, &e.Duration, &e.AllDay,
		&e.RecurrenceRule, &e.RecurrenceID, &e.Sequence,
		&e.Status, &e.Transparency, &e.Classification,
		&e.Organizer, &e.Attendees, pq.Array(&e.Categories),
		&e.Priority, &e.URL, &e.Geo, &e.DTStamp,
		&e.ETag, &e.RawICS, &e.SizeBytes, &e.ComponentType, &e.Properties,
		&e.CreatedAt, &e.UpdatedAt)
	return e, err
}

func scanEventRows(rows *sql.Rows) ([]*models.Event, error) {
	defer rows.Close()
	var out []*models.Event
	for rows.Next() {
		e, err := scanEventRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetResource returns every row of one resource, master (or earliest override) first.
// It returns nil when the resource does not exist.
func (s *Store) GetResource(ctx context.Context, tenantID int, calendarID, uid string) ([]*models.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM caldev_events
		 WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3
		 ORDER BY recurrence_id ASC`, tenantID, calendarID, uid)
	if err != nil {
		return nil, err
	}
	return scanEventRows(rows)
}

const upsertEventSQL = `INSERT INTO caldev_events
	(id, tenant_id, user_id, calendar_id, uid, summary, description, location,
	 dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
	 status, transparency, classification, organizer, attendees, categories,
	 priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)
	ON CONFLICT (calendar_id, uid, recurrence_id) DO UPDATE SET
	 summary = EXCLUDED.summary, description = EXCLUDED.description, location = EXCLUDED.location,
	 dtstart = EXCLUDED.dtstart, dtend = EXCLUDED.dtend, duration = EXCLUDED.duration, all_day = EXCLUDED.all_day,
	 recurrence_rule = EXCLUDED.recurrence_rule, sequence = EXCLUDED.sequence,
	 status = EXCLUDED.status, transparency = EXCLUDED.transparency, classification = EXCLUDED.classification,
	 organizer = EXCLUDED.organizer, attendees = EXCLUDED.attendees, categories = EXCLUDED.categories,
	 priority = EXCLUDED.priority, url = EXCLUDED.url, geo = EXCLUDED.geo, dtstamp = EXCLUDED.dtstamp,
	 etag = EXCLUDED.etag, raw_ics = EXCLUDED.raw_ics, size_bytes = EXCLUDED.size_bytes,
	 component_type = EXCLUDED.component_type, properties = EXCLUDED.properties
	WHERE caldev_events.tenant_id = EXCLUDED.tenant_id
	RETURNING id, created_at, updated_at`

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func upsertEvent(ctx context.Context, q queryRower, e *models.Event) error {
	err := q.QueryRowContext(ctx, upsertEventSQL,
		e.ID, e.TenantID, e.UserID, e.CalendarID, e.UID, e.Summary, e.Description, e.Location,
		e.DTStart, e.DTEnd, e.Duration, e.AllDay, e.RecurrenceRule, e.RecurrenceID, e.Sequence,
		e.Status, e.Transparency, e.Classification, e.Organizer, e.Attendees,
		pq.Array(e.Categories), e.Priority, e.URL, e.Geo,
		e.DTStamp, e.ETag, e.RawICS, e.SizeBytes, e.ComponentType, e.Properties).
		Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("event %s/%s belongs to another tenant", e.UID, e.RecurrenceID)
	}
	return err
}

// ReplaceResource atomically makes the stored resource (tenantID, calendarID, uid) consist of
// exactly the given components: rows are inserted or updated in place (keeping their ids), and
// rows that are no longer present, such as a dropped override, are deleted. All events must
// carry that calendar and UID. The slice should come from ics.ParseICSAll or ics.FinalizeResource
// so the storage invariant holds.
func (s *Store) ReplaceResource(ctx context.Context, tenantID int, calendarID, uid string, events []*models.Event) error {
	if len(events) == 0 {
		return fmt.Errorf("ReplaceResource: no components")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	keep := make([]string, 0, len(events))
	for _, e := range events {
		if e.CalendarID != calendarID || e.UID != uid || e.TenantID != tenantID {
			return fmt.Errorf("ReplaceResource: component %q does not belong to %s/%s", e.RecurrenceID, calendarID, uid)
		}
		if err := upsertEvent(ctx, tx, e); err != nil {
			return err
		}
		keep = append(keep, e.RecurrenceID)
	}
	// Delete last so the resource is never observed empty.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM caldev_events
		 WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3 AND NOT (recurrence_id = ANY($4))`,
		tenantID, calendarID, uid, pq.Array(keep)); err != nil {
		return err
	}
	return tx.Commit()
}

// GetResourcesByCalendar returns one row per resource (its primary row), ordered by start.
func (s *Store) GetResourcesByCalendar(ctx context.Context, tenantID int, calendarID string) ([]*models.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM (
		   SELECT DISTINCT ON (uid) `+eventColumns+` FROM caldev_events
		   WHERE tenant_id = $1 AND calendar_id = $2
		   ORDER BY uid, recurrence_id ASC
		 ) primaries ORDER BY dtstart ASC`, tenantID, calendarID)
	if err != nil {
		return nil, err
	}
	return scanEventRows(rows)
}

// PrimaryEventsFor maps matched rows to their resources' primary rows, one per resource, in
// order of first appearance. Use it before building per-resource responses (CalDAV hrefs are
// per UID) from a query that may have matched an override row, a master, or both.
func (s *Store) PrimaryEventsFor(ctx context.Context, tenantID int, events []*models.Event) ([]*models.Event, error) {
	type key struct{ cal, uid string }
	var order []key
	seen := map[key]bool{}
	allPrimary := true
	for _, e := range events {
		k := key{e.CalendarID, e.UID}
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
		if e.RecurrenceID != "" {
			allPrimary = false
		}
	}
	if allPrimary {
		// every matched row is a master, which is its resource's primary row; just dedupe
		out := make([]*models.Event, 0, len(order))
		done := map[key]bool{}
		for _, e := range events {
			k := key{e.CalendarID, e.UID}
			if !done[k] {
				done[k] = true
				out = append(out, e)
			}
		}
		return out, nil
	}

	uids := make([]string, 0, len(order))
	for _, k := range order {
		uids = append(uids, k.uid)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT ON (calendar_id, uid) `+eventColumns+` FROM caldev_events
		 WHERE tenant_id = $1 AND uid = ANY($2)
		 ORDER BY calendar_id, uid, recurrence_id ASC`, tenantID, pq.Array(uids))
	if err != nil {
		return nil, err
	}
	prim, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}
	byKey := make(map[key]*models.Event, len(prim))
	for _, p := range prim {
		byKey[key{p.CalendarID, p.UID}] = p
	}
	out := make([]*models.Event, 0, len(order))
	for _, k := range order {
		if p, ok := byKey[k]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// AttachOverrides returns events plus, for every series master among them, all of that series'
// override rows (even ones whose own start lies outside any time filter), so an override that
// moved an occurrence out of a window can still cancel the original. Rows already present are
// not duplicated.
func (s *Store) AttachOverrides(ctx context.Context, tenantID int, events []*models.Event) ([]*models.Event, error) {
	type key struct{ cal, uid string }
	have := make(map[string]bool, len(events))
	masters := map[key]bool{}
	var uids []string
	for _, e := range events {
		have[e.ID] = true
		if e.RecurrenceID == "" && (e.RecurrenceRule != "" || strings.Contains(string(e.Properties), `"rdates"`)) {
			k := key{e.CalendarID, e.UID}
			if !masters[k] {
				masters[k] = true
				uids = append(uids, e.UID)
			}
		}
	}
	if len(uids) == 0 {
		return events, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM caldev_events
		 WHERE tenant_id = $1 AND uid = ANY($2) AND recurrence_id <> ''`, tenantID, pq.Array(uids))
	if err != nil {
		return nil, err
	}
	extra, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}
	out := events
	for _, o := range extra {
		if masters[key{o.CalendarID, o.UID}] && !have[o.ID] {
			out = append(out, o)
		}
	}
	return out, nil
}

// QueryResources runs the filter and returns, per matching resource, its primary row followed
// (for a series master) by all of that series' overrides. It is the right input for APIs that
// present one object per series (JMAP): an override that matched the filter on its own resolves
// to its master, so the series shows up under the master's id.
func (s *Store) QueryResources(ctx context.Context, filter *models.EventFilter) ([]*models.Event, error) {
	matched, err := s.queryEvents(ctx, filter)
	if err != nil {
		return nil, err
	}
	primaries, err := s.PrimaryEventsFor(ctx, filter.TenantID, matched)
	if err != nil {
		return nil, err
	}
	return s.AttachOverrides(ctx, filter.TenantID, primaries)
}

// QueryEventsWithOverrides is QueryEvents followed by AttachOverrides: the right input for
// recurrence.Group / Series.Expand.
func (s *Store) QueryEventsWithOverrides(ctx context.Context, filter *models.EventFilter) ([]*models.Event, error) {
	events, err := s.queryEvents(ctx, filter)
	if err != nil {
		return nil, err
	}
	return s.AttachOverrides(ctx, filter.TenantID, events)
}

// FindResourceByUID returns every row of the resource with this UID that tenantID holds, master
// (or earliest override) first. When the UID exists in several calendars the lowest calendar id
// wins, so the choice is stable. It returns nil when there is none.
func (s *Store) FindResourceByUID(ctx context.Context, tenantID int, uid string) ([]*models.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM caldev_events
		 WHERE tenant_id = $1 AND uid = $2 AND calendar_id = (
		   SELECT calendar_id FROM caldev_events WHERE tenant_id = $1 AND uid = $2 ORDER BY calendar_id LIMIT 1)
		 ORDER BY recurrence_id ASC`, tenantID, uid)
	if err != nil {
		return nil, err
	}
	return scanEventRows(rows)
}

// RepairFlattenedResources splits resources that were stored before overrides got their own rows.
// Back then a PUT carrying a master plus RECURRENCE-ID overrides was merged into ONE row (the
// last component's fields won) while raw_ics kept the complete text. For every such resource the
// original text is parsed again and the missing component rows are created; the master keeps the
// stored row's id when possible, and ETags are untouched because raw_ics is not regenerated.
// It is idempotent: a resource that already has a row per component is left alone.
// It returns the number of resources repaired.
func (s *Store) RepairFlattenedResources(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventColumns+` FROM caldev_events
		 WHERE raw_ics LIKE '%BEGIN:VEVENT%BEGIN:VEVENT%' OR raw_ics LIKE '%BEGIN:VTODO%BEGIN:VTODO%'
		 ORDER BY calendar_id, uid, recurrence_id`)
	if err != nil {
		return 0, err
	}
	candidates, err := scanEventRows(rows)
	if err != nil {
		return 0, err
	}

	type key struct{ cal, uid string }
	done := map[key]bool{}
	repaired := 0
	for _, old := range candidates {
		k := key{old.CalendarID, old.UID}
		if done[k] {
			continue
		}
		done[k] = true

		parsed, err := ics.ParseICSAll(old.RawICS, old.TenantID, old.UserID, old.CalendarID)
		if err != nil || len(parsed) < 2 {
			continue // not repairable (e.g. mixed UIDs): leave it exactly as stored
		}
		existing, err := s.GetResource(ctx, old.TenantID, old.CalendarID, old.UID)
		if err != nil {
			return repaired, err
		}
		if len(existing) >= len(parsed) {
			continue // already one row per component
		}
		for _, p := range parsed {
			p.UID = old.UID // the stored UID is the resource name; it may differ from the text's UID
			p.CreatedAt = old.CreatedAt
		}
		if parsed[0].RecurrenceID == "" && old.RecurrenceID == "" {
			parsed[0].ID = old.ID
		}
		if err := s.ReplaceResource(ctx, old.TenantID, old.CalendarID, old.UID, parsed); err != nil {
			return repaired, fmt.Errorf("repair %s/%s: %w", old.CalendarID, old.UID, err)
		}
		repaired++
	}
	return repaired, nil
}
