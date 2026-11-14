package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/lib/pq"
)

// ShareInfo describes the other party of a share.
type ShareInfo struct {
	UserID    int       `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SharedCalendar is a calendar another user shared with the caller.
type SharedCalendar struct {
	models.Calendar
	OwnerEmail string `json:"owner_email"`
	Privilege  string `json:"privilege"`
}

// ============================================================
// Availability shares (free/busy only, never event details)
// ============================================================

// CreateAvailabilityShare lets grantee see owner's free/busy. Idempotent.
func (s *Store) CreateAvailabilityShare(ctx context.Context, ownerID, granteeID int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO caldev_availability_shares (owner_user_id, grantee_user_id)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`, ownerID, granteeID)
	return err
}

// DeleteAvailabilityShare revokes a share. Returns whether a row was removed.
func (s *Store) DeleteAvailabilityShare(ctx context.Context, ownerID, granteeID int) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM caldev_availability_shares WHERE owner_user_id = $1 AND grantee_user_id = $2`,
		ownerID, granteeID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// HasAvailabilityShare reports whether owner shared availability with grantee.
func (s *Store) HasAvailabilityShare(ctx context.Context, ownerID, granteeID int) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM caldev_availability_shares WHERE owner_user_id = $1 AND grantee_user_id = $2)`,
		ownerID, granteeID).Scan(&ok)
	return ok, err
}

// ListAvailabilitySharedByOwner lists users the owner shares availability with.
func (s *Store) ListAvailabilitySharedByOwner(ctx context.Context, ownerID int) ([]ShareInfo, error) {
	return s.queryShareInfo(ctx,
		`SELECT sh.grantee_user_id, m.email, COALESCE(m.name, ''), sh.created_at
		 FROM caldev_availability_shares sh
		 JOIN mail_accounts m ON m.id = sh.grantee_user_id
		 WHERE sh.owner_user_id = $1 ORDER BY m.email`, ownerID)
}

// ListAvailabilitySharedWith lists users who share their availability with grantee.
func (s *Store) ListAvailabilitySharedWith(ctx context.Context, granteeID int) ([]ShareInfo, error) {
	return s.queryShareInfo(ctx,
		`SELECT sh.owner_user_id, m.email, COALESCE(m.name, ''), sh.created_at
		 FROM caldev_availability_shares sh
		 JOIN mail_accounts m ON m.id = sh.owner_user_id
		 WHERE sh.grantee_user_id = $1 ORDER BY m.email`, granteeID)
}

// ListOrgMembers returns active users with the same non-empty tenant_name (excluding excludeID).
func (s *Store) ListOrgMembers(ctx context.Context, tenantName string, excludeID, limit int) ([]ShareInfo, error) {
	if tenantName == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	return s.queryShareInfo(ctx,
		`SELECT id, email, COALESCE(name, ''), created_at
		 FROM mail_accounts
		 WHERE lower(tenant_name) = lower($1) AND id <> $2 AND upper(COALESCE(status, 'ACTIVE')) = 'ACTIVE'
		 ORDER BY email LIMIT $3`, tenantName, excludeID, limit)
}

// ============================================================
// Calendar shares (read-only access to one calendar)
// ============================================================

// CreateCalendarShare shares a calendar read-only. Caller must have verified ownership.
func (s *Store) CreateCalendarShare(ctx context.Context, calendarID string, ownerID, granteeID int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO caldev_calendar_shares (calendar_id, owner_user_id, grantee_user_id, privilege)
		 VALUES ($1, $2, $3, 'read') ON CONFLICT DO NOTHING`, calendarID, ownerID, granteeID)
	return err
}

// DeleteCalendarShare revokes a calendar share. Returns whether a row was removed.
func (s *Store) DeleteCalendarShare(ctx context.Context, calendarID string, ownerID, granteeID int) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM caldev_calendar_shares WHERE calendar_id = $1 AND owner_user_id = $2 AND grantee_user_id = $3`,
		calendarID, ownerID, granteeID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListCalendarShares lists who a calendar is shared with.
func (s *Store) ListCalendarShares(ctx context.Context, calendarID string, ownerID int) ([]ShareInfo, error) {
	return s.queryShareInfo(ctx,
		`SELECT sh.grantee_user_id, m.email, COALESCE(m.name, ''), sh.created_at
		 FROM caldev_calendar_shares sh
		 JOIN mail_accounts m ON m.id = sh.grantee_user_id
		 WHERE sh.calendar_id = $1 AND sh.owner_user_id = $2 ORDER BY m.email`, calendarID, ownerID)
}

const sharedCalendarCols = `c.id, c.tenant_id, c.user_id, c.name, c.description, c.color, c.timezone, c.sort_order,
	c.ctag, c.sync_token, c.is_default, c.is_visible, c.is_readonly, c.supported_components,
	c.max_resource_size, c.properties, c.created_at, c.updated_at, m.email, sh.privilege`

func scanSharedCalendar(row interface{ Scan(...any) error }) (*SharedCalendar, error) {
	sc := &SharedCalendar{}
	err := row.Scan(&sc.ID, &sc.TenantID, &sc.UserID, &sc.Name, &sc.Description, &sc.Color, &sc.Timezone,
		&sc.SortOrder, &sc.CTag, &sc.SyncToken, &sc.IsDefault, &sc.IsVisible, &sc.IsReadOnly,
		pq.Array(&sc.SupportedComponents), &sc.MaxResourceSize, &sc.Properties,
		&sc.CreatedAt, &sc.UpdatedAt, &sc.OwnerEmail, &sc.Privilege)
	return sc, err
}

// ListCalendarsSharedWith lists calendars other users shared with granteeID.
func (s *Store) ListCalendarsSharedWith(ctx context.Context, granteeID int) ([]*SharedCalendar, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sharedCalendarCols+`
		 FROM caldev_calendar_shares sh
		 JOIN caldev_calendars c ON c.id = sh.calendar_id
		 JOIN mail_accounts m ON m.id = c.user_id
		 WHERE sh.grantee_user_id = $1 ORDER BY m.email, c.name`, granteeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SharedCalendar
	for rows.Next() {
		sc, err := scanSharedCalendar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// GetCalendarSharedWith returns the calendar if it is shared with granteeID, else nil.
func (s *Store) GetCalendarSharedWith(ctx context.Context, granteeID int, calendarID string) (*SharedCalendar, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sharedCalendarCols+`
		 FROM caldev_calendar_shares sh
		 JOIN caldev_calendars c ON c.id = sh.calendar_id
		 JOIN mail_accounts m ON m.id = c.user_id
		 WHERE sh.grantee_user_id = $1 AND sh.calendar_id = $2`, granteeID, calendarID)
	sc, err := scanSharedCalendar(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return sc, err
}

func (s *Store) queryShareInfo(ctx context.Context, query string, args ...any) ([]ShareInfo, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShareInfo
	for rows.Next() {
		var si ShareInfo
		if err := rows.Scan(&si.UserID, &si.Email, &si.Name, &si.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}
