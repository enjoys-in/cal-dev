package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/recurrence"
	"github.com/google/uuid"
	"github.com/lib/pq"
	_ "github.com/lib/pq"
)

// Store provides the data access layer for all CalDAV entities.
type Store struct {
	db *sql.DB
}

// New creates a new Store with a database connection.
func New(cfg *config.DatabaseConfig) (*Store, error) {
	db, err := sql.Open("postgres", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("store: ping db: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for transaction management.
func (s *Store) DB() *sql.DB {
	return s.db
}

// RunMigration executes the SQL migration file content.
func (s *Store) RunMigration(sqlContent string) error {
	_, err := s.db.Exec(sqlContent)
	return err
}

// IsMigrationApplied checks if a migration file has already been recorded.
func (s *Store) IsMigrationApplied(filename string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM caldev_migrations WHERE filename = $1)`,
		filename,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// RecordMigration inserts a migration filename into the tracking table.
func (s *Store) RecordMigration(filename string) error {
	_, err := s.db.Exec(
		`INSERT INTO caldev_migrations (filename) VALUES ($1) ON CONFLICT (filename) DO NOTHING`,
		filename,
	)
	return err
}

// ============================================================
// User Operations — see accounts.go (CalDev's own <schema>.mail_accounts)
// ============================================================

func (s *Store) GetUserByTenantAndEmail(ctx context.Context, tenantID int, email string) (*models.User, error) {
	// Accounts have no separate tenant id: tenantID == user.ID.
	// We just look up by email and verify the ID matches.
	u, err := s.GetUserByEmail(ctx, email)
	if err != nil || u == nil {
		return u, err
	}
	if u.ID != tenantID {
		return nil, nil
	}
	return u, nil
}

// ============================================================
// App Password Operations (caldev_app_passwords)
// ============================================================

func (s *Store) CreateAppPassword(ctx context.Context, ap *models.AppPassword) error {
	query := `INSERT INTO caldev_app_passwords (id, user_id, label, password_hash, scopes)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`
	return s.db.QueryRowContext(ctx, query,
		ap.ID, ap.UserID, ap.Label, ap.PasswordHash, pq.Array(ap.Scopes)).
		Scan(&ap.CreatedAt)
}

func (s *Store) GetAppPasswordsByUser(ctx context.Context, userID int) ([]*models.AppPassword, error) {
	query := `SELECT id, user_id, label, password_hash, scopes, last_used_at, created_at
		FROM caldev_app_passwords WHERE user_id = $1 ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var passwords []*models.AppPassword
	for rows.Next() {
		ap := &models.AppPassword{}
		if err := rows.Scan(&ap.ID, &ap.UserID, &ap.Label, &ap.PasswordHash,
			pq.Array(&ap.Scopes), &ap.LastUsedAt, &ap.CreatedAt); err != nil {
			return nil, err
		}
		passwords = append(passwords, ap)
	}
	return passwords, rows.Err()
}

func (s *Store) TouchAppPassword(ctx context.Context, id string) error {
	query := `UPDATE caldev_app_passwords SET last_used_at = NOW() WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, id)
	return err
}

func (s *Store) DeleteAppPassword(ctx context.Context, id string) error {
	query := `DELETE FROM caldev_app_passwords WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, id)
	return err
}

func (s *Store) DeleteAppPasswordForUser(ctx context.Context, userID int, id string) error {
	query := `DELETE FROM caldev_app_passwords WHERE user_id = $1 AND id = $2`
	_, err := s.db.ExecContext(ctx, query, userID, id)
	return err
}

// AppPasswordInput is used by the API handler for creating app passwords.
type AppPasswordInput struct {
	ID           string
	UserID       int
	Label        string
	PasswordHash string
}

// CreateAppPasswordFromInput creates an app password from API input.
func (s *Store) CreateAppPasswordFromInput(ctx context.Context, ap *AppPasswordInput) error {
	ap.ID = uuid.New().String()
	query := `INSERT INTO caldev_app_passwords (id, user_id, label, password_hash)
		VALUES ($1, $2, $3, $4) RETURNING created_at`
	var createdAt time.Time
	return s.db.QueryRowContext(ctx, query,
		ap.ID, ap.UserID, ap.Label, ap.PasswordHash).
		Scan(&createdAt)
}

// ============================================================
// Calendar Operations
// ============================================================

func (s *Store) CreateCalendar(ctx context.Context, c *models.Calendar) error {
	return createCalendar(ctx, s.db, c)
}

func createCalendar(ctx context.Context, q queryRower, c *models.Calendar) error {
	query := `INSERT INTO caldev_calendars 
		(id, tenant_id, user_id, name, description, color, timezone, sort_order, ctag, sync_token,
		 is_default, is_visible, is_readonly, supported_components, max_resource_size, properties)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING created_at, updated_at`
	return q.QueryRowContext(ctx, query,
		c.ID, c.TenantID, c.UserID, c.Name, c.Description, c.Color, c.Timezone,
		c.SortOrder, c.CTag, c.SyncToken, c.IsDefault, c.IsVisible, c.IsReadOnly,
		pq.Array(c.SupportedComponents), c.MaxResourceSize, c.Properties).
		Scan(&c.CreatedAt, &c.UpdatedAt)
}

func (s *Store) GetCalendar(ctx context.Context, tenantID int, calendarID string) (*models.Calendar, error) {
	c := &models.Calendar{}
	query := `SELECT c.id, c.tenant_id, c.user_id, c.name, c.description, c.color, c.timezone, c.sort_order,
		c.ctag, c.sync_token, c.is_default, c.is_visible, c.is_readonly, c.supported_components,
		c.max_resource_size, c.properties, c.created_at, c.updated_at,
		(s.id IS NOT NULL AND s.status = 'active') AS has_byo_smtp,
		COALESCE(s.from_email, '') AS byo_smtp_from
		FROM caldev_calendars c
		LEFT JOIN caldev_calendar_smtp s ON s.calendar_id = c.id
		WHERE c.tenant_id = $1 AND c.id = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID).
		Scan(&c.ID, &c.TenantID, &c.UserID, &c.Name, &c.Description, &c.Color, &c.Timezone,
			&c.SortOrder, &c.CTag, &c.SyncToken, &c.IsDefault, &c.IsVisible, &c.IsReadOnly,
			pq.Array(&c.SupportedComponents), &c.MaxResourceSize, &c.Properties,
			&c.CreatedAt, &c.UpdatedAt, &c.HasByoSmtp, &c.ByoSmtpFrom)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (s *Store) GetCalendarsByUser(ctx context.Context, tenantID, userID int) ([]*models.Calendar, error) {
	query := `SELECT c.id, c.tenant_id, c.user_id, c.name, c.description, c.color, c.timezone, c.sort_order,
		c.ctag, c.sync_token, c.is_default, c.is_visible, c.is_readonly, c.supported_components,
		c.max_resource_size, c.properties, c.created_at, c.updated_at,
		(s.id IS NOT NULL AND s.status = 'active') AS has_byo_smtp,
		COALESCE(s.from_email, '') AS byo_smtp_from
		FROM caldev_calendars c
		LEFT JOIN caldev_calendar_smtp s ON s.calendar_id = c.id
		WHERE c.tenant_id = $1 AND c.user_id = $2 ORDER BY c.sort_order, c.name`
	rows, err := s.db.QueryContext(ctx, query, tenantID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var calendars []*models.Calendar
	for rows.Next() {
		c := &models.Calendar{}
		if err := rows.Scan(&c.ID, &c.TenantID, &c.UserID, &c.Name, &c.Description, &c.Color,
			&c.Timezone, &c.SortOrder, &c.CTag, &c.SyncToken, &c.IsDefault, &c.IsVisible,
			&c.IsReadOnly, pq.Array(&c.SupportedComponents), &c.MaxResourceSize, &c.Properties,
			&c.CreatedAt, &c.UpdatedAt, &c.HasByoSmtp, &c.ByoSmtpFrom); err != nil {
			return nil, err
		}
		calendars = append(calendars, c)
	}
	return calendars, rows.Err()
}

// EnsureDefaultCalendar creates a default "Personal" calendar if the user has none.
// The check and the insert run in one transaction under a per-user advisory lock, so parallel
// first requests cannot both create one.
func (s *Store) EnsureDefaultCalendar(ctx context.Context, tenantID, userID int) error {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM caldev_calendars WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID).Scan(&count)
	if err != nil || count > 0 {
		return err // fast path: nearly every call
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6002, $1)`, userID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM caldev_calendars WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID).Scan(&count); err != nil || count > 0 {
		return err
	}
	cal := &models.Calendar{
		ID:                  uuid.New().String(),
		TenantID:            tenantID,
		UserID:              userID,
		Name:                "Personal",
		Description:         "Default calendar",
		Color:               "#0078D4",
		Timezone:            "UTC",
		IsDefault:           true,
		IsVisible:           true,
		SupportedComponents: []string{"VEVENT", "VTODO", "VJOURNAL"},
		MaxResourceSize:     10485760,
		Properties:          json.RawMessage(`{}`),
	}
	if err := createCalendar(ctx, tx, cal); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateCalendar(ctx context.Context, c *models.Calendar) error {
	query := `UPDATE caldev_calendars SET
		name = $3, description = $4, color = $5, timezone = $6, sort_order = $7,
		is_default = $8, is_visible = $9, is_readonly = $10, properties = $11
		WHERE tenant_id = $1 AND id = $2`
	_, err := s.db.ExecContext(ctx, query, c.TenantID, c.ID, c.Name, c.Description,
		c.Color, c.Timezone, c.SortOrder, c.IsDefault, c.IsVisible, c.IsReadOnly, c.Properties)
	return err
}

func (s *Store) DeleteCalendar(ctx context.Context, tenantID int, calendarID string) error {
	query := `DELETE FROM caldev_calendars WHERE tenant_id = $1 AND id = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, calendarID)
	return err
}

// ============================================================
// Event Operations
// ============================================================

func (s *Store) CreateEvent(ctx context.Context, e *models.Event) error {
	query := `INSERT INTO caldev_events
		(id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		 dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		 status, transparency, classification, organizer, attendees, categories,
		 priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)
		RETURNING created_at, updated_at`
	return s.db.QueryRowContext(ctx, query,
		e.ID, e.TenantID, e.UserID, e.CalendarID, e.UID, e.Summary, e.Description, e.Location,
		e.DTStart, e.DTEnd, e.Duration, e.AllDay, e.RecurrenceRule, e.RecurrenceID, e.Sequence,
		e.Status, e.Transparency, e.Classification, e.Organizer, e.Attendees,
		pq.Array(e.Categories), e.Priority, e.URL, e.Geo,
		e.DTStamp, e.ETag, e.RawICS, e.SizeBytes, e.ComponentType, e.Properties).
		Scan(&e.CreatedAt, &e.UpdatedAt)
}

func (s *Store) UpdateEvent(ctx context.Context, e *models.Event) error {
	query := `UPDATE caldev_events SET
		summary = $4, description = $5, location = $6,
		dtstart = $7, dtend = $8, duration = $9, all_day = $10,
		recurrence_rule = $11, recurrence_id = $12, sequence = $13,
		status = $14, transparency = $15, classification = $16,
		organizer = $17, attendees = $18, categories = $19,
		priority = $20, url = $21, geo = $22, dtstamp = $23,
		etag = $24, raw_ics = $25, size_bytes = $26,
		component_type = $27, properties = $28
		WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3 AND recurrence_id = $29`
	// recurrence_id is part of the key: a resource has one row per component (master + overrides),
	// so without it this would overwrite every row of the UID with this one row's values.
	_, err := s.db.ExecContext(ctx, query,
		e.TenantID, e.CalendarID, e.UID,
		e.Summary, e.Description, e.Location,
		e.DTStart, e.DTEnd, e.Duration, e.AllDay,
		e.RecurrenceRule, e.RecurrenceID, e.Sequence,
		e.Status, e.Transparency, e.Classification,
		e.Organizer, e.Attendees, pq.Array(e.Categories),
		e.Priority, e.URL, e.Geo, e.DTStamp,
		e.ETag, e.RawICS, e.SizeBytes,
		e.ComponentType, e.Properties, e.RecurrenceID)
	return err
}

func (s *Store) GetEvent(ctx context.Context, tenantID int, calendarID, uid string) (*models.Event, error) {
	e := &models.Event{}
	query := `SELECT id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		status, transparency, classification, organizer, attendees, categories,
		priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties,
		created_at, updated_at
		FROM caldev_events WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3
		ORDER BY recurrence_id ASC LIMIT 1` // the resource's primary row: the master ('' sorts first), else the earliest override
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID, uid).
		Scan(&e.ID, &e.TenantID, &e.UserID, &e.CalendarID, &e.UID,
			&e.Summary, &e.Description, &e.Location,
			&e.DTStart, &e.DTEnd, &e.Duration, &e.AllDay,
			&e.RecurrenceRule, &e.RecurrenceID, &e.Sequence,
			&e.Status, &e.Transparency, &e.Classification,
			&e.Organizer, &e.Attendees, pq.Array(&e.Categories),
			&e.Priority, &e.URL, &e.Geo, &e.DTStamp,
			&e.ETag, &e.RawICS, &e.SizeBytes, &e.ComponentType, &e.Properties,
			&e.CreatedAt, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return e, err
}

func (s *Store) GetEventByID(ctx context.Context, tenantID int, eventID string) (*models.Event, error) {
	e := &models.Event{}
	query := `SELECT id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		status, transparency, classification, organizer, attendees, categories,
		priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties,
		created_at, updated_at
		FROM caldev_events WHERE tenant_id = $1 AND id = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, eventID).
		Scan(&e.ID, &e.TenantID, &e.UserID, &e.CalendarID, &e.UID,
			&e.Summary, &e.Description, &e.Location,
			&e.DTStart, &e.DTEnd, &e.Duration, &e.AllDay,
			&e.RecurrenceRule, &e.RecurrenceID, &e.Sequence,
			&e.Status, &e.Transparency, &e.Classification,
			&e.Organizer, &e.Attendees, pq.Array(&e.Categories),
			&e.Priority, &e.URL, &e.Geo, &e.DTStamp,
			&e.ETag, &e.RawICS, &e.SizeBytes, &e.ComponentType, &e.Properties,
			&e.CreatedAt, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return e, err
}

func (s *Store) DeleteEvent(ctx context.Context, tenantID int, calendarID, uid string) error {
	query := `DELETE FROM caldev_events WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3`
	_, err := s.db.ExecContext(ctx, query, tenantID, calendarID, uid)
	return err
}

func (s *Store) GetEventsByCalendar(ctx context.Context, tenantID int, calendarID string) ([]*models.Event, error) {
	return s.queryEvents(ctx, &models.EventFilter{
		TenantID:   tenantID,
		CalendarID: calendarID,
	})
}

// QueryEvents performs a filtered query for REPORT operations.
func (s *Store) QueryEvents(ctx context.Context, filter *models.EventFilter) ([]*models.Event, error) {
	return s.queryEvents(ctx, filter)
}

func (s *Store) queryEvents(ctx context.Context, filter *models.EventFilter) ([]*models.Event, error) {
	var conditions []string
	var args []interface{}
	argIdx := 1

	if filter.TenantID != 0 {
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d", argIdx))
		args = append(args, filter.TenantID)
		argIdx++
	}
	if filter.CalendarID != "" {
		conditions = append(conditions, fmt.Sprintf("calendar_id = $%d", argIdx))
		args = append(args, filter.CalendarID)
		argIdx++
	}
	if filter.UserID != 0 {
		conditions = append(conditions, fmt.Sprintf("user_id = $%d", argIdx))
		args = append(args, filter.UserID)
		argIdx++
	}
	if filter.ComponentType != "" {
		conditions = append(conditions, fmt.Sprintf("component_type = $%d", argIdx))
		args = append(args, filter.ComponentType)
		argIdx++
	}
	if filter.TimeRangeStart != nil {
		conditions = append(conditions, fmt.Sprintf("(dtend IS NULL OR dtend > $%d OR recurrence_rule != '')", argIdx))
		args = append(args, *filter.TimeRangeStart)
		argIdx++
	}
	if filter.TimeRangeEnd != nil {
		conditions = append(conditions, fmt.Sprintf("dtstart < $%d", argIdx))
		args = append(args, *filter.TimeRangeEnd)
		argIdx++
	}
	if filter.UpdatedSince != nil {
		conditions = append(conditions, fmt.Sprintf("updated_at > $%d", argIdx))
		args = append(args, *filter.UpdatedSince)
		argIdx++
	}
	if len(filter.UIDs) > 0 {
		conditions = append(conditions, fmt.Sprintf("uid = ANY($%d)", argIdx))
		args = append(args, pq.Array(filter.UIDs))
		argIdx++
	}

	query := `SELECT id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		status, transparency, classification, organizer, attendees, categories,
		priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties,
		created_at, updated_at
		FROM caldev_events`

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY dtstart ASC, id ASC"

	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}
	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", filter.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
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

		// Recurring events only match when at least one occurrence overlaps the window.
		if e.RecurrenceRule != "" && (filter.TimeRangeStart != nil || filter.TimeRangeEnd != nil) {
			from := e.DTStart.Add(-24 * time.Hour)
			if filter.TimeRangeStart != nil {
				from = *filter.TimeRangeStart
			}
			to := from.AddDate(100, 0, 0)
			if filter.TimeRangeEnd != nil {
				to = *filter.TimeRangeEnd
			}
			if !recurrence.HasOccurrenceIn(e, from, to) {
				continue
			}
		}

		events = append(events, e)
	}
	return events, rows.Err()
}

