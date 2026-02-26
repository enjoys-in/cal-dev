package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/models"
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

// ============================================================
// Tenant Operations
// ============================================================

func (s *Store) CreateTenant(ctx context.Context, t *models.Tenant) error {
	query := `INSERT INTO caldev.tenants (id, slug, name, settings)
		VALUES ($1, $2, $3, $4) RETURNING created_at, updated_at`
	return s.db.QueryRowContext(ctx, query, t.ID, t.Slug, t.Name, t.Settings).
		Scan(&t.CreatedAt, &t.UpdatedAt)
}

func (s *Store) GetTenantBySlug(ctx context.Context, slug string) (*models.Tenant, error) {
	t := &models.Tenant{}
	query := `SELECT id, slug, name, settings, created_at, updated_at
		FROM caldev.tenants WHERE slug = $1`
	err := s.db.QueryRowContext(ctx, query, slug).
		Scan(&t.ID, &t.Slug, &t.Name, &t.Settings, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

func (s *Store) GetTenantByID(ctx context.Context, id string) (*models.Tenant, error) {
	t := &models.Tenant{}
	query := `SELECT id, slug, name, settings, created_at, updated_at
		FROM caldev.tenants WHERE id = $1`
	err := s.db.QueryRowContext(ctx, query, id).
		Scan(&t.ID, &t.Slug, &t.Name, &t.Settings, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// ============================================================
// User Operations (reads from public.users — NOT caldev.users)
// ============================================================

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*models.User, error) {
	u := &models.User{}
	query := `SELECT id, tenant_id, email, COALESCE(display_name, '') as display_name, created_at, updated_at
		FROM public.users WHERE email = $1`
	err := s.db.QueryRowContext(ctx, query, email).
		Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// CreateUser inserts a new user into public.users and returns it.
func (s *Store) CreateUser(ctx context.Context, tenantID, email, displayName string) (*models.User, error) {
	u := &models.User{}
	id := uuid.New().String()
	query := `INSERT INTO public.users (id, tenant_id, email, display_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id, tenant_id, email, COALESCE(display_name, '') as display_name, created_at, updated_at`
	err := s.db.QueryRowContext(ctx, query, id, tenantID, email, displayName).
		Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("store: create user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByID(ctx context.Context, id string) (*models.User, error) {
	u := &models.User{}
	query := `SELECT id, tenant_id, email, COALESCE(display_name, '') as display_name, created_at, updated_at
		FROM public.users WHERE id = $1`
	err := s.db.QueryRowContext(ctx, query, id).
		Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (s *Store) GetUserByTenantAndEmail(ctx context.Context, tenantID, email string) (*models.User, error) {
	u := &models.User{}
	query := `SELECT id, tenant_id, email, COALESCE(display_name, '') as display_name, created_at, updated_at
		FROM public.users WHERE tenant_id = $1 AND email = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, email).
		Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// ============================================================
// App Password Operations (caldev.app_passwords)
// ============================================================

func (s *Store) CreateAppPassword(ctx context.Context, ap *models.AppPassword) error {
	query := `INSERT INTO caldev.app_passwords (id, user_id, label, password_hash, scopes)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`
	return s.db.QueryRowContext(ctx, query,
		ap.ID, ap.UserID, ap.Label, ap.PasswordHash, pq.Array(ap.Scopes)).
		Scan(&ap.CreatedAt)
}

func (s *Store) GetAppPasswordsByUser(ctx context.Context, userID string) ([]*models.AppPassword, error) {
	query := `SELECT id, user_id, label, password_hash, scopes, last_used_at, created_at
		FROM caldev.app_passwords WHERE user_id = $1 ORDER BY created_at DESC`
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
	query := `UPDATE caldev.app_passwords SET last_used_at = NOW() WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, id)
	return err
}

func (s *Store) DeleteAppPassword(ctx context.Context, id string) error {
	query := `DELETE FROM caldev.app_passwords WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, id)
	return err
}

// AppPasswordInput is used by the API handler for creating app passwords.
type AppPasswordInput struct {
	ID           string
	UserID       string
	Label        string
	PasswordHash string
}

// CreateAppPasswordFromInput creates an app password from API input.
func (s *Store) CreateAppPasswordFromInput(ctx context.Context, ap *AppPasswordInput) error {
	ap.ID = uuid.New().String()
	query := `INSERT INTO caldev.app_passwords (id, user_id, label, password_hash)
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
	query := `INSERT INTO caldev.calendars 
		(id, tenant_id, user_id, name, description, color, timezone, sort_order, ctag, sync_token,
		 is_default, is_visible, is_readonly, supported_components, max_resource_size, properties)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING created_at, updated_at`
	return s.db.QueryRowContext(ctx, query,
		c.ID, c.TenantID, c.UserID, c.Name, c.Description, c.Color, c.Timezone,
		c.SortOrder, c.CTag, c.SyncToken, c.IsDefault, c.IsVisible, c.IsReadOnly,
		pq.Array(c.SupportedComponents), c.MaxResourceSize, c.Properties).
		Scan(&c.CreatedAt, &c.UpdatedAt)
}

func (s *Store) GetCalendar(ctx context.Context, tenantID, calendarID string) (*models.Calendar, error) {
	c := &models.Calendar{}
	query := `SELECT id, tenant_id, user_id, name, description, color, timezone, sort_order,
		ctag, sync_token, is_default, is_visible, is_readonly, supported_components,
		max_resource_size, properties, created_at, updated_at
		FROM caldev.calendars WHERE tenant_id = $1 AND id = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID).
		Scan(&c.ID, &c.TenantID, &c.UserID, &c.Name, &c.Description, &c.Color, &c.Timezone,
			&c.SortOrder, &c.CTag, &c.SyncToken, &c.IsDefault, &c.IsVisible, &c.IsReadOnly,
			pq.Array(&c.SupportedComponents), &c.MaxResourceSize, &c.Properties,
			&c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (s *Store) GetCalendarsByUser(ctx context.Context, tenantID, userID string) ([]*models.Calendar, error) {
	query := `SELECT id, tenant_id, user_id, name, description, color, timezone, sort_order,
		ctag, sync_token, is_default, is_visible, is_readonly, supported_components,
		max_resource_size, properties, created_at, updated_at
		FROM caldev.calendars WHERE tenant_id = $1 AND user_id = $2 ORDER BY sort_order, name`
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
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		calendars = append(calendars, c)
	}
	return calendars, rows.Err()
}

func (s *Store) UpdateCalendar(ctx context.Context, c *models.Calendar) error {
	query := `UPDATE caldev.calendars SET
		name = $3, description = $4, color = $5, timezone = $6, sort_order = $7,
		is_default = $8, is_visible = $9, is_readonly = $10, properties = $11
		WHERE tenant_id = $1 AND id = $2`
	_, err := s.db.ExecContext(ctx, query, c.TenantID, c.ID, c.Name, c.Description,
		c.Color, c.Timezone, c.SortOrder, c.IsDefault, c.IsVisible, c.IsReadOnly, c.Properties)
	return err
}

func (s *Store) DeleteCalendar(ctx context.Context, tenantID, calendarID string) error {
	query := `DELETE FROM caldev.calendars WHERE tenant_id = $1 AND id = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, calendarID)
	return err
}

// ============================================================
// Event Operations
// ============================================================

func (s *Store) CreateEvent(ctx context.Context, e *models.Event) error {
	query := `INSERT INTO caldev.events
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
	query := `UPDATE caldev.events SET
		summary = $4, description = $5, location = $6,
		dtstart = $7, dtend = $8, duration = $9, all_day = $10,
		recurrence_rule = $11, recurrence_id = $12, sequence = $13,
		status = $14, transparency = $15, classification = $16,
		organizer = $17, attendees = $18, categories = $19,
		priority = $20, url = $21, geo = $22, dtstamp = $23,
		etag = $24, raw_ics = $25, size_bytes = $26,
		component_type = $27, properties = $28
		WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3`
	_, err := s.db.ExecContext(ctx, query,
		e.TenantID, e.CalendarID, e.UID,
		e.Summary, e.Description, e.Location,
		e.DTStart, e.DTEnd, e.Duration, e.AllDay,
		e.RecurrenceRule, e.RecurrenceID, e.Sequence,
		e.Status, e.Transparency, e.Classification,
		e.Organizer, e.Attendees, pq.Array(e.Categories),
		e.Priority, e.URL, e.Geo, e.DTStamp,
		e.ETag, e.RawICS, e.SizeBytes,
		e.ComponentType, e.Properties)
	return err
}

func (s *Store) GetEvent(ctx context.Context, tenantID, calendarID, uid string) (*models.Event, error) {
	e := &models.Event{}
	query := `SELECT id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		status, transparency, classification, organizer, attendees, categories,
		priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties,
		created_at, updated_at
		FROM caldev.events WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3`
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

func (s *Store) GetEventByID(ctx context.Context, tenantID, eventID string) (*models.Event, error) {
	e := &models.Event{}
	query := `SELECT id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		status, transparency, classification, organizer, attendees, categories,
		priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties,
		created_at, updated_at
		FROM caldev.events WHERE tenant_id = $1 AND id = $2`
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

func (s *Store) DeleteEvent(ctx context.Context, tenantID, calendarID, uid string) error {
	query := `DELETE FROM caldev.events WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3`
	_, err := s.db.ExecContext(ctx, query, tenantID, calendarID, uid)
	return err
}

func (s *Store) GetEventsByCalendar(ctx context.Context, tenantID, calendarID string) ([]*models.Event, error) {
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

	if filter.TenantID != "" {
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d", argIdx))
		args = append(args, filter.TenantID)
		argIdx++
	}
	if filter.CalendarID != "" {
		conditions = append(conditions, fmt.Sprintf("calendar_id = $%d", argIdx))
		args = append(args, filter.CalendarID)
		argIdx++
	}
	if filter.UserID != "" {
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
		conditions = append(conditions, fmt.Sprintf("(dtend IS NULL OR dtend > $%d)", argIdx))
		args = append(args, *filter.TimeRangeStart)
		argIdx++
	}
	if filter.TimeRangeEnd != nil {
		conditions = append(conditions, fmt.Sprintf("dtstart < $%d", argIdx))
		args = append(args, *filter.TimeRangeEnd)
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
		FROM caldev.events`

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY dtstart ASC"

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
		events = append(events, e)
	}
	return events, rows.Err()
}

// MoveEvent moves an event from one calendar to another.
func (s *Store) MoveEvent(ctx context.Context, tenantID, oldCalendarID, uid, newCalendarID string) error {
	query := `UPDATE caldev.events SET calendar_id = $4
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

// CopyEvent copies an event to another calendar.
func (s *Store) CopyEvent(ctx context.Context, tenantID, srcCalendarID, uid, destCalendarID, newID, newETag string) error {
	query := `INSERT INTO caldev.events
		(id, tenant_id, user_id, calendar_id, uid, summary, description, location,
		 dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		 status, transparency, classification, organizer, attendees, categories,
		 priority, url, geo, dtstamp, etag, raw_ics, size_bytes, component_type, properties)
		SELECT $4, tenant_id, user_id, $5, uid, summary, description, location,
		 dtstart, dtend, duration, all_day, recurrence_rule, recurrence_id, sequence,
		 status, transparency, classification, organizer, attendees, categories,
		 priority, url, geo, dtstamp, $6, raw_ics, size_bytes, component_type, properties
		FROM caldev.events WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3`
	_, err := s.db.ExecContext(ctx, query, tenantID, srcCalendarID, uid, newID, destCalendarID, newETag)
	return err
}

// ============================================================
// Sync Token Operations
// ============================================================

func (s *Store) GetSyncChanges(ctx context.Context, calendarID, syncToken string) ([]*models.SyncChange, error) {
	query := `SELECT id, tenant_id, calendar_id, event_uid, change_type, sync_token, changed_at
		FROM caldev.sync_changes
		WHERE calendar_id = $1 AND id > COALESCE(
			(SELECT id FROM caldev.sync_changes WHERE calendar_id = $1 AND sync_token = $2 LIMIT 1),
			0
		)
		ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query, calendarID, syncToken)
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
	query := `INSERT INTO caldev.locks
		(id, tenant_id, resource_path, lock_token, owner, lock_type, lock_scope, depth, timeout_seconds, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING created_at`
	return s.db.QueryRowContext(ctx, query,
		l.ID, l.TenantID, l.ResourcePath, l.LockToken, l.Owner,
		l.LockType, l.LockScope, l.Depth, l.TimeoutSeconds, l.ExpiresAt).
		Scan(&l.CreatedAt)
}

func (s *Store) GetLock(ctx context.Context, tenantID, resourcePath string) (*models.Lock, error) {
	l := &models.Lock{}
	query := `SELECT id, tenant_id, resource_path, lock_token, owner, lock_type, lock_scope,
		depth, timeout_seconds, created_at, expires_at
		FROM caldev.locks WHERE tenant_id = $1 AND resource_path = $2 AND expires_at > NOW()`
	err := s.db.QueryRowContext(ctx, query, tenantID, resourcePath).
		Scan(&l.ID, &l.TenantID, &l.ResourcePath, &l.LockToken, &l.Owner,
			&l.LockType, &l.LockScope, &l.Depth, &l.TimeoutSeconds,
			&l.CreatedAt, &l.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

func (s *Store) DeleteLock(ctx context.Context, tenantID, lockToken string) error {
	query := `DELETE FROM caldev.locks WHERE tenant_id = $1 AND lock_token = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, lockToken)
	return err
}

func (s *Store) CleanupExpiredLocks(ctx context.Context) error {
	query := `DELETE FROM caldev.locks WHERE expires_at < NOW()`
	_, err := s.db.ExecContext(ctx, query)
	return err
}

// ============================================================
// Scheduling Operations
// ============================================================

func (s *Store) CreateSchedulingMessage(ctx context.Context, m *models.SchedulingMessage) error {
	query := `INSERT INTO caldev.scheduling_messages
		(id, tenant_id, sender_id, recipient_email, event_uid, method, status, raw_ics)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at`
	return s.db.QueryRowContext(ctx, query,
		m.ID, m.TenantID, m.SenderID, m.RecipientEmail, m.EventUID,
		m.Method, m.Status, m.RawICS).
		Scan(&m.CreatedAt)
}

func (s *Store) GetPendingSchedulingMessages(ctx context.Context, tenantID string) ([]*models.SchedulingMessage, error) {
	var query string
	var rows *sql.Rows
	var err error

	if tenantID == "" {
		query = `SELECT id, tenant_id, sender_id, recipient_email, event_uid, method, status, raw_ics, error_message, created_at, sent_at
			FROM caldev.scheduling_messages WHERE status = 'pending' ORDER BY created_at ASC LIMIT 100`
		rows, err = s.db.QueryContext(ctx, query)
	} else {
		query = `SELECT id, tenant_id, sender_id, recipient_email, event_uid, method, status, raw_ics, error_message, created_at, sent_at
			FROM caldev.scheduling_messages WHERE tenant_id = $1 AND status = 'pending' ORDER BY created_at ASC`
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

func (s *Store) UpdateSchedulingMessageStatus(ctx context.Context, id, status, errorMsg string) error {
	var query string
	if status == "sent" {
		query = `UPDATE caldev.scheduling_messages SET status = $2, sent_at = NOW() WHERE id = $1`
	} else {
		query = `UPDATE caldev.scheduling_messages SET status = $2, error_message = $3 WHERE id = $1`
	}
	_, err := s.db.ExecContext(ctx, query, id, status, errorMsg)
	return err
}

// ============================================================
// JMAP State Operations
// ============================================================

func (s *Store) GetJMAPState(ctx context.Context, tenantID, userID, entityType string) (string, error) {
	var state string
	query := `SELECT state FROM caldev.jmap_states
		WHERE tenant_id = $1 AND user_id = $2 AND entity_type = $3`
	err := s.db.QueryRowContext(ctx, query, tenantID, userID, entityType).Scan(&state)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return state, err
}

func (s *Store) GetEventsChangedSince(ctx context.Context, tenantID, userID string, since time.Time) ([]*models.Event, error) {
	return s.queryEvents(ctx, &models.EventFilter{
		TenantID: tenantID,
		UserID:   userID,
	})
}

// UpsertEvent creates or updates an event based on UID uniqueness.
func (s *Store) UpsertEvent(ctx context.Context, e *models.Event) error {
	existing, err := s.GetEvent(ctx, e.TenantID, e.CalendarID, e.UID)
	if err != nil {
		return err
	}
	if existing != nil {
		e.ID = existing.ID
		return s.UpdateEvent(ctx, e)
	}
	return s.CreateEvent(ctx, e)
}

// EventExistsByUID checks if an event with the given UID exists in the calendar.
func (s *Store) EventExistsByUID(ctx context.Context, tenantID, calendarID, uid string) (bool, error) {
	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM caldev.events WHERE tenant_id = $1 AND calendar_id = $2 AND uid = $3)`
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID, uid).Scan(&exists)
	return exists, err
}

// GetCalendarCTag returns the current CTag for a calendar.
func (s *Store) GetCalendarCTag(ctx context.Context, tenantID, calendarID string) (string, error) {
	var ctag string
	query := `SELECT ctag FROM caldev.calendars WHERE tenant_id = $1 AND id = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID).Scan(&ctag)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return ctag, err
}

// GetCalendarSyncToken returns the current sync token for a calendar.
func (s *Store) GetCalendarSyncToken(ctx context.Context, tenantID, calendarID string) (string, error) {
	var token string
	query := `SELECT sync_token FROM caldev.calendars WHERE tenant_id = $1 AND id = $2`
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
	query := `INSERT INTO caldev.calendar_subscriptions
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
func (s *Store) GetSubscription(ctx context.Context, tenantID, subID string) (*models.CalendarSubscription, error) {
	sub := &models.CalendarSubscription{}
	query := `SELECT id, tenant_id, user_id, calendar_id, source_type, source_url,
		display_name, color, auth_type, auth_credentials, is_enabled, sync_interval,
		last_synced_at, last_etag, last_error, error_count, events_count, created_at, updated_at
		FROM caldev.calendar_subscriptions WHERE tenant_id = $1 AND id = $2`
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
func (s *Store) GetSubscriptionsByUser(ctx context.Context, tenantID, userID string) ([]*models.CalendarSubscription, error) {
	query := `SELECT id, tenant_id, user_id, calendar_id, source_type, source_url,
		display_name, color, auth_type, '', is_enabled, sync_interval,
		last_synced_at, last_etag, last_error, error_count, events_count, created_at, updated_at
		FROM caldev.calendar_subscriptions WHERE tenant_id = $1 AND user_id = $2
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
		FROM caldev.calendar_subscriptions
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
		query := `UPDATE caldev.calendar_subscriptions
			SET last_synced_at = NOW(), last_error = $2, error_count = error_count + 1
			WHERE id = $1`
		_, err := s.db.ExecContext(ctx, query, subID, syncErr)
		return err
	}
	query := `UPDATE caldev.calendar_subscriptions
		SET last_synced_at = NOW(), last_etag = $2, events_count = $3,
		    last_error = '', error_count = 0
		WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, subID, etag, eventsCount)
	return err
}

// UpdateSubscription updates a subscription's editable fields.
func (s *Store) UpdateSubscription(ctx context.Context, sub *models.CalendarSubscription) error {
	query := `UPDATE caldev.calendar_subscriptions SET
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
func (s *Store) DeleteSubscription(ctx context.Context, tenantID, subID string) error {
	query := `DELETE FROM caldev.calendar_subscriptions WHERE tenant_id = $1 AND id = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, subID)
	return err
}

// DeleteEventsByCalendar deletes all events in a calendar (used during re-sync).
func (s *Store) DeleteEventsByCalendar(ctx context.Context, tenantID, calendarID string) error {
	query := `DELETE FROM caldev.events WHERE tenant_id = $1 AND calendar_id = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, calendarID)
	return err
}

// ============================================================
// Job History Operations
// ============================================================

func (s *Store) RecordJobResult(ctx context.Context, jr *models.JobResult) error {
	query := `INSERT INTO caldev.job_history (id, job_type, payload, status, error_message, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`
	return s.db.QueryRowContext(ctx, query,
		jr.ID, jr.JobType, jr.Payload, jr.Status, jr.ErrorMessage, jr.DurationMs).
		Scan(&jr.CreatedAt)
}

func (s *Store) CleanupJobHistory(ctx context.Context) error {
	query := `SELECT caldev.cleanup_job_history()`
	_, err := s.db.ExecContext(ctx, query)
	return err
}