func extractUntil(rrule string) time.Time {
	for _, part := range strings.Split(rrule, ";") {
		if strings.HasPrefix(strings.ToUpper(part), "UNTIL=") {
			val := part[6:]
			if t, err := time.Parse("20060102T150405Z", val); err == nil {
				return t
			}
			if t, err := time.Parse("20060102", val); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// EventChanges holds the lists of IDs created, updated, and destroyed since a state.
type EventChanges struct {
	Created   []string
	Updated   []string
	Destroyed []string
}

// GetEventChangesSinceState returns created, updated, and destroyed event IDs since the given state.
func (s *Store) GetEventChangesSinceState(ctx context.Context, tenantID, userID int, sinceState string) (*EventChanges, error) {
	var sinceID int64
	if sinceState != "" && sinceState != "0" && sinceState != "initial" {
		var err error
		sinceID, err = strconv.ParseInt(sinceState, 10, 64)
		if err != nil || sinceID < 0 {
			return nil, fmt.Errorf("invalid state: %s", sinceState)
		}
	}

	// JMAP clients know the master's id (overrides are folded into it as recurrenceOverrides).
	//  - change to a master row            -> that event's own id (recorded in event_id, so even a
	//                                         deleted event is reported under its real id)
	//  - change to an override row         -> the master was modified, if the master still exists
	//  - change to an override of no master -> the override is itself the visible event
	query := `SELECT sc.event_uid, sc.recurrence_id, sc.change_type,
			COALESCE(sc.event_id::text, ''), COALESCE(m.id::text, '')
		FROM caldev_sync_changes sc
		LEFT JOIN caldev_events m ON m.tenant_id = sc.tenant_id AND m.calendar_id = sc.calendar_id
			AND m.uid = sc.event_uid AND m.recurrence_id = ''
		WHERE sc.tenant_id = $1 AND sc.id > $2
		ORDER BY sc.id ASC`
	rows, err := s.db.QueryContext(ctx, query, tenantID, sinceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	createdMap := make(map[string]bool)
	updatedMap := make(map[string]bool)
	destroyedMap := make(map[string]bool)
	// Deleting a series also deletes its override rows. Clients only ever held the master's id
	// (overrides are folded into it), so those row ids must not be reported as destroyed events.
	destroyedOverrideOf := make(map[string]string) // override row id -> uid
	destroyedMasters := make(map[string]bool)      // uid of series whose master row was deleted

	for rows.Next() {
		var uid, recurrenceID, changeType, rowID, masterID string
		if err := rows.Scan(&uid, &recurrenceID, &changeType, &rowID, &masterID); err != nil {
			return nil, err
		}
		targetID := rowID
		if recurrenceID != "" && masterID != "" {
			// an override changed while its master lives on: the master is what clients hold
			targetID = masterID
			changeType = "modified"
		}
		if targetID == "" {
			targetID = masterID // rows recorded before event_id existed
		}
		if targetID == "" {
			targetID = uid
		}
		switch changeType {
		case "created":
			if destroyedMap[targetID] {
				// deleted then re-created under the same id: a move to another calendar
				delete(destroyedMap, targetID)
				updatedMap[targetID] = true
				continue
			}
			createdMap[targetID] = true
		case "modified":
			if !createdMap[targetID] {
				updatedMap[targetID] = true
			}
		case "deleted":
			delete(createdMap, targetID)
			delete(updatedMap, targetID)
			destroyedMap[targetID] = true
			if recurrenceID == "" {
				destroyedMasters[uid] = true
			} else {
				destroyedOverrideOf[targetID] = uid
			}
		}
	}
	for id, uid := range destroyedOverrideOf {
		if destroyedMasters[uid] {
			delete(destroyedMap, id)
		}
	}

	changes := &EventChanges{
		Created:   make([]string, 0, len(createdMap)),
		Updated:   make([]string, 0, len(updatedMap)),
		Destroyed: make([]string, 0, len(destroyedMap)),
	}
	for id := range createdMap {
		changes.Created = append(changes.Created, id)
	}
	for id := range updatedMap {
		changes.Updated = append(changes.Updated, id)
	}
	for id := range destroyedMap {
		changes.Destroyed = append(changes.Destroyed, id)
	}

	return changes, rows.Err()
}

// MoveEvent moves an event from one calendar to another.
func (s *Store) MoveEvent(ctx context.Context, tenantID int, oldCalendarID, uid, newCalendarID string) error {
	query := `UPDATE caldev_events SET calendar_id = $4
		WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3`
	result, err := s.db.ExecContext(ctx, query, tenantID, oldCalendarID, uid, newCalendarID)
	if err != nil {
		return err
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CopyEvent copies a whole resource (master and every override) to another calendar.
// Every copied row gets its own fresh id and keeps the source's content-derived ETag;
// newID and newETag are accepted for API compatibility only.
func (s *Store) CopyEvent(ctx context.Context, tenantID int, srcCalendarID, uid, destCalendarID, newID, newETag string) error {
	query := `INSERT INTO caldev_events
		(id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		 dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		 status, transparency, classification, organizer, attendees, categories,
		 priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties)
		SELECT uuid_generate_v4(), tenant_id, user_id, $4, uid, summary, description, location,
		 dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		 status, transparency, classification, organizer, attendees, categories,
		 priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties
		FROM caldev_events WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3`
	_, err := s.db.ExecContext(ctx, query, tenantID, srcCalendarID, uid, destCalendarID)
	return err
}

// ============================================================
// Sync Token Operations
// ============================================================

// GetSyncChanges returns the calendar's changes logged after syncToken; an empty token means the
// whole log. A non-empty token this calendar never issued returns sql.ErrNoRows (CalDAV answers
// it with DAV:valid-sync-token) instead of silently falling back to the whole log.
func (s *Store) GetSyncChanges(ctx context.Context, tenantID int, calendarID, syncToken string) ([]*models.SyncChange, error) {
	var since sql.NullInt64
	since.Valid = syncToken == ""
	if syncToken != "" {
		// The token is either in the change log, or it is the calendar's current token and nothing
		// was logged yet (a new calendar).
		err := s.db.QueryRowContext(ctx, `SELECT COALESCE(
			(SELECT id FROM caldev_sync_changes WHERE tenant_id = $1 AND calendar_id = $2 AND sync_token = $3 LIMIT 1),
			(SELECT 0 FROM caldev_calendars WHERE tenant_id = $1 AND id = $2 AND sync_token = $3))`,
			tenantID, calendarID, syncToken).Scan(&since)
		if err != nil {
			return nil, err
		}
	}
	if !since.Valid {
		return nil, sql.ErrNoRows
	}
	query := `SELECT id, tenant_id, calendar_id, event_uid, change_type, sync_token, changed_at
		FROM caldev_sync_changes
		WHERE tenant_id = $1 AND calendar_id = $2 AND id > $3
		ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query, tenantID, calendarID, since.Int64)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var changes []*models.SyncChange
	for rows.Next() {
		ch := &models.SyncChange{}
		if err := rows.Scan(&ch.ID, &ch.TenantID, &ch.CalendarID, &ch.EventUID,
			&ch.ChangeType, &ch.SyncToken, &ch.ChangedAt); err != nil {
			return nil, err
		}
		changes = append(changes, ch)
	}
	return changes, rows.Err()
}

// ============================================================
// Lock Operations
// ============================================================

func (s *Store) CreateLock(ctx context.Context, l *models.Lock) error {
	query := `INSERT INTO caldev_locks
		(id, tenant_id, resource_path, lock_token, owner, lock_type, lock_scope, depth, timeout_seconds, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING created_at`
	return s.db.QueryRowContext(ctx, query,
		l.ID, l.TenantID, l.ResourcePath, l.LockToken, l.Owner,
		l.LockType, l.LockScope, l.Depth, l.TimeoutSeconds, l.ExpiresAt).
		Scan(&l.CreatedAt)
}

func (s *Store) GetLock(ctx context.Context, tenantID int, resourcePath string) (*models.Lock, error) {
	l := &models.Lock{}
	query := `SELECT id, tenant_id, resource_path, lock_token, owner, lock_type, lock_scope,
		depth, timeout_seconds, created_at, expires_at
		FROM caldev_locks WHERE tenant_id = $1 AND resource_path = $2 AND expires_at > NOW()`
	err := s.db.QueryRowContext(ctx, query, tenantID, resourcePath).
		Scan(&l.ID, &l.TenantID, &l.ResourcePath, &l.LockToken, &l.Owner,
			&l.LockType, &l.LockScope, &l.Depth, &l.TimeoutSeconds,
			&l.CreatedAt, &l.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

func (s *Store) DeleteLock(ctx context.Context, tenantID int, lockToken string) error {
	query := `DELETE FROM caldev_locks WHERE tenant_id = $1 AND lock_token = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, lockToken)
	return err
}

func (s *Store) CleanupExpiredLocks(ctx context.Context) error {
	query := `DELETE FROM caldev_locks WHERE expires_at < NOW()`
	_, err := s.db.ExecContext(ctx, query)
	return err
}

// ============================================================
// Scheduling Operations
// ============================================================

func (s *Store) CreateSchedulingMessage(ctx context.Context, m *models.SchedulingMessage) error {
	query := `INSERT INTO caldev_scheduling_messages
		(id, tenant_id, sender_id, recipient_email, event_uid, method, status, raw_ics)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at`
	return s.db.QueryRowContext(ctx, query,
		m.ID, m.TenantID, m.SenderID, m.RecipientEmail, m.EventUID,
		m.Method, m.Status, m.RawICS).
		Scan(&m.CreatedAt)
}

func (s *Store) GetPendingSchedulingMessages(ctx context.Context, tenantID int) ([]*models.SchedulingMessage, error) {
	var query string
	var rows *sql.Rows
	var err error

	if tenantID == 0 {
		query = `SELECT id, tenant_id, sender_id, recipient_email, event_uid, method, status, raw_ics, error_message, created_at, sent_at
			FROM caldev_scheduling_messages WHERE status = 'pending' ORDER BY created_at ASC LIMIT 100`
		rows, err = s.db.QueryContext(ctx, query)
	} else {
		query = `SELECT id, tenant_id, sender_id, recipient_email, event_uid, method, status, raw_ics, error_message, created_at, sent_at
			FROM caldev_scheduling_messages WHERE tenant_id = $1 AND status = 'pending' ORDER BY created_at ASC`
		rows, err = s.db.QueryContext(ctx, query, tenantID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*models.SchedulingMessage
	for rows.Next() {
		m := &models.SchedulingMessage{}
		if err := rows.Scan(&m.ID, &m.TenantID, &m.SenderID, &m.RecipientEmail,
			&m.EventUID, &m.Method, &m.Status, &m.RawICS, &m.ErrorMessage,
			&m.CreatedAt, &m.SentAt); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// GetPendingSchedulingMessage returns one message while it is still pending, or nil when it does
// not exist or was already processed. tenantID 0 matches any tenant.
func (s *Store) GetPendingSchedulingMessage(ctx context.Context, tenantID int, id string) (*models.SchedulingMessage, error) {
	m := &models.SchedulingMessage{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, sender_id, recipient_email, event_uid, method, status, raw_ics, error_message, created_at, sent_at
		FROM caldev_scheduling_messages
		WHERE id = $1 AND status = 'pending' AND ($2::int = 0 OR tenant_id = $2)`, id, tenantID).
		Scan(&m.ID, &m.TenantID, &m.SenderID, &m.RecipientEmail,
			&m.EventUID, &m.Method, &m.Status, &m.RawICS, &m.ErrorMessage,
			&m.CreatedAt, &m.SentAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Store) UpdateSchedulingMessageStatus(ctx context.Context, id, status, errorMsg string) error {
	var query string
	if status == "sent" {
		query = `UPDATE caldev_scheduling_messages SET status = $2, sent_at = NOW() WHERE id = $1`
	} else {
		query = `UPDATE caldev_scheduling_messages SET status = $2, error_message = $3 WHERE id = $1`
	}
	_, err := s.db.ExecContext(ctx, query, id, status, errorMsg)
	return err
}

// ============================================================
// JMAP State Operations
// ============================================================

func (s *Store) GetJMAPState(ctx context.Context, tenantID, userID int, entityType string) (string, error) {
	if entityType == "CalendarEvent" {
		var maxID sql.NullInt64
		query := `SELECT MAX(id) FROM caldev_sync_changes WHERE tenant_id = $1`
		err := s.db.QueryRowContext(ctx, query, tenantID).Scan(&maxID)
		if err == nil && maxID.Valid {
			return fmt.Sprintf("%d", maxID.Int64), nil
		}
		return "0", nil
	}
	var state string
	query := `SELECT state FROM caldev_jmap_states
		WHERE tenant_id = $1 AND user_id = $2 AND entity_type = $3`
	err := s.db.QueryRowContext(ctx, query, tenantID, userID, entityType).Scan(&state)
	if err == sql.ErrNoRows {
		return "0", nil // no change recorded yet: stable initial state
	}
	return state, err
}

func (s *Store) GetEventsChangedSince(ctx context.Context, tenantID, userID int, since time.Time) ([]*models.Event, error) {
	filter := &models.EventFilter{
		TenantID: tenantID,
		UserID:   userID,
	}
	if !since.IsZero() {
		filter.UpdatedSince = &since
	}
	return s.queryEvents(ctx, filter)
}

// UpsertEvent creates or updates an event based on UID uniqueness.
func (s *Store) UpsertEvent(ctx context.Context, e *models.Event) error {
	return upsertEvent(ctx, s.db, e) // keyed on (calendar, uid, recurrence_id)
}

// EventExistsByUID checks if an event with the given UID exists in the calendar.
func (s *Store) EventExistsByUID(ctx context.Context, tenantID int, calendarID, uid string) (bool, error) {
	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM caldev_events WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3)`
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID, uid).Scan(&exists)
	return exists, err
}

// GetCalendarCTag returns the current CTag for a calendar.
func (s *Store) GetCalendarCTag(ctx context.Context, tenantID int, calendarID string) (string, error) {
	var ctag string
	query := `SELECT ctag FROM caldev_calendars WHERE tenant_id = $1 AND id = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID).Scan(&ctag)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return ctag, err
}

// GetCalendarSyncToken returns the current sync token for a calendar.
func (s *Store) GetCalendarSyncToken(ctx context.Context, tenantID int, calendarID string) (string, error) {
	var token string
	query := `SELECT sync_token FROM caldev_calendars WHERE tenant_id = $1 AND id = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return token, err
}

// Ensure json is imported (used at compile time for model scanning)
var _ = json.Marshal

// ============================================================
// Calendar Subscription Operations
// ============================================================

// CreateSubscription creates a new external calendar subscription.
func (s *Store) CreateSubscription(ctx context.Context, sub *models.CalendarSubscription) error {
	if sub.ID == "" {
		sub.ID = uuid.New().String()
	}
	query := `INSERT INTO caldev_calendar_subscriptions
		(id, tenant_id, user_id, calendar_id, source_type, source_url, display_name, color,
		 auth_type, auth_credentials, is_enabled, sync_interval)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING created_at, updated_at`
	return s.db.QueryRowContext(ctx, query,
		sub.ID, sub.TenantID, sub.UserID, sub.CalendarID,
		sub.SourceType, sub.SourceURL, sub.DisplayName, sub.Color,
		sub.AuthType, sub.AuthCredentials, sub.IsEnabled, sub.SyncInterval).
		Scan(&sub.CreatedAt, &sub.UpdatedAt)
}

// GetSubscription returns a single subscription by ID.
func (s *Store) GetSubscription(ctx context.Context, tenantID int, subID string) (*models.CalendarSubscription, error) {
	sub := &models.CalendarSubscription{}
	query := `SELECT id, tenant_id, user_id, calendar_id, source_type, source_url,
		display_name, color, auth_type, auth_credentials, is_enabled, sync_interval,
		last_synced_at, last_etag, last_error, error_count, events_count, created_at, updated_at
		FROM caldev_calendar_subscriptions WHERE tenant_id = $1 AND id = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, subID).
		Scan(&sub.ID, &sub.TenantID, &sub.UserID, &sub.CalendarID,
			&sub.SourceType, &sub.SourceURL, &sub.DisplayName, &sub.Color,
			&sub.AuthType, &sub.AuthCredentials, &sub.IsEnabled, &sub.SyncInterval,
			&sub.LastSyncedAt, &sub.LastETag, &sub.LastError, &sub.ErrorCount,
			&sub.EventsCount, &sub.CreatedAt, &sub.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return sub, err
}

// GetSubscriptionsByUser returns all subscriptions for a user.
func (s *Store) GetSubscriptionsByUser(ctx context.Context, tenantID, userID int) ([]*models.CalendarSubscription, error) {
	query := `SELECT id, tenant_id, user_id, calendar_id, source_type, source_url,
		display_name, color, auth_type, '', is_enabled, sync_interval,
		last_synced_at, last_etag, last_error, error_count, events_count, created_at, updated_at
		FROM caldev_calendar_subscriptions WHERE tenant_id = $1 AND user_id = $2
		ORDER BY display_name, created_at`
	rows, err := s.db.QueryContext(ctx, query, tenantID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*models.CalendarSubscription
	for rows.Next() {
		sub := &models.CalendarSubscription{}
		if err := rows.Scan(&sub.ID, &sub.TenantID, &sub.UserID, &sub.CalendarID,
			&sub.SourceType, &sub.SourceURL, &sub.DisplayName, &sub.Color,
			&sub.AuthType, &sub.AuthCredentials, &sub.IsEnabled, &sub.SyncInterval,
			&sub.LastSyncedAt, &sub.LastETag, &sub.LastError, &sub.ErrorCount,
			&sub.EventsCount, &sub.CreatedAt, &sub.UpdatedAt); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// GetDueSubscriptions returns enabled subscriptions that need syncing.
func (s *Store) GetDueSubscriptions(ctx context.Context, limit int) ([]*models.CalendarSubscription, error) {
	query := `SELECT id, tenant_id, user_id, calendar_id, source_type, source_url,
		display_name, color, auth_type, auth_credentials, is_enabled, sync_interval,
		last_synced_at, last_etag, last_error, error_count, events_count, created_at, updated_at
		FROM caldev_calendar_subscriptions
		WHERE is_enabled = TRUE
		  AND (last_synced_at IS NULL OR last_synced_at + (sync_interval * INTERVAL '1 second') < NOW())
		  AND error_count < 10
		ORDER BY last_synced_at ASC NULLS FIRST
		LIMIT $1`
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*models.CalendarSubscription
	for rows.Next() {
		sub := &models.CalendarSubscription{}
		if err := rows.Scan(&sub.ID, &sub.TenantID, &sub.UserID, &sub.CalendarID,
			&sub.SourceType, &sub.SourceURL, &sub.DisplayName, &sub.Color,
			&sub.AuthType, &sub.AuthCredentials, &sub.IsEnabled, &sub.SyncInterval,
			&sub.LastSyncedAt, &sub.LastETag, &sub.LastError, &sub.ErrorCount,
			&sub.EventsCount, &sub.CreatedAt, &sub.UpdatedAt); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// UpdateSubscriptionSyncState updates sync state after a sync attempt.
func (s *Store) UpdateSubscriptionSyncState(ctx context.Context, subID string, etag string, eventsCount int, syncErr string) error {
	if syncErr != "" {
		query := `UPDATE caldev_calendar_subscriptions
			SET last_synced_at = NOW(), last_error = $2, error_count = error_count + 1
			WHERE id = $1`
		_, err := s.db.ExecContext(ctx, query, subID, syncErr)
		return err
	}
	query := `UPDATE caldev_calendar_subscriptions
		SET last_synced_at = NOW(), last_etag = $2, events_count = $3,
		    last_error = '', error_count = 0
		WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, subID, etag, eventsCount)
	return err
}

// UpdateSubscription updates a subscription's editable fields.
func (s *Store) UpdateSubscription(ctx context.Context, sub *models.CalendarSubscription) error {
	query := `UPDATE caldev_calendar_subscriptions SET
		display_name = $3, color = $4, source_url = $5,
		auth_type = $6, auth_credentials = $7,
		is_enabled = $8, sync_interval = $9
		WHERE tenant_id = $1 AND id = $2`
	_, err := s.db.ExecContext(ctx, query,
		sub.TenantID, sub.ID, sub.DisplayName, sub.Color, sub.SourceURL,
		sub.AuthType, sub.AuthCredentials, sub.IsEnabled, sub.SyncInterval)
	return err
}

// DeleteSubscription deletes a subscription (events in the linked calendar remain).
func (s *Store) DeleteSubscription(ctx context.Context, tenantID int, subID string) error {
	query := `DELETE FROM caldev_calendar_subscriptions WHERE tenant_id = $1 AND id = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, subID)
	return err
}

// ReplaceCalendarEvents makes the calendar hold exactly the given events, in one transaction, so
// readers never see it empty or half-imported. Concurrent replaces of one calendar run one after
// the other. An event that cannot be stored is skipped (onSkip is told why); the rest still go in.
// Rows are upserted on (calendar_id, uid, recurrence_id), so existing events keep their ids; rows
// whose ETag is unchanged are not rewritten at all, and rows no longer in the feed are deleted.
func (s *Store) ReplaceCalendarEvents(ctx context.Context, tenantID int, calendarID string, events []*models.Event, onSkip func(*models.Event, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6004, hashtext($1))`, calendarID); err != nil {
		return err
	}

	type key struct{ uid, rid string }
	type storedRow struct{ id, etag string }
	stored := map[key]storedRow{}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, uid, recurrence_id, etag FROM caldev_events WHERE tenant_id = $1 AND calendar_id = $2`,
		tenantID, calendarID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k key
		var r storedRow
		if err := rows.Scan(&r.id, &k.uid, &k.rid, &r.etag); err != nil {
			rows.Close()
			return err
		}
		stored[k] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	uids := make([]string, 0, len(events))
	rids := make([]string, 0, len(events))
	for _, e := range events {
		uids = append(uids, e.UID)
		rids = append(rids, e.RecurrenceID)
		// All rows of a resource share its ETag (a hash of the full resource text), so an equal
		// ETag means the row's content is unchanged: leave it alone (no change-log entry).
		if r, ok := stored[key{e.UID, e.RecurrenceID}]; ok && e.TenantID == tenantID && r.etag == e.ETag {
			e.ID = r.id
			continue
		}
		if _, err := tx.ExecContext(ctx, `SAVEPOINT ev`); err != nil {
			return err
		}
		if err := upsertEvent(ctx, tx, e); err != nil {
			if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT ev`); rerr != nil {
				return rerr
			}
			if onSkip != nil {
				onSkip(e, err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT ev`); err != nil {
			return err
		}
	}
	// Delete last: only rows of this calendar that are no longer in the feed.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM caldev_events
		 WHERE tenant_id = $1 AND calendar_id = $2
		   AND (uid, recurrence_id) NOT IN (SELECT * FROM unnest($3::text[], $4::text[]))`,
		tenantID, calendarID, pq.Array(uids), pq.Array(rids)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteEventsByCalendar deletes all events in a calendar (used during re-sync).
func (s *Store) DeleteEventsByCalendar(ctx context.Context, tenantID int, calendarID string) error {
	query := `DELETE FROM caldev_events WHERE tenant_id = $1 AND calendar_id = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, calendarID)
	return err
}

// ============================================================
// Job History Operations
// ============================================================

func (s *Store) RecordJobResult(ctx context.Context, jr *models.JobResult) error {
	query := `INSERT INTO caldev_job_history (id, job_type, payload, status, error_message, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`
	return s.db.QueryRowContext(ctx, query,
		jr.ID, jr.JobType, jr.Payload, jr.Status, jr.ErrorMessage, jr.DurationMs).
		Scan(&jr.CreatedAt)
}

func (s *Store) CleanupJobHistory(ctx context.Context) error {
	query := `SELECT caldev_cleanup_job_history()`
	_, err := s.db.ExecContext(ctx, query)
	return err
}
